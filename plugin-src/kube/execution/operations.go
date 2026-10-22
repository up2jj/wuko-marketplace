package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

type Status struct {
	Record               Record            `json:"record"`
	Labels               map[string]string `json:"labels"`
	Annotations          map[string]string `json:"annotations"`
	Phase                string            `json:"phase"`
	TerminationConfirmed bool              `json:"termination_confirmed"`
	Waiting              []string          `json:"waiting,omitempty"`
	Pods                 []string          `json:"pods,omitempty"`
}

func (s *Service) Status(ctx context.Context, id string) (Status, error) {
	st, _, err := s.status(ctx, id)
	return st, err
}

// status also returns the observed Job (nil when missing) so callers do not
// fetch it again.
func (s *Service) status(ctx context.Context, id string) (Status, *batch.Job, error) {
	cm, r, err := s.record(ctx, id)
	if err != nil {
		return Status{}, nil, err
	}
	j, pods, err := s.observe(ctx, id)
	if err != nil {
		return Status{Record: r, Labels: cm.Labels, Annotations: cm.Annotations, Phase: r.State}, nil, err
	}
	st, err := s.statusFrom(ctx, cm, r, j, pods)
	return st, j, err
}
func (s *Service) statusFrom(ctx context.Context, cm *core.ConfigMap, r Record, j *batch.Job, pods []core.Pod) (Status, error) {
	id := r.ExecutionID
	st := Status{Record: r, Labels: cm.Labels, Annotations: cm.Annotations, Phase: r.State}
	for _, p := range pods {
		st.Pods = append(st.Pods, p.Name)
		for _, c := range p.Status.ContainerStatuses {
			if c.State.Waiting != nil {
				st.Waiting = append(st.Waiting, c.State.Waiting.Reason+": "+c.State.Waiting.Message)
			}
		}
		for _, c := range p.Status.Conditions {
			if c.Status == core.ConditionFalse && c.Message != "" {
				st.Waiting = append(st.Waiting, c.Reason+": "+c.Message)
			}
		}
	}
	if j == nil {
		st.Phase = "provisioning"
		expired, e := s.ownerExpired(ctx, id)
		if e == nil && expired {
			if done, se := stopped(r, j, pods); se == nil && done {
				st.Phase = "stopped"
				st.TerminationConfirmed = true
			}
		}
		if r.JobUID != "" && r.State != "purging" {
			st.Phase = "unknown"
		}
		return st, nil
	}
	if r.JobUID != "" && string(j.UID) != r.JobUID {
		return st, fmt.Errorf("Job identity changed")
	}
	expired, e := s.ownerExpired(ctx, id)
	if e != nil {
		return st, e
	}
	if expired {
		st.Phase = "stopping"
	}
	if jobTerminal(j) {
		if j.Status.Succeeded > 0 {
			st.Phase = "succeeded"
		} else {
			st.Phase = "failed"
		}
	}
	done, e := stopped(r, j, pods)
	if e == nil {
		st.TerminationConfirmed = done
	}
	if done {
		st.Phase = "stopped"
		if r.State == "finished" {
			if r.ExitCode == 0 {
				st.Phase = "succeeded"
			} else {
				st.Phase = "failed"
			}
		} else if r.PodUID != "" {
			st.Record.ReportState = "unpublished"
			st.Record.Reason = "worker exited without publishing final report"
		}
	}
	return st, nil
}

