package execution

import (
	"encoding/json"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
)

func options(t *testing.T, id string) Options {
	t.Helper()
	o := Options{Image: "worker:v1", Workflow: "/workflows/test.yaml", ExecutionID: id}
	if err := o.Validate("run"); err != nil {
		t.Fatal(err)
	}
	return o
}
func TestConfigurationAndProtectedInputs(t *testing.T) {
	cases := []struct {
		name, extra string
		invalid     bool
	}{
		{"inline", `"workflow":"","workflow_inline":"version: 1\nsteps: []\n"`, false},
		{"unknown", `"podTemplate":{}`, true},
		{"reserved env", `"env":[{"name":"WUKO_KUBE_POD_UID","value":"x"}]`, true},
		{"reserved prefix", `"envFrom":[{"prefix":"WUKO_KUBE_","secretRef":{"name":"settings"}}]`, true},
		{"unprefixed import", `"envFrom":[{"secretRef":{"name":"settings"}}]`, false},
		{"ambiguous import", `"envFrom":[{"secretRef":{"name":"a"},"configMapRef":{"name":"b"}}]`, true},
		{"references", `"env":[{"name":"TOKEN","valueFrom":{"secretKeyRef":{"name":"credentials","key":"token"}}},{"name":"NODE","valueFrom":{"fieldRef":{"fieldPath":"status.hostIP"}}}]`, false},
		{"unknown field", `"env":[{"name":"FIELD","valueFrom":{"fieldRef":{"fieldPath":"spec.containers"}}}]`, true},
		{"ambiguous ref", `"env":[{"name":"FIELD","valueFrom":{"fieldRef":{"fieldPath":"metadata.name"},"secretKeyRef":{"name":"secret","key":"key"}}}]`, true},
		{"duplicate env", `"env":[{"name":"X"},{"name":"X"}]`, true},
		{"protected mount", `"volumes":[{"name":"bad","mountPath":"/usr/local","configMap":{"name":"x"}}]`, true},
		{"metadata", `"metadata":{"labels":{"team":"platform"},"annotations":{"example.com/context":"test"}}`, false},
		{"internal metadata", `"metadata":{"labels":{"wuko.dev/execution-id":"spoof"}}`, true},
		{"unsafe timing", `"authorization_ttl":"2s"`, true},
		{"wait group", `"concurrency":{"group":"x","on_busy":"wait"}`, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			base := map[string]json.RawMessage{"image": json.RawMessage(`"worker:v1"`), "workflow": json.RawMessage(`"/workflows/test.yaml"`)}
			var extra map[string]json.RawMessage
			if err := json.Unmarshal([]byte("{"+tt.extra+"}"), &extra); err != nil {
				t.Fatal(err)
			}
			for k, v := range extra {
				base[k] = v
			}
			b, _ := json.Marshal(base)
			_, err := Decode(b, "run")
			if (err != nil) != tt.invalid {
				t.Fatalf("Decode: %v", err)
			}
		})
	}
	o := options(t, "test")
	o.Vars = map[string]any{"large": strings.Repeat("x", InputLimit)}
	if o.Validate("run") == nil {
		t.Fatal("accepted oversized input")
	}
}
func TestJobEnvironmentAndMetadata(t *testing.T) {
	o := options(t, "test")
	o.Metadata = Metadata{Labels: map[string]string{"team": "platform"}, Annotations: map[string]string{"example.com/note": "hello"}}
	o.EnvFrom = []core.EnvFromSource{{SecretRef: &core.SecretEnvSource{LocalObjectReference: core.LocalObjectReference{Name: "existing"}}}}
	o.Env = []core.EnvVar{{Name: "LOG_LEVEL", Value: "info"}}
	j := Job(o)
	c := j.Spec.Template.Spec.Containers[0]
	if *j.Spec.Suspend != true || *j.Spec.BackoffLimit != 0 || j.Spec.TTLSecondsAfterFinished != nil || len(j.Spec.Template.Spec.Containers) != 1 {
		t.Fatal("unsafe job defaults")
	}
	if c.Env[len(c.Env)-1].Name != Prefix+"POD_NAME" || c.EnvFrom[0].SecretRef.Name != "existing" {
		t.Fatal("environment imports or authoritative variables lost")
	}
	if j.Labels["team"] != "platform" || j.Spec.Template.Labels[IDLabel] != "test" || j.Spec.Template.Annotations["example.com/note"] != "hello" {
		t.Fatal("metadata not propagated")
	}
	a := o.Digest()
	o.EnvFrom = append(o.EnvFrom, core.EnvFromSource{Prefix: "MORE_", ConfigMapRef: &core.ConfigMapEnvSource{LocalObjectReference: core.LocalObjectReference{Name: "settings"}}})
	if o.Digest() == a {
		t.Fatal("envFrom absent from fingerprint")
	}
	o.ExecutionID = "different"
	b := o.Digest()
	o.ExecutionID = "another"
	if o.Digest() != b {
		t.Fatal("execution ID affects workload fingerprint")
	}
}
func TestGeneratedID(t *testing.T) {
	a, b := RandomID(), RandomID()
	if a == b || len(a) != 35 || !strings.HasPrefix(a, "wk-") {
		t.Fatalf("bad IDs %s %s", a, b)
	}
}
