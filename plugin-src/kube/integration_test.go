package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/up2jj/wuko-marketplace/plugin-src/kube/execution"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Run only against the disposable cluster created by integration/run.sh.
func TestKubernetesIntegration(t *testing.T) {
	if os.Getenv("WUKO_KUBE_INTEGRATION") != "1" {
		t.Skip("requires integration/run.sh and its disposable kind cluster")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	cfg := os.Getenv("KUBECONFIG")
	if cfg == "" {
		t.Fatal("explicit disposable KUBECONFIG required")
	}
	wuko := os.Getenv("WUKO_BINARY")
	if wuko == "" {
		wuko = "wuko"
	}
	api, err := execution.Client(execution.Options{Kubeconfig: cfg})
	if err != nil {
		t.Fatal(err)
	}
	s := &execution.Service{API: api, Namespace: "wuko-jobs"}
	runCommand(t, "kubectl", "--kubeconfig", cfg, "apply", "-f", "deploy/namespace.yaml")
	_, err = api.CoreV1().Pods(s.Namespace).Create(ctx, &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "sink", Labels: map[string]string{"app": "sink"}}, Spec: core.PodSpec{Containers: []core.Container{{Name: "sink", Image: "wuko-kube-sink:dev", ImagePullPolicy: core.PullNever}}}}, meta.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	_, err = api.CoreV1().Services(s.Namespace).Create(ctx, &core.Service{ObjectMeta: meta.ObjectMeta{Name: "sink"}, Spec: core.ServiceSpec{Selector: map[string]string{"app": "sink"}, Ports: []core.ServicePort{{Port: 8080}}}}, meta.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	counts := func() (map[string]int, error) {
		b, e := api.CoreV1().RESTClient().Get().Namespace(s.Namespace).Resource("services").Name("sink:8080").SubResource("proxy").Suffix("counts").Do(ctx).Raw()
		if e != nil {
			return nil, e
		}
		var v map[string]int
		e = json.Unmarshal(b, &v)
		return v, e
	}
	eventually(t, ctx, 60*time.Second, func() bool { _, e := counts(); return e == nil }, "counter readiness")
	pluginBinary := filepath.Join(t.TempDir(), "wuko-plugin-kube")
	runCommand(t, "go", "build", "-o", pluginBinary, ".")
	o := func(id string) execution.Options {
		v := execution.Options{Kubeconfig: cfg, ExecutionID: id, Image: "wuko-worker:dev", WorkflowInline: "version: 1\nname: traffic\nsteps:\n  - id: send\n    type: shell\n    with:\n      script: |\n        setsid sh -c 'trap \"\" TERM; while true; do wget -q -O /dev/null http://sink:8080/tick/" + id + "; sleep 0.2; done' &\n        wait\n", Concurrency: &execution.Concurrency{Group: "integration-exclusive", OnBusy: "error"}, MaxDuration: "2m", AuthorizationTTL: "6s", RenewInterval: "1s", ShutdownGrace: "2s"}
		if e := v.Validate("run"); e != nil {
			t.Fatal(e)
		}
		return v
	}
	for _, mode := range []string{"plugin-kill", "stdin-eof", "wuko-kill", "worker-api-loss"} {
		t.Run(mode, func(t *testing.T) {
			id := "it-" + mode + "-" + execution.RandomID()[3:9]
			v := o(id)
			var stop func()
			if mode == "wuko-kill" {
				dir := t.TempDir()
				manifest, _ := filepath.Abs("plugin.json")
				c := exec.CommandContext(ctx, wuko, "plugin", "install", manifest)
				c.Dir = dir
				if b, e := c.CombinedOutput(); e != nil {
					t.Fatalf("install: %s %v", b, e)
				}
				doc := map[string]any{"version": 1, "name": "owner", "steps": []any{map[string]any{"id": "work", "type": "kube.run", "with": v}}}
				file := filepath.Join(dir, "wuko.yaml")
				lockProbe := exec.CommandContext(ctx, wuko, "plugin", "lock", "--help")
				lockProbe.Dir = dir
				probeOutput, probeErr := lockProbe.CombinedOutput()
				supportsLock := probeErr == nil && strings.Contains(string(probeOutput), "wuko plugin lock")
				if supportsLock {
					doc["plugins"] = map[string]any{"kube": map[string]any{"source": manifest}}
				}
				if e := os.WriteFile(file, []byte(execution.Encode(doc)), 0600); e != nil {
					t.Fatal(e)
				}
				if supportsLock {
					lock := exec.CommandContext(ctx, wuko, "plugin", "lock", file)
					lock.Dir = dir
					if b, e := lock.CombinedOutput(); e != nil {
						t.Fatalf("lock: %s %v", b, e)
					}
				}
				c = exec.CommandContext(ctx, wuko, "run", "--file", file, "--reporter", "plain")
				c.Dir = dir
				c.Stdout = os.Stdout
				c.Stderr = os.Stderr
				if e := c.Start(); e != nil {
					t.Fatal(e)
				}
				stop = func() { _ = c.Process.Kill(); _ = c.Wait() }
			} else {
				c := exec.CommandContext(ctx, pluginBinary)
				in, e := c.StdinPipe()
				if e != nil {
					t.Fatal(e)
				}
				out, e := c.StdoutPipe()
				if e != nil {
					t.Fatal(e)
				}
				c.Stderr = os.Stderr
				if e = c.Start(); e != nil {
					t.Fatal(e)
				}
				encoder := json.NewEncoder(in)
				if e = encoder.Encode(map[string]any{"id": "init", "method": "initialize", "params": map[string]any{"protocol": "wuko.plugin/v3"}}); e != nil {
					t.Fatal(e)
				}
				reader := bufio.NewReader(out)
				for {
					b, e := reader.ReadBytes('\n')
					if e != nil {
						t.Fatal(e)
					}
					var response map[string]any
					_ = json.Unmarshal(b, &response)
					if response["id"] == "init" {
						break
					}
				}
				go func() { _, _ = io.Copy(io.Discard, reader) }()
				if e = encoder.Encode(map[string]any{"id": "run", "method": "step.run", "params": map[string]any{"type": "kube.run", "with": v}}); e != nil {
					t.Fatal(e)
				}
				stop = func() {
					if mode == "stdin-eof" {
						_ = in.Close()
					} else {
						_ = c.Process.Kill()
					}
					_ = c.Wait()
				}
			}
			defer func() {
				if stop != nil {
					stop()
				}
			}()
			eventually(t, ctx, 60*time.Second, func() bool { m, e := counts(); return e == nil && m[id] >= 3 }, "traffic start")
			blocked := o(id + "-busy")
			if _, e := s.Run(ctx, blocked, io.Discard); e == nil || !strings.Contains(e.Error(), id) {
				t.Fatalf("group did not fail busy with holder ID: %v", e)
			}
			started := time.Now()
			if mode == "worker-api-loss" {
				runCommand(t, "kubectl", "--kubeconfig", cfg, "-n", s.Namespace, "delete", "rolebinding", "wuko-worker")
			} else {
				stop()
				stop = nil
			}
			eventually(t, ctx, 15*time.Second, func() bool {
				pods, e := api.CoreV1().Pods(s.Namespace).List(ctx, meta.ListOptions{LabelSelector: execution.IDLabel + "=" + id})
				if e != nil || len(pods.Items) == 0 {
					return false
				}
				for _, p := range pods.Items {
					if p.Status.Phase != core.PodSucceeded && p.Status.Phase != core.PodFailed {
						return false
					}
				}
				return true
			}, "worker termination within TTL + grace + API tolerance")
			if time.Since(started) > 15*time.Second {
				t.Fatal("termination exceeded bound")
			}
			before, e := counts()
			if e != nil {
				t.Fatal(e)
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-timer.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			after, e := counts()
			if e != nil || before[id] != after[id] {
				t.Fatal("detached descendant continued sending traffic")
			}
			if mode == "worker-api-loss" {
				runCommand(t, "kubectl", "--kubeconfig", cfg, "apply", "-f", "deploy/namespace.yaml")
				stop()
				stop = nil
			}
			st, e := s.Cancel(ctx, id)
			if e != nil || !st.TerminationConfirmed {
				t.Fatalf("cancel confirmation: %v %+v", e, st)
			}
			if e = s.Purge(ctx, id); e != nil {
				t.Fatal(e)
			}
		})
	}
	t.Run("success-env-imports-report-replay", func(t *testing.T) {
		_, e := api.CoreV1().ConfigMaps(s.Namespace).Create(ctx, &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: "import-a"}, Data: map[string]string{"ORDER": "first", "IMPORTED_ORDER": "first", "WUKO_KUBE_POD_UID": "spoof", "KUBECONFIG": "/bad", "HTTP_PROXY": "http://127.0.0.1:1"}}, meta.CreateOptions{})
		if e != nil && !apierrors.IsAlreadyExists(e) {
			t.Fatal(e)
		}
		_, e = api.CoreV1().Secrets(s.Namespace).Create(ctx, &core.Secret{ObjectMeta: meta.ObjectMeta{Name: "import-b"}, StringData: map[string]string{"ORDER": "last", "IMPORTED_ORDER": "last", "SECRET_VALUE": "cluster-only"}}, meta.CreateOptions{})
		if e != nil && !apierrors.IsAlreadyExists(e) {
			t.Fatal(e)
		}
		v := o("it-success-" + execution.RandomID()[3:9])
		v.WorkflowInline = "version: 1\nname: success\noutputs:\n  answer: {type: string, value: '\"done\"'}\nsteps:\n  - id: verify\n    type: shell\n    with:\n      script: |\n        test \"$ORDER\" = explicit\n        test \"$IMPORTED_ORDER\" = last\n        test \"$SECRET_VALUE\" = cluster-only\n        test \"$KUBECONFIG\" = /bad\n        test -z \"${WUKO_KUBE_POD_UID:-}\"\n        test -n \"$EXECUTION_ID\"\n"
		v.EnvFrom = []core.EnvFromSource{{ConfigMapRef: &core.ConfigMapEnvSource{LocalObjectReference: core.LocalObjectReference{Name: "import-a"}}}, {SecretRef: &core.SecretEnvSource{LocalObjectReference: core.LocalObjectReference{Name: "import-b"}}}}
		v.Env = []core.EnvVar{{Name: "ORDER", Value: "explicit"}, {Name: "EXECUTION_ID", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.labels['wuko.dev/execution-id']"}}}}
		if e = v.Validate("run"); e != nil {
			t.Fatal(e)
		}
		st, e := s.Run(ctx, v, os.Stdout)
		if e != nil {
			t.Fatalf("success: %v %+v", e, st)
		}
		if st.Record.ReportState != "available" {
			t.Fatal(st)
		}
		replayed, e := s.Run(ctx, v, io.Discard)
		if e != nil || replayed.Record.PodUID != st.Record.PodUID {
			t.Fatal("terminal execution reran", e)
		}
		if e = s.Purge(ctx, v.ExecutionID); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("k6-otlp", func(t *testing.T) {
		image := os.Getenv("WUKO_KUBE_K6_IMAGE")
		if image == "" {
			t.Skip("set WUKO_KUBE_K6_IMAGE to the loaded k6 example image")
		}
		v := o("it-k6-" + execution.RandomID()[3:9])
		v.Image = image
		v.Env = []core.EnvVar{{Name: "TARGET_URL", Value: "http://sink:8080/tick/k6"}, {Name: "K6_OTEL_EXPORTER_PROTOCOL", Value: "http/protobuf"}, {Name: "K6_OTEL_HTTP_EXPORTER_ENDPOINT", Value: "sink:8080"}, {Name: "K6_OTEL_HTTP_EXPORTER_INSECURE", Value: "true"}, {Name: "K6_OTEL_METRIC_PREFIX", Value: "k6_"}, {Name: "K6_OTEL_EXPORT_INTERVAL", Value: "1s"}}
		v.WorkflowInline = "version: 1\nname: k6-otlp\nsteps:\n  - id: k6\n    type: shell\n    with:\n      script: k6 run --duration 3s --vus 1 --out opentelemetry --tag execution_id=" + v.ExecutionID + " /tests/test.js\n"
		if e := v.Validate("run"); e != nil {
			t.Fatal(e)
		}
		if st, e := s.Run(ctx, v, os.Stdout); e != nil {
			t.Fatalf("k6: %v %+v", e, st)
		}
		m, e := counts()
		if e != nil || m["otlp_batches"] == 0 || m["otlp_execution_id:"+v.ExecutionID] == 0 || m["otlp_k6_metrics"] == 0 {
			t.Fatalf("missing OTLP metrics/attributes: %v %v", m, e)
		}
		if e = s.Purge(ctx, v.ExecutionID); e != nil {
			t.Fatal(e)
		}
	})

	t.Run("companion-workflows", func(t *testing.T) {
		dir := t.TempDir()
		manifest, _ := filepath.Abs("plugin.json")
		command := exec.CommandContext(ctx, wuko, "plugin", "install", manifest)
		command.Dir = dir
		if b, e := command.CombinedOutput(); e != nil {
			t.Fatalf("install: %s %v", b, e)
		}
		probe := exec.CommandContext(ctx, wuko, "plugin", "lock", "--help")
		probe.Dir = dir
		probeOutput, probeErr := probe.CombinedOutput()
		supportsLock := probeErr == nil && strings.Contains(string(probeOutput), "wuko plugin lock")
		files := map[string]string{}
		for _, op := range []string{"run", "list", "status", "watch", "logs", "report", "cancel", "purge"} {
			source := filepath.Join("..", "..", ".wuko", "workflows", "kube-"+op, "wuko.yaml")
			data, e := os.ReadFile(source)
			if e != nil {
				t.Fatal(e)
			}
			var doc map[string]any
			if e = yaml.Unmarshal(data, &doc); e != nil {
				t.Fatal(e)
			}
			if supportsLock {
				doc["plugins"] = map[string]any{"kube": map[string]any{"source": manifest}}
			}
			file := filepath.Join(dir, "kube-"+op+".yaml")
			if e = os.WriteFile(file, []byte(execution.Encode(doc)), 0600); e != nil {
				t.Fatal(e)
			}
			files[op] = file
			if supportsLock {
				command = exec.CommandContext(ctx, wuko, "plugin", "lock", file)
				command.Dir = dir
				if b, e := command.CombinedOutput(); e != nil {
					t.Fatalf("lock: %s %v", b, e)
				}
			}
			command = exec.CommandContext(ctx, wuko, "run", "--dry-run", "--file", file, "--var", "image=wuko-worker:dev", "--var", "workflow=/workflows/example.yaml", "--var", "execution_id=validation")
			command.Dir = dir
			if b, e := command.CombinedOutput(); e != nil {
				t.Fatalf("%s preflight: %s %v", op, b, e)
			}
		}
		id := "it-companion-" + execution.RandomID()[3:9]
		for _, op := range []string{"run", "list", "status", "watch", "logs", "report", "cancel", "purge"} {
			args := []string{"run", "--file", files[op], "--var", "execution_id=" + id, "--var", "kubeconfig=" + cfg, "--reporter", "plain"}
			if op == "run" {
				args = append(args, "--var", "image=wuko-worker:dev", "--var", "group=integration-companion", "--var", "workflow_inline=version: 1\nname: companion\nsteps:\n  - id: noop\n    type: shell\n    with: {script: ':'}\n")
			}
			command = exec.CommandContext(ctx, wuko, args...)
			command.Dir = dir
			if b, e := command.CombinedOutput(); e != nil {
				t.Fatalf("%s companion: %s %v", op, b, e)
			}
		}
	})

}
func runCommand(t *testing.T, name string, args ...string) {
	t.Helper()
	c := exec.CommandContext(t.Context(), name, args...)
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("%s: %s: %v", name, b, e)
	}
}
func eventually(t *testing.T, ctx context.Context, timeout time.Duration, predicate func() bool, reason string) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if predicate() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timer.C:
			t.Fatal("timeout: " + reason)
		case <-ticker.C:
		}
	}
}