// List fetches managed Jobs and Pods once instead of once per execution.
func (s *Service) List(ctx context.Context, selector string) ([]Status, error) {
	sel := Managed + "=wuko-kube," + RoleLabel + "=result"
	if selector != "" {
		sel += "," + selector
	}
	cms, err := s.API.CoreV1().ConfigMaps(s.Namespace).List(ctx, meta.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, err
	}
	result := []Status{}
	if len(cms.Items) == 0 {
		return result, nil
	}
	jobList, err := s.API.BatchV1().Jobs(s.Namespace).List(ctx, meta.ListOptions{LabelSelector: IDLabel})
	if err != nil {
		return nil, err
	}
	podList, err := s.API.CoreV1().Pods(s.Namespace).List(ctx, meta.ListOptions{LabelSelector: IDLabel})
	if err != nil {
		return nil, err
	}
	jobs := map[string]*batch.Job{}
	for i := range jobList.Items {
		j := &jobList.Items[i]
		if id := j.Labels[IDLabel]; j.Name == Name(id, "job") {
			jobs[id] = j
		}
	}
	pods := map[string][]core.Pod{}
	for _, p := range podList.Items {
		pods[p.Labels[IDLabel]] = append(pods[p.Labels[IDLabel]], p)
	}
	for i := range cms.Items {
		cm := &cms.Items[i]
		id := cm.Labels[IDLabel]
		if cm.Name != Name(id, "result") {
			continue
		}
		r, err := ReadRecord(cm)
		if err != nil {
			return nil, err
		}
		st, err := s.statusFrom(ctx, cm, r, jobs[id], pods[id])
		if err != nil {
			return nil, err
		}
		result = append(result, st)
	}
	return result, nil
}
func (s *Service) Cancel(ctx context.Context, id string) (Status, error) {
	if err := s.Freeze(ctx, id); err != nil {
		return Status{}, err
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		st, err := s.Status(wait, id)
		if err != nil {
			return st, err
		}
		if st.TerminationConfirmed {
			return st, nil
		}
		select {
		case <-wait.Done():
			st.Phase = "stop_requested"
			return st, nil
		case <-time.After(time.Second):
		}
	}
}
func (s *Service) Purge(ctx context.Context, id string) error {
	resultCM, r, err := s.record(ctx, id)
	if err != nil {
		return err
	}
	expired, err := s.ownerExpired(ctx, id)
	if apierrors.IsNotFound(err) && (r.JobUID == "" || r.State == "purging") {
		expired = true
		err = nil
	}
	if err != nil {
		return err
	}
	if !expired {
		return fmt.Errorf("execution owner is still active")
	}
	if err = s.Freeze(ctx, id); err != nil {
		return err
	}
	if r.JobUID == "" {
		job, e := s.API.BatchV1().Jobs(s.Namespace).Get(ctx, Name(id, "job"), meta.GetOptions{})
		if e != nil && !apierrors.IsNotFound(e) {
			return e
		}
		if e == nil {
			if job.Annotations["wuko.dev/owner"] != r.Owner {
				return fmt.Errorf("Job owner changed during purge")
			}
			r.JobUID = string(job.UID)
			if err = s.UpdateRecord(ctx, id, func(record *Record) error {
				if record.Owner != r.Owner {
					return fmt.Errorf("execution owner changed")
				}
				record.JobUID = r.JobUID
				return nil
			}); err != nil {
				return err
			}
		}
	}
	stopped, err := s.Stopped(ctx, r)
	if err != nil {
		return err
	}
	if !stopped {
		return fmt.Errorf("refusing purge: termination not confirmed")
	}
	if err = s.Release(ctx, r); err != nil {
		return err
	}
	// Persist confirmed termination before deletions, so an interrupted purge
	// can resume without turning an expected missing object into uncertainty.
	if err = s.UpdateRecord(ctx, id, func(record *Record) error {
		if record.Owner != r.Owner {
			return fmt.Errorf("execution owner changed during purge")
		}
		record.State = "purging"
		return nil
	}); err != nil {
		return err
	}
	pods, err := s.API.CoreV1().Pods(s.Namespace).List(ctx, meta.ListOptions{LabelSelector: IDLabel + "=" + id})
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		// A resumed purge may find Pods whose finalizer is already gone; a
		// replace on the then-missing finalizers path would fail.
		if !slices.Contains(p.Finalizers, "wuko.dev/retain") {
			continue
		}
		remaining := slices.DeleteFunc(p.Finalizers, func(f string) bool { return f == "wuko.dev/retain" })
		patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": p.UID}, {"op": "test", "path": "/metadata/resourceVersion", "value": p.ResourceVersion}, {"op": "replace", "path": "/metadata/finalizers", "value": remaining}}
		data, _ := json.Marshal(patch)
		if _, err = s.API.CoreV1().Pods(s.Namespace).Patch(ctx, p.Name, types.JSONPatchType, data, meta.PatchOptions{}); err != nil {
			return err
		}
	}
	// Delete the Job with UID preconditions. Keep the result until all other
	// deletions succeed so an interrupted purge remains discoverable.
	if r.JobUID != "" {
		uid := types.UID(r.JobUID)
		err = s.API.BatchV1().Jobs(s.Namespace).Delete(ctx, Name(id, "job"), meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &uid}, PropagationPolicy: ptr.To(meta.DeletePropagationBackground)})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	input, e := s.API.CoreV1().ConfigMaps(s.Namespace).Get(ctx, Name(id, "input"), meta.GetOptions{})
	if e != nil && !apierrors.IsNotFound(e) {
		return e
	}
	if e == nil {
		var control Control
		if json.Unmarshal([]byte(input.Data["control.json"]), &control) != nil || control.Owner != r.Owner {
			return fmt.Errorf("input owner changed during purge")
		}
		err = s.API.CoreV1().ConfigMaps(s.Namespace).Delete(ctx, Name(id, "input"), meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &input.UID}})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	lease, e := s.Lease(ctx, id)
	if e != nil && !apierrors.IsNotFound(e) {
		return e
	}
	if e == nil {
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != r.Owner {
			return fmt.Errorf("execution owner changed during purge")
		}
		err = s.API.CoordinationV1().Leases(s.Namespace).Delete(ctx, Name(id, "auth"), meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &lease.UID}})
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return s.API.CoreV1().ConfigMaps(s.Namespace).Delete(ctx, Name(id, "result"), meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &resultCM.UID}})
}

