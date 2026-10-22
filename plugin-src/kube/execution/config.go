// Package execution implements native Kubernetes execution records and fencing.
package execution

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"path"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	Prefix      = "WUKO_KUBE_"
	Managed     = "app.kubernetes.io/managed-by"
	IDLabel     = "wuko.dev/execution-id"
	RoleLabel   = "wuko.dev/role"
	Fingerprint = "wuko.dev/fingerprint"
	Revoked     = "wuko.dev/revoked"
	Sequence    = "wuko.dev/sequence"
	InputLimit  = 256 * 1024
	ReportLimit = 256 * 1024
	ControlPath = "/var/run/wuko/control/control.json"
)

// InternalEnv is the complete registry. Runtime configuration comes from ControlPath.
var InternalEnv = []core.EnvVar{
	{Name: Prefix + "POD_UID", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.uid"}}},
	{Name: Prefix + "POD_NAME", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"}}},
}

type Metadata struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}
type Concurrency struct {
	Group  string `json:"group"`
	OnBusy string `json:"on_busy,omitempty"`
}
type Mount struct {
	Name      string                      `json:"name"`
	MountPath string                      `json:"mountPath"`
	Secret    *core.SecretVolumeSource    `json:"secret,omitempty"`
	ConfigMap *core.ConfigMapVolumeSource `json:"configMap,omitempty"`
}
type Options struct {
	Kubeconfig       string                      `json:"kubeconfig,omitempty"`
	Context          string                      `json:"context,omitempty"`
	Namespace        string                      `json:"namespace,omitempty"`
	ExecutionID      string                      `json:"execution_id,omitempty"`
	Image            string                      `json:"image,omitempty"`
	Workflow         string                      `json:"workflow,omitempty"`
	WorkflowInline   string                      `json:"workflow_inline,omitempty"`
	Target           string                      `json:"target,omitempty"`
	Workdir          string                      `json:"workdir,omitempty"`
	Vars             map[string]any              `json:"vars,omitempty"`
	Env              []core.EnvVar               `json:"env,omitempty"`
	EnvFrom          []core.EnvFromSource        `json:"envFrom,omitempty"`
	Volumes          []Mount                     `json:"volumes,omitempty"`
	Resources        core.ResourceRequirements   `json:"resources,omitempty"`
	ImagePullSecrets []core.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	NodeSelector     map[string]string           `json:"nodeSelector,omitempty"`
	Tolerations      []core.Toleration           `json:"tolerations,omitempty"`
	ServiceAccount   string                      `json:"serviceAccount,omitempty"`
	Metadata         Metadata                    `json:"metadata,omitempty"`
	Concurrency      *Concurrency                `json:"concurrency,omitempty"`
	MaxDuration      string                      `json:"max_duration,omitempty"`
	AuthorizationTTL string                      `json:"authorization_ttl,omitempty"`
	RenewInterval    string                      `json:"renew_interval,omitempty"`
	ShutdownGrace    string                      `json:"shutdown_grace,omitempty"`
	LabelSelector    string                      `json:"label_selector,omitempty"`
	Follow           bool                        `json:"follow,omitempty"`
	TailLines        int64                       `json:"tail_lines,omitempty"`
}

func Decode(data []byte, operation string) (Options, error) {
	var o Options
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(&o); err != nil {
		return o, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return o, fmt.Errorf("expected one configuration object")
	}
	return o, o.Validate(operation)
}
func (o *Options) Validate(op string) error {
	if o.Namespace == "" {
		o.Namespace = "wuko-jobs"
	}
	if errs := validation.IsDNS1123Label(o.Namespace); len(errs) > 0 {
		return fmt.Errorf("namespace: %s", strings.Join(errs, ", "))
	}
	if o.ExecutionID != "" {
		if len(o.ExecutionID) > 48 || len(validation.IsDNS1123Label(o.ExecutionID)) > 0 {
			return fmt.Errorf("execution_id must be a DNS label of at most 48 characters")
		}
	}
	if op == "list" {
		_, err := labels.Parse(o.LabelSelector)
		return err
	}
	if op != "run" {
		if o.ExecutionID == "" {
			return fmt.Errorf("execution_id is required")
		}
		return nil
	}
	if o.Image == "" {
		return fmt.Errorf("image is required")
	}
	if (o.Workflow == "") == (o.WorkflowInline == "") {
		return fmt.Errorf("provide exactly one of workflow or workflow_inline")
	}
	if o.Workflow != "" && !path.IsAbs(o.Workflow) {
		return fmt.Errorf("workflow must be an absolute image-local path")
	}
	if o.Workdir == "" {
		o.Workdir = "/work"
	}
	if !path.IsAbs(o.Workdir) {
		return fmt.Errorf("workdir must be absolute")
	}
	if o.ServiceAccount == "" {
		o.ServiceAccount = "wuko-worker"
	}
	if o.MaxDuration == "" {
		o.MaxDuration = "30m"
	}
	if o.AuthorizationTTL == "" {
		o.AuthorizationTTL = "15s"
	}
	if o.RenewInterval == "" {
		o.RenewInterval = "3s"
	}
	if o.ShutdownGrace == "" {
		o.ShutdownGrace = "5s"
	}
	max, ttl, renew, grace, err := o.Durations()
	if err != nil {
		return err
	}
	if ttl < 3*renew || renew < time.Second || ttl > time.Minute || grace > 30*time.Second || max > 24*time.Hour {
		return fmt.Errorf("require renew_interval >= 1s, 3*renew_interval <= authorization_ttl <= 1m, shutdown_grace <= 30s, max_duration <= 24h")
	}
	if o.Concurrency != nil {
		if o.Concurrency.Group == "" || len(o.Concurrency.Group) > 512 {
			return fmt.Errorf("concurrency.group must contain 1..512 bytes")
		}
		if o.Concurrency.OnBusy == "" {
			o.Concurrency.OnBusy = "error"
		}
		if o.Concurrency.OnBusy != "error" {
			return fmt.Errorf("only concurrency.on_busy: error is supported")
		}
	}
	for _, m := range []map[string]string{o.Metadata.Labels, o.Metadata.Annotations} {
		for k := range m {
			if strings.HasPrefix(k, "wuko.dev/") || k == Managed {
				return fmt.Errorf("reserved metadata key %s", k)
			}
			if len(validation.IsQualifiedName(k)) > 0 {
				return fmt.Errorf("invalid metadata key %s", k)
			}
		}
	}
	if errs := validation.IsValidLabelValue(o.ServiceAccount); len(errs) > 0 {
		return fmt.Errorf("invalid serviceAccount")
	}
	for _, v := range o.Metadata.Labels {
		if len(validation.IsValidLabelValue(v)) > 0 {
			return fmt.Errorf("invalid label value")
		}
	}
	var annotationBytes int
	for k, v := range o.Metadata.Annotations {
		annotationBytes += len(k) + len(v)
	}
	if annotationBytes > 256*1024-512 {
		return fmt.Errorf("annotations exceed 256 KiB")
	}
	names := map[string]bool{}
	for _, e := range o.Env {
		if names[e.Name] || strings.HasPrefix(e.Name, Prefix) || len(validation.IsEnvVarName(e.Name)) > 0 {
			return fmt.Errorf("invalid, duplicate or reserved environment name %q", e.Name)
		}
		names[e.Name] = true
		if e.ValueFrom == nil {
			continue
		}
		s := e.ValueFrom
		n := 0
		if s.SecretKeyRef != nil {
			n++
			if s.SecretKeyRef.Name == "" || s.SecretKeyRef.Key == "" {
				return fmt.Errorf("secretKeyRef requires name and key")
			}
		}
		if s.ConfigMapKeyRef != nil {
			n++
			if s.ConfigMapKeyRef.Name == "" || s.ConfigMapKeyRef.Key == "" {
				return fmt.Errorf("configMapKeyRef requires name and key")
			}
		}
		if s.FieldRef != nil {
			n++
			if s.FieldRef.APIVersion == "" {
				s.FieldRef.APIVersion = "v1"
			}
			if s.FieldRef.APIVersion != "v1" || !validField(s.FieldRef.FieldPath) {
				return fmt.Errorf("unsupported fieldRef %q", s.FieldRef.FieldPath)
			}
		}
		if n != 1 || s.ResourceFieldRef != nil || s.FileKeyRef != nil || e.Value != "" {
			return fmt.Errorf("env valueFrom requires exactly one supported reference and no value")
		}
	}
	for _, s := range o.EnvFrom {
		if (s.SecretRef == nil) == (s.ConfigMapRef == nil) {
			return fmt.Errorf("envFrom requires exactly one secretRef or configMapRef")
		}
		if strings.HasPrefix(s.Prefix, Prefix) {
			return fmt.Errorf("reserved envFrom prefix")
		}
		if s.Prefix != "" && len(validation.IsEnvVarName(s.Prefix)) > 0 {
			return fmt.Errorf("invalid envFrom prefix")
		}
		name := ""
		if s.SecretRef != nil {
			name = s.SecretRef.Name
		} else {
			name = s.ConfigMapRef.Name
		}
		if len(validation.IsDNS1123Subdomain(name)) > 0 {
			return fmt.Errorf("invalid envFrom source name")
		}
	}
	mounts := map[string]bool{}
	for _, m := range o.Volumes {
		if len(validation.IsDNS1123Label(m.Name)) > 0 || mounts[m.Name] || m.Name == "control" || m.Name == "runtime" {
			return fmt.Errorf("invalid or duplicate volume name")
		}
		mounts[m.Name] = true
		if !path.IsAbs(m.MountPath) || overlaps(m.MountPath, "/var/run") || overlaps(m.MountPath, "/usr/local/bin") || overlaps(m.MountPath, "/etc/ssl") {
			return fmt.Errorf("protected volume mount path %q", m.MountPath)
		}
		if (m.Secret == nil) == (m.ConfigMap == nil) {
			return fmt.Errorf("volume requires exactly one secret or configMap")
		}
	}
	if o.WorkflowInline != "" {
		var doc map[string]any
		if err := yaml.UnmarshalStrict([]byte(o.WorkflowInline), &doc); err != nil {
			return fmt.Errorf("workflow_inline YAML: %w", err)
		}
		if doc["version"] != float64(1) || doc["steps"] == nil {
			return fmt.Errorf("workflow_inline requires version: 1 and steps")
		}
	}
	data, _ := json.Marshal(o)
	if len(data) > InputLimit {
		return fmt.Errorf("input exceeds 256 KiB")
	}
	return nil
}
func overlaps(a, b string) bool {
	a = path.Clean(a)
	return a == "/" || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func validField(s string) bool {
	switch s {
	case "metadata.name", "metadata.namespace", "metadata.uid", "spec.nodeName", "spec.serviceAccountName", "status.hostIP", "status.hostIPs", "status.podIP", "status.podIPs":
		return true
	}
	for _, p := range []string{"metadata.labels['", "metadata.annotations['"} {
		if strings.HasPrefix(s, p) && strings.HasSuffix(s, "']") {
			return len(validation.IsQualifiedName(strings.TrimSuffix(strings.TrimPrefix(s, p), "']"))) == 0
		}
	}
	return false
}
func (o Options) Durations() (time.Duration, time.Duration, time.Duration, time.Duration, error) {
	var v [4]time.Duration
	for i, s := range []string{o.MaxDuration, o.AuthorizationTTL, o.RenewInterval, o.ShutdownGrace} {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return 0, 0, 0, 0, fmt.Errorf("durations must be positive Go durations")
		}
		v[i] = d
	}
	return v[0], v[1], v[2], v[3], nil
}
func RandomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "wk-" + hex.EncodeToString(b[:])
}
func Name(id, role string) string { return id + "-" + role }
func GroupName(group string) string {
	h := sha256.Sum256([]byte(group))
	return "wk-group-" + hex.EncodeToString(h[:20])
}
func (o Options) Digest() string {
	o.Kubeconfig = ""
	o.Context = ""
	o.ExecutionID = ""
	b, _ := json.Marshal(o)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (o Options) Meta(role string) meta.ObjectMeta {
	l := maps.Clone(o.Metadata.Labels)
	if l == nil {
		l = map[string]string{}
	}
	l[Managed] = "wuko-kube"
	l[IDLabel] = o.ExecutionID
	l[RoleLabel] = role
	a := maps.Clone(o.Metadata.Annotations)
	if a == nil {
		a = map[string]string{}
	}
	a[Fingerprint] = o.Digest()
	return meta.ObjectMeta{Name: Name(o.ExecutionID, role), Namespace: o.Namespace, Labels: l, Annotations: a}
}

type Control struct {
	Version    int     `json:"version"`
	Options    Options `json:"options"`
	Owner      string  `json:"owner"`
	JobUID     string  `json:"job_uid"`
	GroupToken string  `json:"group_token,omitempty"`
}
type Record struct {
	Version     int             `json:"version"`
	ExecutionID string          `json:"execution_id"`
	Fingerprint string          `json:"fingerprint"`
	Owner       string          `json:"owner"`
	Group       string          `json:"group,omitempty"`
	GroupToken  string          `json:"group_token,omitempty"`
	JobUID      string          `json:"job_uid,omitempty"`
	PodUID      string          `json:"pod_uid,omitempty"`
	State       string          `json:"state"`
	ReportState string          `json:"report_state"`
	Report      json.RawMessage `json:"report,omitempty"`
	ExitCode    int             `json:"exit_code"`
	Reason      string          `json:"reason,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

func Encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func ReadRecord(cm *core.ConfigMap) (Record, error) {
	var r Record
	err := json.Unmarshal([]byte(cm.Data["record.json"]), &r)
	if err != nil {
		return r, fmt.Errorf("invalid execution record: %w", err)
	}
	if r.Version != 1 {
		return r, fmt.Errorf("unsupported execution record version")
	}
	return r, nil
}

// ValidateConfiguration accepts unresolved parent templates during Wuko's
// validation phase. Run always calls Decode again with rendered values before
// any Kubernetes request. Unknown fields and reserved settings remain errors.
func ValidateConfiguration(data []byte, operation string) error {
	if !bytes.Contains(data, []byte("{{")) {
		_, err := Decode(data, operation)
		return err
	}
	var o Options
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(&o); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one configuration object")
	}
	if len(data) > InputLimit {
		return fmt.Errorf("input exceeds 256 KiB")
	}
	for _, e := range o.Env {
		if strings.HasPrefix(e.Name, Prefix) {
			return fmt.Errorf("reserved environment name %q", e.Name)
		}
	}
	for _, e := range o.EnvFrom {
		if strings.HasPrefix(e.Prefix, Prefix) {
			return fmt.Errorf("reserved envFrom prefix")
		}
	}
	for _, m := range []map[string]string{o.Metadata.Labels, o.Metadata.Annotations} {
		for k := range m {
			if strings.HasPrefix(k, "wuko.dev/") || k == Managed {
				return fmt.Errorf("reserved metadata key %s", k)
			}
		}
	}
	return nil
}
