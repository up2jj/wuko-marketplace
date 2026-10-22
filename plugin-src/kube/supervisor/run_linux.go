//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/up2jj/wuko-marketplace/plugin-src/kube/execution"
)

func ChildEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, execution.Prefix) {
			out = append(out, e)
		}
	}
	return out
}

// Run requires PID 1 in the private container namespace. Exiting PID 1 is the
// final kernel-enforced fence for descendants that create their own sessions.
func Run(ctx context.Context, c execution.Control, s *execution.Service) error {
	if os.Getpid() != 1 {
		return fmt.Errorf("supervisor must be PID 1 in a private PID namespace")
	}
	max, ttl, _, grace, err := c.Options.Durations()
	if err != nil {
		return err
	}
	if c.Version != 1 || c.JobUID == "" || c.Owner == "" {
		return fmt.Errorf("invalid immutable control file")
	}
	watchdogCtx, stopWatchdog := context.WithCancel(ctx)
	defer stopWatchdog()
	ready := make(chan struct{})
	stopping := make(chan error, 1)
	go func() {
		stopping <- (Watchdog{Owner: c.Owner, TTL: ttl, Interval: time.Second, MaxDuration: max, Poll: PollCluster(s, c)}).Watch(watchdogCtx, ready)
	}()
	select {
	case <-ready:
	case err := <-stopping:
		return err
	}
	podUID, podName := os.Getenv(execution.Prefix+"POD_UID"), os.Getenv(execution.Prefix+"POD_NAME")
	if podUID == "" || podName == "" {
		return fmt.Errorf("missing authoritative Pod identity")
	}
	claim, cancelClaim := context.WithTimeout(watchdogCtx, 2*execution.RequestTimeout)
	err = s.Claim(claim, c, podUID, podName)
	cancelClaim()
	if err != nil {
		return err
	}
	// Check the fence again after the claim, before starting any workflow code.
	a, err := PollCluster(s, c)(watchdogCtx)
	if err != nil || a.Revoked || a.Owner != c.Owner {
		return fmt.Errorf("authorization lost after claim")
	}
	select {
	case err := <-stopping:
		return err
	default:
	}
	runtimeDir := "/var/run/wuko/runtime"
	private, err := os.MkdirTemp(runtimeDir, "execution-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(private)
	workflow := c.Options.Workflow
	if c.Options.WorkflowInline != "" {
		workflow = filepath.Join(private, "wuko.yaml")
		if err = os.WriteFile(workflow, []byte(c.Options.WorkflowInline), 0600); err != nil {
			return err
		}
	}
	vars := filepath.Join(private, "vars.json")
	if err = os.WriteFile(vars, []byte(execution.Encode(c.Options.Vars)), 0600); err != nil {
		return err
	}
	report := filepath.Join(private, "report.json")
	args := []string{"--env-loader", "none", "run", "--once", "--reporter", "plain", "--file", workflow, "--report-json", report, "--var-file", vars}
	if c.Options.Target != "" {
		args = append(args, c.Options.Target)
	}
	command := exec.Command("/usr/local/bin/wuko", args...)
	command.Dir = c.Options.Workdir
	command.Env = ChildEnv(os.Environ())
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = command.Start(); err != nil {
		return err
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	exitCode := 0
	reason := "completed"
	var waitErr error
	select {
	case waitErr = <-finished:
		if waitErr != nil {
			exitCode = 1
			reason = "workload failed"
			if e, ok := waitErr.(*exec.ExitError); ok {
				exitCode = e.ExitCode()
			}
		}
	case stopErr := <-stopping:
		exitCode = 1
		reason = stopErr.Error()
		// Setpgid made the child a group leader; signal the whole group.
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(grace)
		select {
		case <-finished:
		case <-timer.C:
		}
		timer.Stop()
	}
	// Includes detached process groups and reparented grandchildren. Non-root
	// workers and descendants share a UID; caps and privilege escalation are off.
	_ = syscall.Kill(-1, syscall.SIGKILL)
	reapUntil := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(reapUntil) {
		var status syscall.WaitStatus
		pid, e := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if e != nil {
			break
		}
		if pid == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	// Reporting occurs only after termination and gets a separate bounded budget.
	state, data := ReadReport(report)
	publish, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = s.UpdateRecord(publish, c.Options.ExecutionID, func(r *execution.Record) error {
		if r.PodUID != podUID {
			return fmt.Errorf("Pod claim changed")
		}
		r.State = "finished"
		r.ExitCode = exitCode
		r.Reason = reason
		r.ReportState = state
		r.Report = data
		return nil
	})
	if err != nil {
		return fmt.Errorf("report unpublished: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("%s", reason)
	}
	return nil
}

func LoadControl() (execution.Control, error) {
	var c execution.Control
	data, err := os.ReadFile(execution.ControlPath)
	if err != nil {
		return c, err
	}
	// UseNumber keeps integer vars exact when they are re-encoded to vars.json.
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	err = d.Decode(&c)
	return c, err
}