func (s *Service) Run(ctx context.Context, o Options, out io.Writer) (Status, error) {
	if o.ExecutionID == "" {
		o.ExecutionID = RandomID()
	}
	// Printing must not precede owner cancellation handling or block heartbeats.
	if _, err := fmt.Fprintln(out, "execution_id="+o.ExecutionID); err != nil {
		return Status{}, err
	}
	logs := newLogQueue(out)
	defer logs.close()
	r, created, err := s.Begin(ctx, o)
	if err != nil {
		return Status{}, err
	}
	if !created {
		st, err := s.Status(ctx, o.ExecutionID)
		if err != nil {
			return st, err
		}
		if !st.TerminationConfirmed {
			return st, fmt.Errorf("execution_id already owned; inspection and cancellation only")
		}
		return outcome(st)
	}
	ownerCtx, stopOwner := context.WithCancel(ctx)
	defer func() {
		stopOwner()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.freezeRecord(cleanup, r)
		_, latest, e := s.record(cleanup, o.ExecutionID)
		if e == nil && latest.Owner == r.Owner {
			_ = s.Release(cleanup, latest)
		}
	}()
	if err = s.Authorize(ctx, o, r); err != nil {
		return Status{}, err
	}
	_, ttl, interval, _, _ := o.Durations()
	heartbeatCtx, stopHeartbeat := context.WithCancel(ownerCtx)
	defer stopHeartbeat()
	done := make(chan struct{})
	defer func() { stopHeartbeat(); <-done }()
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		renewed := time.Now()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				renew, cancel := context.WithTimeout(heartbeatCtx, interval)
				e := s.Renew(renew, o.ExecutionID, r.Owner)
				cancel()
				if e == nil {
					renewed = time.Now()
					continue
				}
				// Revocation is definite. Transient failures are retried only while
				// the last successful renewal still covers the next tick.
				if errors.Is(e, ErrRevoked) || time.Since(renewed)+interval >= ttl {
					stopOwner()
					return
				}
			}
		}
	}()
	if err = s.Provision(ownerCtx, o, r); err != nil {
		return Status{}, err
	}
	logCtx, stopLogs := context.WithCancel(ownerCtx)
	defer stopLogs()
	logsDone := make(chan struct{})
	go func() { defer close(logsDone); _ = s.Logs(logCtx, o.ExecutionID, true, 100, logs) }()
	for {
		st, j, e := s.status(ownerCtx, o.ExecutionID)
		if e != nil {
			return st, e
		}
		if st.Record.Owner != r.Owner {
			return st, fmt.Errorf("execution owner changed")
		}
		if j == nil {
			return st, fmt.Errorf("Job missing; termination is unknown")
		}
		if jobTerminal(j) || st.Record.State == "finished" {
			// Stop renewal before revoking. Observation retains its own context.
			stopHeartbeat()
			<-done
			// Revoke first, then suspend and wait for actual Pod termination.
			if e = s.freezeRecord(ownerCtx, st.Record); e != nil {
				return st, e
			}
			for {
				st, e = s.Status(ownerCtx, o.ExecutionID)
				if e != nil {
					return st, e
				}
				if st.TerminationConfirmed {
					// Let the follower drain the final log lines before returning.
					select {
					case <-logsDone:
					case <-time.After(5 * time.Second):
					}
					return outcome(st)
				}
				select {
				case <-ownerCtx.Done():
					return st, fmt.Errorf("execution authorization stopped: %w", ownerCtx.Err())
				case <-time.After(time.Second):
				}
			}
		}
		select {
		case <-ownerCtx.Done():
			return st, fmt.Errorf("execution authorization stopped: %w", ownerCtx.Err())
		case <-time.After(time.Second):
		}
	}
}
func outcome(st Status) (Status, error) {
	if st.Record.State != "finished" || st.Record.ExitCode != 0 {
		return st, fmt.Errorf("execution %s unsuccessful (%s)", st.Record.ExecutionID, st.Phase)
	}
	if st.Record.ReportState != "available" {
		return st, fmt.Errorf("execution %s report is %s", st.Record.ExecutionID, st.Record.ReportState)
	}
	return st, nil
}
func (s *Service) Watch(ctx context.Context, id string, out io.Writer) (Status, error) {
	last := ""
	for {
		st, err := s.Status(ctx, id)
		if err != nil {
			return st, err
		}
		b, _ := json.Marshal(st)
		if string(b) != last {
			if _, err = fmt.Fprintln(out, string(b)); err != nil {
				return st, err
			}
			last = string(b)
		}
		if st.TerminationConfirmed {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
func (s *Service) Logs(ctx context.Context, id string, follow bool, tail int64, out io.Writer) error {
	if _, _, err := s.record(ctx, id); err != nil {
		return err
	}
	if tail <= 0 {
		tail = 100
	}
	seen := map[string]bool{}
	for {
		pods, err := s.API.CoreV1().Pods(s.Namespace).List(ctx, meta.ListOptions{LabelSelector: IDLabel + "=" + id})
		if err != nil {
			return err
		}
		for _, p := range pods.Items {
			if seen[p.Name] {
				continue
			}
			stream, err := s.API.CoreV1().Pods(s.Namespace).GetLogs(p.Name, &core.PodLogOptions{Container: "worker", Follow: follow, TailLines: &tail}).Stream(ctx)
			if err != nil {
				continue
			}
			if !follow {
				seen[p.Name] = true
			}
			_, _ = fmt.Fprintf(out, "[%s/%s]\n", id, p.Name)
			_, err = io.Copy(out, stream)
			_ = stream.Close()
			if follow {
				pod, e := s.API.CoreV1().Pods(s.Namespace).Get(ctx, p.Name, meta.GetOptions{})
				if e == nil && (pod.Status.Phase == core.PodSucceeded || pod.Status.Phase == core.PodFailed) {
					seen[p.Name] = true
				}
			}
			if err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if !follow {
			return nil
		}
		st, err := s.Status(ctx, id)
		if err != nil {
			return err
		}
		if st.TerminationConfirmed {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// A bounded queue isolates ownership from slow SDK/log consumers. One writer
// goroutine may remain blocked in a host stream until the protocol closes it.
type logQueue struct {
	mu     sync.Mutex
	chunks [][]byte
	size   int
	gap    bool
	wake   chan struct{}
	done   chan struct{}
	exited chan struct{}
	once   sync.Once
}

// logFlushTimeout bounds how long close waits for queued chunks to be written.
const logFlushTimeout = 2 * time.Second

func newLogQueue(out io.Writer) *logQueue {
	q := &logQueue{wake: make(chan struct{}, 1), done: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		defer close(q.exited)
		for {
			select {
			case <-q.done:
				q.drain(out)
				return
			case <-q.wake:
			}
			if !q.drain(out) {
				return
			}
		}
	}()
	return q
}

// drain writes every queued chunk and reports whether the writer still works.
func (q *logQueue) drain(out io.Writer) bool {
	for {
		q.mu.Lock()
		if q.gap {
			q.gap = false
			q.mu.Unlock()
			_, _ = io.WriteString(out, "[wuko-kube: log gap; 1 MiB queue overflow]\n")
			continue
		}
		if len(q.chunks) == 0 {
			q.mu.Unlock()
			return true
		}
		b := q.chunks[0]
		q.chunks = q.chunks[1:]
		q.size -= len(b)
		q.mu.Unlock()
		if _, err := out.Write(b); err != nil {
			return false
		}
	}
}
func (q *logQueue) write(b []byte) {
	q.mu.Lock()
	if q.size+len(b) > 1024*1024 {
		q.gap = true
	} else {
		q.chunks = append(q.chunks, append([]byte(nil), b...))
		q.size += len(b)
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
func (q *logQueue) Write(b []byte) (int, error) { q.write(b); return len(b), nil }

// close flushes queued chunks, waiting at most logFlushTimeout for a blocked writer.
func (q *logQueue) close() {
	q.once.Do(func() { close(q.done) })
	select {
	case <-q.exited:
	case <-time.After(logFlushTimeout):
	}
}
