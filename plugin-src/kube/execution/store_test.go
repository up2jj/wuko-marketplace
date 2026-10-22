package execution

import (
	"context"
	"fmt"
	"sync"
	"testing"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// client-go's tracker does not enforce resourceVersion. Add the real CAS rule
// for tests that exercise competing owners and stale cleanup.
func service(t *testing.T) (*Service, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset()
	var mu sync.Mutex
	var revision int
	client.PrependReactor("update", "configmaps", func(a ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		obj := a.(ktesting.UpdateAction).GetObject().(*core.ConfigMap)
		old, err := client.Tracker().Get(core.SchemeGroupVersion.WithResource("configmaps"), a.GetNamespace(), obj.Name)
		if err != nil {
			return true, nil, err
		}
		if obj.ResourceVersion != old.(*core.ConfigMap).ResourceVersion {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.Name, fmt.Errorf("stale"))
		}
		revision++
		obj = obj.DeepCopy()
		obj.ResourceVersion = fmt.Sprint(revision)
		err = client.Tracker().Update(core.SchemeGroupVersion.WithResource("configmaps"), obj, a.GetNamespace())
		return true, obj, err
	})
	return &Service{API: client, Namespace: "wuko-jobs"}, client
}
func begin(t *testing.T, s *Service, id, group string) (Options, Record) {
	t.Helper()
	o := options(t, id)
	if group != "" {
		o.Concurrency = &Concurrency{Group: group, OnBusy: "error"}
	}
	r, created, err := s.Begin(t.Context(), o)
	if err != nil || !created {
		t.Fatalf("begin: %v %v", created, err)
	}
	if err = s.Authorize(t.Context(), o, r); err != nil {
		t.Fatal(err)
	}
	return o, r
}
func TestIdentityNeverResubmits(t *testing.T) {
	s, _ := service(t)
	o, r := begin(t, s, "same", "")
	again, created, err := s.Begin(t.Context(), o)
	if err != nil || created || again.Owner != r.Owner {
		t.Fatal("same identity submitted again", err)
	}
	o.Image = "other:v1"
	if _, _, err = s.Begin(t.Context(), o); err == nil {
		t.Fatal("configuration conflict accepted")
	}
}
func TestExclusiveGroupAndStaleRelease(t *testing.T) {
	s, client := service(t)
	_, a := begin(t, s, "first", "shared")
	_, b := begin(t, s, "second", "shared")
	if err := s.Acquire(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(t.Context(), b); err == nil {
		t.Fatal("overlap allowed")
	}
	if err := s.Revoke(t.Context(), a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	// Provisioning never created a Job, and owner was revoked: reclaim is safe.
	if err := s.Acquire(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	cm, err := client.CoreV1().ConfigMaps(s.Namespace).Get(t.Context(), GroupName("shared"), meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data["guard.json"] != Encode(Guard{Version: 1, Group: "shared", ExecutionID: b.ExecutionID, Token: b.GroupToken}) {
		t.Fatal("old cleanup released new owner")
	}
	if cm.Labels[IDLabel] != "" {
		t.Fatal("guard has per-execution labels")
	}
}
func TestGuardDoesNotReleaseOnExpiredLeaseOrDeletion(t *testing.T) {
	s, client := service(t)
	o, a := begin(t, s, "first", "shared")
	_, b := begin(t, s, "second", "shared")
	if err := s.Acquire(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	j := Job(o)
	j.Annotations["wuko.dev/owner"] = a.Owner
	j.UID = types.UID("job-1")
	if _, err := client.BatchV1().Jobs(s.Namespace).Create(t.Context(), j, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRecord(t.Context(), a.ExecutionID, func(r *Record) error { r.JobUID = "job-1"; r.PodUID = "pod-1"; return nil }); err != nil {
		t.Fatal(err)
	}
	p := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod", Namespace: s.Namespace, UID: "pod-1", Labels: map[string]string{IDLabel: a.ExecutionID}}, Status: core.PodStatus{Phase: core.PodRunning}}
	if _, err := client.CoreV1().Pods(s.Namespace).Create(t.Context(), p, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(t.Context(), a.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(t.Context(), b); err == nil {
		t.Fatal("lease expiry released active Pod")
	}
	if err := client.CoreV1().Pods(s.Namespace).Delete(t.Context(), p.Name, meta.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(t.Context(), b); err == nil {
		t.Fatal("missing claimed Pod treated as stopped")
	}
	p.Status.Phase = core.PodFailed
	if _, err := client.CoreV1().Pods(s.Namespace).Create(t.Context(), p, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(t.Context(), b); err != nil {
		t.Fatal(err)
	}
}
func TestClaimExactlyOnce(t *testing.T) {
	s, client := service(t)
	o, r := begin(t, s, "claim", "")
	j := Job(o)
	j.UID = "job"
	if _, err := client.BatchV1().Jobs(s.Namespace).Create(t.Context(), j, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRecord(t.Context(), r.ExecutionID, func(r *Record) error { r.JobUID = "job"; return nil }); err != nil {
		t.Fatal(err)
	}
	c := Control{Version: 1, Owner: r.Owner, JobUID: "job", Options: o}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"pod-a", "pod-b"} {
		p := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: id, Namespace: s.Namespace, UID: types.UID(id), OwnerReferences: []meta.OwnerReference{{Kind: "Job", UID: "job"}}}}
		if _, err := client.CoreV1().Pods(s.Namespace).Create(t.Context(), p, meta.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		wg.Go(func() { results <- s.Claim(t.Context(), c, id, id) })
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("%d claims", success)
	}
}
func TestRevokeCannotBeRenewed(t *testing.T) {
	s, _ := service(t)
	_, r := begin(t, s, "revoked", "")
	if err := s.Revoke(t.Context(), r.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := s.Renew(t.Context(), r.ExecutionID, r.Owner); err == nil {
		t.Fatal("revoked authorization renewed")
	}
}
func TestBlockedLogsDoNotBlockOwnership(t *testing.T) {
	gate := make(chan struct{})
	q := newLogQueue(writerFunc(func(b []byte) (int, error) { <-gate; return len(b), nil }))
	defer close(gate)
	defer q.close()
	for range 100 {
		q.write(make([]byte, 32*1024))
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.size > 1024*1024 || !q.gap {
		t.Fatal("unbounded logs or missing gap")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
func TestPurgeRefusesActiveOwner(t *testing.T) {
	s, _ := service(t)
	_, r := begin(t, s, "active", "")
	if err := s.Purge(t.Context(), r.ExecutionID); err == nil {
		t.Fatal("active execution purged")
	}
}
func TestWatchCancellationDoesNotRenew(t *testing.T) {
	s, client := service(t)
	_, r := begin(t, s, "observer", "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _ = s.Watch(ctx, r.ExecutionID, writerFunc(func(b []byte) (int, error) { return len(b), nil }))
	l, err := client.CoordinationV1().Leases(s.Namespace).Get(t.Context(), Name(r.ExecutionID, "auth"), meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if l.Annotations[Sequence] != "0" || l.Annotations[Revoked] != "" {
		t.Fatal("observer changed ownership")
	}
}

func TestOldOwnerCannotRevokeReplacement(t *testing.T) {
	s, client := service(t)
	o, old := begin(t, s, "reused", "")
	// A previously purged ID can be reused. An old deferred cleanup must not
	// revoke or suspend this new execution's resources.
	l, err := s.Lease(t.Context(), old.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	replacement := "new-owner"
	l.Spec.HolderIdentity = &replacement
	if _, err = client.CoordinationV1().Leases(s.Namespace).Update(t.Context(), l, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	j := Job(o)
	j.UID = "new-job"
	j.Annotations["wuko.dev/owner"] = replacement
	active := false
	j.Spec.Suspend = &active
	if _, err = client.BatchV1().Jobs(s.Namespace).Create(t.Context(), j, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = s.freezeRecord(t.Context(), old); err == nil {
		t.Fatal("stale cleanup accepted replacement")
	}
	l, err = s.Lease(t.Context(), old.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if l.Annotations[Revoked] == "true" {
		t.Fatal("replacement revoked")
	}
	job, err := client.BatchV1().Jobs(s.Namespace).Get(t.Context(), j.Name, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *job.Spec.Suspend {
		t.Fatal("replacement suspended")
	}
}
func TestConfigurationPreservesLargeNumbers(t *testing.T) {
	o, err := Decode([]byte(`{"image":"worker:v1","workflow":"/workflows/test.yaml","vars":{"large":9007199254740993}}`), "run")
	if err != nil {
		t.Fatal(err)
	}
	if Encode(o.Vars) != `{"large":9007199254740993}` {
		t.Fatal("numeric input precision lost", Encode(o.Vars))
	}
}

func TestPurgeResumesAfterJobDeletion(t *testing.T) {
	s, client := service(t)
	o, r := begin(t, s, "purge-retry", "")
	j := Job(o)
	j.UID = "job"
	j.Annotations["wuko.dev/owner"] = r.Owner
	if _, err := client.BatchV1().Jobs(s.Namespace).Create(t.Context(), j, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRecord(t.Context(), r.ExecutionID, func(r *Record) error { r.JobUID = "job"; return nil }); err != nil {
		t.Fatal(err)
	}
	input := &core.ConfigMap{ObjectMeta: o.Meta("input"), Data: map[string]string{"control.json": Encode(Control{Version: 1, Owner: r.Owner})}}
	if _, err := client.CoreV1().ConfigMaps(s.Namespace).Create(t.Context(), input, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(t.Context(), r.ExecutionID); err != nil {
		t.Fatal(err)
	}
	failed := false
	client.PrependReactor("delete", "configmaps", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.(ktesting.DeleteAction).GetName() == input.Name && !failed {
			failed = true
			return true, nil, fmt.Errorf("interrupted purge")
		}
		return false, nil, nil
	})
	if err := s.Purge(t.Context(), r.ExecutionID); err == nil {
		t.Fatal("expected injected interruption")
	}
	if err := s.Purge(t.Context(), r.ExecutionID); err != nil {
		t.Fatal("purge did not resume", err)
	}
}
func TestPurgeFindsJobFromPartialProvisioning(t *testing.T) {
	s, client := service(t)
	o, r := begin(t, s, "partial", "")
	j := Job(o)
	j.UID = "job"
	j.Annotations["wuko.dev/owner"] = r.Owner
	if _, err := client.BatchV1().Jobs(s.Namespace).Create(t.Context(), j, meta.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(t.Context(), r.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := s.Purge(t.Context(), r.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.BatchV1().Jobs(s.Namespace).Get(t.Context(), j.Name, meta.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("partial Job retained after purge", err)
	}
}
