package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvaluateRuntimeContext(t *testing.T) {
	job := workerJob{
		Mode:     workerModeRun,
		Filename: "deployment.cue",
		Source: `package example

import "strings"

#Deployment: {
	name: string
	replicas: int & >=1 & <=10
	labels: {managed_by: *"wuko" | string}
}

plan: #Deployment & {
	name: strings.ToLower("API-\(wuko.vars.environment)")
	replicas: wuko.vars.replicas
}

output: {
	deployment: plan
	targets: [for item in wuko.vars.regions {
		region: item
		service: plan.name
	}]
	metadata: {
		workflow: wuko.workflow.name
		step: wuko.step.id
		attempt: wuko.step.attempt
	}
}`,
		Context: stepContext{
			WorkflowName: "release", StepID: "plan", Attempt: 2, MaxAttempts: 3,
			// Protocol JSON decodes an untyped workflow integer as float64. Compiling the
			// JSON scope must recover JSON integer semantics before CUE sees it.
			Vars: map[string]any{"environment": "STAGING", "replicas": float64(3), "regions": []any{"eu", "us"}},
		},
	}
	data, err := evaluate(job)
	if err != nil {
		t.Fatalf("evaluate() error = %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatal(err)
	}
	deployment := output["deployment"].(map[string]any)
	if deployment["name"] != "api-staging" || deployment["replicas"] != float64(3) {
		t.Fatalf("evaluate() deployment = %#v", deployment)
	}
	labels := deployment["labels"].(map[string]any)
	if labels["managed_by"] != "wuko" {
		t.Fatalf("evaluate() labels = %#v", labels)
	}
	targets := output["targets"].([]any)
	if len(targets) != 2 || targets[1].(map[string]any)["region"] != "us" {
		t.Fatalf("evaluate() targets = %#v", targets)
	}
}

func TestEvaluateValidationAllowsFutureStepOutput(t *testing.T) {
	_, err := evaluate(workerJob{
		Mode: workerModeValidate, Filename: "policy.cue",
		Source:  `output: wuko.steps.previous.value`,
		Context: stepContext{Steps: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("evaluate() validation error = %v", err)
	}
}

func TestEvaluateErrors(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantErr string
	}{
		{name: "missing output", source: `value: true`, wantErr: "top-level field output is required"},
		{name: "incomplete output", source: `output: string`, wantErr: "incomplete value"},
		{name: "conflict", source: "output: 1 & 2\n", wantErr: "inline.cue:1"},
		{name: "unsafe integer", source: `output: 9007199254740992`, wantErr: "safe integer range"},
		{name: "tool package", source: "import \"tool/file\"\noutput: true\n", wantErr: "tool packages are not supported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := evaluate(workerJob{Mode: workerModeRun, Filename: "inline.cue", Source: test.source})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("evaluate() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestServeWorker(t *testing.T) {
	job := workerJob{Mode: workerModeRun, Filename: "inline.cue", Source: `output: {ok: true}`}
	request, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := serveWorker(strings.NewReader(string(request)), &output); err != nil {
		t.Fatalf("serveWorker() error = %v", err)
	}
	var response workerResponse
	if err := json.Unmarshal([]byte(output.String()), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "" || string(response.Value) != `{"ok":true}` {
		t.Fatalf("serveWorker() response = %#v", response)
	}
}

func TestEvaluateRejectsOversizedOutput(t *testing.T) {
	err := validateOutputSize(make([]byte, maxOutputSize+1))
	if err == nil || !strings.Contains(err.Error(), "protocol limit") {
		t.Fatalf("evaluate() error = %v", err)
	}
}
