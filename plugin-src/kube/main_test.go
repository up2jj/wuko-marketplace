package main

import (
	"testing"

	"github.com/up2jj/wuko-marketplace/plugin-src/kube/execution"
	"github.com/up2jj/wuko/plugin/sdk/sdktest"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func TestProtocolPureInitializationAndValidation(t *testing.T) {
	calls := 0
	p, err := newPlugin(func(execution.Options) (kubernetes.Interface, error) { calls++; return fake.NewClientset(), nil })
	if err != nil {
		t.Fatal(err)
	}
	h := sdktest.Start(t, p)
	if len(h.Initialization().Steps) != 8 || calls != 0 {
		t.Fatal("initialization contacted cluster or omitted steps")
	}
	r := h.CallStep(sdktest.StepRequest{Type: "kube.run", Operation: sdktest.StepValidate, With: map[string]any{"image": "worker:v1", "workflow": "/workflows/test.yaml"}}).Await()
	if r.Error != nil || calls != 0 {
		t.Fatal("validation contacted cluster", r.Error)
	}
	r = h.CallStep(sdktest.StepRequest{Type: "kube.run", Operation: sdktest.StepValidate, With: map[string]any{"image": "worker:v1", "workflow": "/workflows/test.yaml", "env": []any{map[string]any{"name": "WUKO_KUBE_POD_UID", "value": "spoof"}}}}).Await()
	if r.Error == nil || r.Error.Code != "invalid_configuration" {
		t.Fatal("reserved env accepted")
	}
}

func TestTemplateValidationDefersRenderedValues(t *testing.T) {
	calls := 0
	p, err := newPlugin(func(execution.Options) (kubernetes.Interface, error) { calls++; return fake.NewClientset(), nil })
	if err != nil {
		t.Fatal(err)
	}
	h := sdktest.Start(t, p)
	response := h.CallStep(sdktest.StepRequest{Type: "kube.run", Operation: sdktest.StepValidate, With: map[string]any{"namespace": "{{ .vars.namespace }}", "image": "{{ .vars.image }}", "workflow": "{{ .vars.workflow }}"}}).Await()
	if response.Error != nil || calls != 0 {
		t.Fatal("parent templates were rejected or contacted cluster", response.Error)
	}
	response = h.CallStep(sdktest.StepRequest{Type: "kube.run", With: map[string]any{"namespace": "{{ .vars.namespace }}", "image": "worker:v1", "workflow": "/workflows/test.yaml"}}).Await()
	if response.Error == nil || calls != 0 {
		t.Fatal("unresolved run input reached the cluster")
	}
	response = h.CallStep(sdktest.StepRequest{Type: "kube.run", Operation: sdktest.StepValidate, With: map[string]any{"namespace": "{{ .vars.namespace }}", "envFrom": []any{map[string]any{"prefix": "WUKO_KUBE_", "secretRef": map[string]any{"name": "settings"}}}}}).Await()
	if response.Error == nil {
		t.Fatal("template deferred reserved-variable validation")
	}
}
