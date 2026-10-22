package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	batch "k8s.io/api/batch/v1"
	coordination "k8s.io/api/coordination/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

type Service struct {
	API       kubernetes.Interface
	Namespace string
}

// RequestTimeout bounds one short API exchange. Streams are never bounded by it.
const RequestTimeout = 2 * time.Second

var (
	// ErrRevoked reports a definite revocation or owner change, never a transient failure.
	ErrRevoked = errors.New("authorization revoked or owner changed")
	// ErrGuardChanged reports that the exclusive group reservation no longer matches.
	ErrGuardChanged = errors.New("exclusive group reservation changed")
)

func (s *Service) record(ctx context.Context, id string) (*core.ConfigMap, Record, error) {
	cm, err := s.API.CoreV1().ConfigMaps(s.Namespace).Get(ctx, Name(id, "result"), meta.GetOptions{})
	if err != nil {
		return nil, Record{}, err
	}
	if cm.Labels[Managed] != "wuko-kube" || cm.Labels[IDLabel] != id {
		return nil, Record{}, fmt.Errorf("resource is not a managed execution")
	}
	r, err := ReadRecord(cm)
	return cm, r, err
}
func (s *Service) UpdateRecord(ctx context.Context, id string, update func(*Record) error) error {
	for range 8 {
		cm, r, err := s.record(ctx, id)
		if err != nil {
			return err
		}
		if err = update(&r); err != nil {
			return err
		}
		cm.Data["record.json"] = Encode(r)
		_, err = s.API.CoreV1().ConfigMaps(s.Namespace).Update(ctx, cm, meta.UpdateOptions{})
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("execution record conflict")
}
func (s *Service) Lease(ctx context.Context, id string) (*coordination.Lease, error) {
	return s.API.CoordinationV1().Leases(s.Namespace).Get(ctx, Name(id, "auth"), meta.GetOptions{})
}
func (s *Service) Renew(ctx context.Context, id, owner string) error {
	for range 8 {
		l, err := s.Lease(ctx, id)
		if err != nil {
			return err
		}
		if l.Annotations[Revoked] == "true" || l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != owner {
			return ErrRevoked
		}
		n, err := strconv.ParseUint(l.Annotations[Sequence], 10, 64)
		if err != nil {
			return err
		}
		l.Annotations[Sequence] = strconv.FormatUint(n+1, 10)
		now := meta.NewMicroTime(time.Now())
		l.Spec.RenewTime = &now
		_, err = s.API.CoordinationV1().Leases(s.Namespace).Update(ctx, l, meta.UpdateOptions{})
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("authorization conflict")
}
func (s *Service) Revoke(ctx context.Context, id string) error { return s.revokeOwner(ctx, id, "") }
func (s *Service) revokeOwner(ctx context.Context, id, owner string) error {
	for range 8 {
		l, err := s.Lease(ctx, id)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if owner != "" && (l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != owner) {
			return fmt.Errorf("execution owner changed")
		}
		l.Annotations[Revoked] = "true"
		_, err = s.API.CoordinationV1().Leases(s.Namespace).Update(ctx, l, meta.UpdateOptions{})
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("revocation conflict")
}
func (s *Service) ownerExpired(ctx context.Context, id string) (bool, error) {
	return leaseExpired(s.Lease(ctx, id))
}
func leaseExpired(l *coordination.Lease, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	if l.Annotations[Revoked] == "true" {
		return true, nil
	}
	if l.Spec.RenewTime == nil || l.Spec.LeaseDurationSeconds == nil {
		return false, fmt.Errorf("invalid authorization Lease")
	}
	return time.Now().After(l.Spec.RenewTime.Add(time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second)), nil
}
func (s *Service) Suspend(ctx context.Context, id string) error {
	b := []byte(`{"spec":{"suspend":true}}`)
	_, err := s.API.BatchV1().Jobs(s.Namespace).Patch(ctx, Name(id, "job"), types.MergePatchType, b, meta.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// Stopped never treats a deletion timestamp, missing Job or expired Lease as
// evidence that a process stopped. Both the expected Job and every Pod must agree.
func (s *Service) Stopped(ctx context.Context, r Record) (bool, error) {
	j, pods, err := s.observe(ctx, r.ExecutionID)
	if err != nil {
		return false, err
	}
	return stopped(r, j, pods)
}

// observe returns the execution's Job (nil when missing) and its Pods.
func (s *Service) observe(ctx context.Context, id string) (*batch.Job, []core.Pod, error) {
	j, err := s.API.BatchV1().Jobs(s.Namespace).Get(ctx, Name(id, "job"), meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		j = nil
	} else if err != nil {
		return nil, nil, err
	}
	pods, err := s.API.CoreV1().Pods(s.Namespace).List(ctx, meta.ListOptions{LabelSelector: IDLabel + "=" + id})
	if err != nil {
		return nil, nil, err
	}
	return j, pods.Items, nil
}
func stopped(r Record, j *batch.Job, pods []core.Pod) (bool, error) {
	if j == nil {
		if r.JobUID != "" && r.State != "purging" {
			return false, fmt.Errorf("Job missing; termination is unknown")
		}
	} else if r.JobUID != "" && string(j.UID) != r.JobUID {
		return false, fmt.Errorf("Job identity changed")
	}
	for _, p := range pods {
		if p.Status.Phase != core.PodSucceeded && p.Status.Phase != core.PodFailed {
			return false, nil
		}
	}
	if j == nil {
		return r.State == "purging" || (r.JobUID == "" && len(pods) == 0), nil
	}
	if (j.Spec.Suspend == nil || !*j.Spec.Suspend) && !jobTerminal(j) {
		return false, nil
	}
	// A previously claimed Pod disappearing is not proof of termination.
	if r.PodUID != "" {
		found := false
		for _, p := range pods {
			if string(p.UID) == r.PodUID {
				found = true
			}
		}
		if !found && r.State != "purging" {
			return false, fmt.Errorf("claimed Pod missing; termination is unknown")
		}
	}
	return true, nil
}

type Guard struct {
	Version     int    `json:"version"`
	Group       string `json:"group"`
	ExecutionID string `json:"execution_id"`
	Token       string `json:"token"`
	JobUID      string `json:"job_uid,omitempty"`
}

func (s *Service) Acquire(ctx context.Context, r Record) error {
	if r.Group == "" {
		return nil
	}
	cm := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: GroupName(r.Group), Namespace: s.Namespace, Labels: map[string]string{Managed: "wuko-kube", RoleLabel: "guard"}}, Data: map[string]string{"guard.json": Encode(Guard{Version: 1, Group: r.Group, ExecutionID: r.ExecutionID, Token: r.GroupToken})}}
	_, err := s.API.CoreV1().ConfigMaps(s.Namespace).Create(ctx, cm, meta.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	for range 8 {
		old, err := s.API.CoreV1().ConfigMaps(s.Namespace).Get(ctx, cm.Name, meta.GetOptions{})
		if err != nil {
			return err
		}
		var g Guard
		if err = json.Unmarshal([]byte(old.Data["guard.json"]), &g); err != nil || g.Version != 1 || g.Group != r.Group || old.Labels[Managed] != "wuko-kube" {
			return fmt.Errorf("invalid group guard")
		}
		if g.ExecutionID == r.ExecutionID && g.Token == r.GroupToken {
			return nil
		}
		if g.ExecutionID != "" {
			_, previous, err := s.record(ctx, g.ExecutionID)
			if err != nil {
				return fmt.Errorf("group %q blocked by %s: termination unknown: %w", r.Group, g.ExecutionID, err)
			}
			expired, err := s.ownerExpired(ctx, g.ExecutionID)
			if err != nil || !expired {
				return fmt.Errorf("group %q busy: execution %s", r.Group, g.ExecutionID)
			}
			if previous.GroupToken != g.Token {
				return fmt.Errorf("guard identity mismatch; termination unknown")
			}
			if err = s.freezeRecord(ctx, previous); err != nil {
				return err
			}
			stopped, err := s.Stopped(ctx, previous)
			if err != nil || !stopped {
				return fmt.Errorf("group %q busy or termination unknown: execution %s", r.Group, g.ExecutionID)
			}
		}
		old.Data["guard.json"] = cm.Data["guard.json"]
		_, err = s.API.CoreV1().ConfigMaps(s.Namespace).Update(ctx, old, meta.UpdateOptions{})
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("group acquisition conflict")
}
func (s *Service) bindGuard(ctx context.Context, r Record, jobUID string) error {
	if r.Group == "" {
		return nil
	}
	for range 8 {
		cm, err := s.API.CoreV1().ConfigMaps(s.Namespace).Get(ctx, GroupName(r.Group), meta.GetOptions{})
		if err != nil {
			return err
		}
		var g Guard
		if json.Unmarshal([]byte(cm.Data["guard.json"]), &g) != nil || g.ExecutionID != r.ExecutionID || g.Token != r.GroupToken || (g.JobUID != "" && g.JobUID != jobUID) {
			return fmt.Errorf("guard reservation changed")
		}
		g.JobUID = jobUID
		cm.Data["guard.json"] = Encode(g)
		_, err = s.API.CoreV1().ConfigMaps(s.Namespace).Update(ctx, cm, meta.UpdateOptions{})
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("guard binding conflict")
}
func (s *Service) CheckGuard(ctx context.Context, c Control) error {
	if c.Options.Concurrency == nil {
		return nil
	}
	cm, err := s.API.CoreV1().ConfigMaps(s.Namespace).Get(ctx, GroupName(c.Options.Concurrency.Group), meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ErrGuardChanged
	}
	if err != nil {
		return err
	}
	var g Guard
	if json.Unmarshal([]byte(cm.Data["guard.json"]), &g) != nil || g.Version != 1 || g.ExecutionID != c.Options.ExecutionID || g.Token != c.GroupToken || g.JobUID != c.JobUID {
		return ErrGuardChanged
	}
	return nil
}
func (s *Service) Release(ctx context.Context, r Record) error {
	if r.Group == "" {
		return nil
	}
	stopped, err := s.Stopped(ctx, r)
	if err != nil {
		return err
	}
	if !stopped {
		return fmt.Errorf("termination not confirmed")
	}
	for range 8 {
		cm, err := s.API.CoreV1().ConfigMaps(s.Namespace).Get(ctx, GroupName(r.Group), meta.GetOptions{})
		if err != nil {
			return err
		}
		var g Guard
		if err = json.Unmarshal([]byte(cm.Data["guard.json"]), &g); err != nil {
			return err
		}
		if g.ExecutionID != r.ExecutionID || g.Token != r.GroupToken {
			return nil
		}
		g.ExecutionID = ""
		g.Token = ""
		g.JobUID = ""
		cm.Data["guard.json"] = Encode(g)
		_, err = s.API.CoreV1().ConfigMaps(s.Namespace).Update(ctx, cm, meta.UpdateOptions{})
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("group release conflict")
}
func (s *Service) Claim(ctx context.Context, c Control, podUID, podName string) error {
	p, err := s.API.CoreV1().Pods(s.Namespace).Get(ctx, podName, meta.GetOptions{})
	if err != nil {
		return err
	}
	owned := false
	for _, ref := range p.OwnerReferences {
		if ref.Kind == "Job" && string(ref.UID) == c.JobUID {
			owned = true
		}
	}
	if !owned || string(p.UID) != podUID {
		return fmt.Errorf("pod does not belong to expected Job")
	}
	if err = s.CheckGuard(ctx, c); err != nil {
		return err
	}
	return s.UpdateRecord(ctx, c.Options.ExecutionID, func(r *Record) error {
		if r.PodUID != "" {
			return fmt.Errorf("execution already claimed by Pod %s", r.PodUID)
		}
		if r.JobUID != c.JobUID || r.Owner != c.Owner || r.State != "provisioning" {
			return fmt.Errorf("execution cannot be claimed")
		}
		r.PodUID = podUID
		r.State = "running"
		return nil
	})
}
func (s *Service) Freeze(ctx context.Context, id string) error {
	_, r, err := s.record(ctx, id)
	if err != nil {
		return err
	}
	return s.freezeRecord(ctx, r)
}
func (s *Service) freezeRecord(ctx context.Context, r Record) error {
	return errors.Join(s.revokeOwner(ctx, r.ExecutionID, r.Owner), s.suspendOwner(ctx, r))
}
func (s *Service) suspendOwner(ctx context.Context, r Record) error {
	j, err := s.API.BatchV1().Jobs(s.Namespace).Get(ctx, Name(r.ExecutionID, "job"), meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.Annotations["wuko.dev/owner"] != r.Owner {
		return fmt.Errorf("Job owner changed")
	}
	patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": j.UID}, {"op": "test", "path": "/metadata/annotations/wuko.dev~1owner", "value": r.Owner}, {"op": "add", "path": "/spec/suspend", "value": true}}
	data, _ := json.Marshal(patch)
	_, err = s.API.BatchV1().Jobs(s.Namespace).Patch(ctx, j.Name, types.JSONPatchType, data, meta.PatchOptions{})
	return err
}

func jobTerminal(j *batch.Job) bool {
	for _, c := range j.Status.Conditions {
		if (c.Type == batch.JobComplete || c.Type == batch.JobFailed) && c.Status == core.ConditionTrue {
			return true
		}
	}
	return false
}
