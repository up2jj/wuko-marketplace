package execution

import (
	"context"
	"fmt"
	"time"

	batch "k8s.io/api/batch/v1"
	coordination "k8s.io/api/coordination/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func Job(o Options) *batch.Job {
	max, _, _, grace, _ := o.Durations()
	env := append([]core.EnvVar(nil), o.Env...)
	env = append(env, InternalEnv...)
	mounts := []core.VolumeMount{{Name: "control", MountPath: "/var/run/wuko/control", ReadOnly: true}, {Name: "runtime", MountPath: "/var/run/wuko/runtime"}}
	volumes := []core.Volume{{Name: "control", VolumeSource: core.VolumeSource{ConfigMap: &core.ConfigMapVolumeSource{LocalObjectReference: core.LocalObjectReference{Name: Name(o.ExecutionID, "input")}, DefaultMode: ptr.To(int32(0444))}}}, {Name: "runtime", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{SizeLimit: nil}}}}
	for _, m := range o.Volumes {
		mounts = append(mounts, core.VolumeMount{Name: m.Name, MountPath: m.MountPath, ReadOnly: true})
		volumes = append(volumes, core.Volume{Name: m.Name, VolumeSource: core.VolumeSource{Secret: m.Secret, ConfigMap: m.ConfigMap}})
	}
	pm := o.Meta("pod")
	pm.Name = ""
	pm.Finalizers = []string{"wuko.dev/retain"}
	return &batch.Job{ObjectMeta: o.Meta("job"), Spec: batch.JobSpec{Suspend: ptr.To(true), Parallelism: ptr.To(int32(1)), Completions: ptr.To(int32(1)), BackoffLimit: ptr.To(int32(0)), ActiveDeadlineSeconds: ptr.To(int64((max + grace + 10*time.Second) / time.Second)), Template: core.PodTemplateSpec{ObjectMeta: pm, Spec: core.PodSpec{RestartPolicy: core.RestartPolicyNever, ServiceAccountName: o.ServiceAccount, TerminationGracePeriodSeconds: ptr.To(int64(grace/time.Second) + 5), HostPID: false, ShareProcessNamespace: ptr.To(false), NodeSelector: o.NodeSelector, Tolerations: o.Tolerations, ImagePullSecrets: o.ImagePullSecrets, SecurityContext: &core.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(10001)), RunAsGroup: ptr.To(int64(10001)), FSGroup: ptr.To(int64(10001))}, Containers: []core.Container{{Name: "worker", Image: o.Image, Command: []string{"/usr/local/bin/wuko-supervisor"}, WorkingDir: o.Workdir, Env: env, EnvFrom: o.EnvFrom, Resources: o.Resources, VolumeMounts: mounts, SecurityContext: &core.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}}, SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault}}}}, Volumes: volumes}}}}
}

func (s *Service) Begin(ctx context.Context, o Options) (Record, bool, error) {
	r := Record{Version: 1, ExecutionID: o.ExecutionID, Fingerprint: o.Digest(), Owner: RandomID(), State: "provisioning", ReportState: "unpublished", CreatedAt: time.Now().UTC()}
	if o.Concurrency != nil {
		r.Group = o.Concurrency.Group
		r.GroupToken = RandomID()
	}
	cm := &core.ConfigMap{ObjectMeta: o.Meta("result"), Data: map[string]string{"record.json": Encode(r)}}
	_, err := s.API.CoreV1().ConfigMaps(s.Namespace).Create(ctx, cm, meta.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, old, e := s.record(ctx, o.ExecutionID)
		if e != nil {
			return old, false, e
		}
		if old.Fingerprint != r.Fingerprint {
			return old, false, fmt.Errorf("execution_id configuration conflict")
		}
		return old, false, nil
	}
	return r, true, err
}
func (s *Service) Authorize(ctx context.Context, o Options, r Record) error {
	_, ttl, _, _, _ := o.Durations()
	now := meta.NewMicroTime(time.Now())
	m := o.Meta("auth")
	m.Annotations[Sequence] = "0"
	l := &coordination.Lease{ObjectMeta: m, Spec: coordination.LeaseSpec{HolderIdentity: ptr.To(r.Owner), LeaseDurationSeconds: ptr.To(int32((ttl + time.Second - 1) / time.Second)), RenewTime: &now}}
	_, err := s.API.CoordinationV1().Leases(s.Namespace).Create(ctx, l, meta.CreateOptions{})
	return err
}
func (s *Service) Provision(ctx context.Context, o Options, r Record) error {
	if err := s.Acquire(ctx, r); err != nil {
		return err
	}
	if err := s.Renew(ctx, o.ExecutionID, r.Owner); err != nil {
		return err
	}
	job := Job(o)
	job.Annotations["wuko.dev/owner"] = r.Owner
	j, err := s.API.BatchV1().Jobs(s.Namespace).Create(ctx, job, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating suspended Job: %w", err)
	}
	if err = s.UpdateRecord(ctx, r.ExecutionID, func(rec *Record) error {
		if rec.Owner != r.Owner {
			return fmt.Errorf("execution owner changed")
		}
		rec.JobUID = string(j.UID)
		return nil
	}); err != nil {
		return err
	}
	if err = s.bindGuard(ctx, r, string(j.UID)); err != nil {
		return err
	}
	workerOptions := o
	workerOptions.Kubeconfig = ""
	workerOptions.Context = ""
	control := Control{Version: 1, Options: workerOptions, Owner: r.Owner, JobUID: string(j.UID), GroupToken: r.GroupToken}
	cm := &core.ConfigMap{ObjectMeta: o.Meta("input"), Immutable: ptr.To(true), Data: map[string]string{"control.json": Encode(control)}}
	if _, err = s.API.CoreV1().ConfigMaps(s.Namespace).Create(ctx, cm, meta.CreateOptions{}); err != nil {
		return err
	}
	if err = s.CheckGuard(ctx, control); err != nil {
		return err
	}
	if err = s.Renew(ctx, o.ExecutionID, r.Owner); err != nil {
		return err
	}
	_, err = s.API.BatchV1().Jobs(s.Namespace).Patch(ctx, j.Name, types.MergePatchType, []byte(`{"spec":{"suspend":false}}`), meta.PatchOptions{})
	return err
}
