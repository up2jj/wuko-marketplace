package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEvaluateRuntimeContext(t *testing.T) {
	job := workerJob{
		Mode: workerModeRun,
		Configuration: config{Source: `package example

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
	}`},
		Context: stepContext{
			WorkflowName: "release", StepID: "plan", Attempt: 2, MaxAttempts: 3,
			Vars: map[string]any{"environment": "STAGING", "replicas": json.Number("3"), "regions": []any{"eu", "us"}},
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
		Mode:          workerModeValidate,
		Configuration: config{Source: `output: wuko.steps.previous.value`},
		Context:       stepContext{Steps: map[string]any{}},
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
		{name: "tool package", source: "import \"tool/file\"\noutput: true\n", wantErr: "tool packages are not supported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := evaluate(workerJob{Mode: workerModeRun, Configuration: config{Source: test.source}})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("evaluate() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestServeWorker(t *testing.T) {
	job := workerJob{
		Mode:          workerModeRun,
		Configuration: config{Source: `output: {account_id: wuko.vars.account_id}`},
		Context:       stepContext{Vars: map[string]any{"account_id": json.Number("9007199254740993")}},
	}
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
	if response.Error != "" || string(response.Value) != `{"account_id":9007199254740993}` {
		t.Fatalf("serveWorker() response = %#v", response)
	}
}

func TestEvaluatePreservesLargeInteger(t *testing.T) {
	data, err := evaluate(workerJob{Mode: workerModeRun, Configuration: config{Source: `output: 9007199254740993`}})
	if err != nil {
		t.Fatalf("evaluate() error = %v", err)
	}
	if string(data) != "9007199254740993" {
		t.Fatalf("evaluate() = %s", data)
	}
}

func TestEvaluateModuleAwareFile(t *testing.T) {
	root := t.TempDir()
	writeCUEFile(t, root, "cue.mod/module.cue", `module: "example.com/workflow@v0"
language: version: "v0.17.0"
`)
	writeCUEFile(t, root, "schema/deployment.cue", `package schema

#Deployment: {
	name: string
	replicas: int & >=1
}
`)
	writeCUEFile(t, root, "policy.cue", `package policy

import "example.com/workflow/schema"

output: schema.#Deployment & {
	name: "api"
	replicas: wuko.vars.replicas
}
`)
	data, err := evaluate(workerJob{
		Mode:          workerModeRun,
		Configuration: config{File: "policy.cue"},
		Context:       stepContext{WorkflowDir: root, Vars: map[string]any{"replicas": json.Number("3")}},
	})
	if err != nil {
		t.Fatalf("evaluate() error = %v", err)
	}
	if string(data) != `{"name":"api","replicas":3}` {
		t.Fatalf("evaluate() = %s", data)
	}
}

func TestEvaluateStandaloneFile(t *testing.T) {
	root := t.TempDir()
	writeCUEFile(t, root, "policy.cue", "output: {ok: true}\n")
	data, err := evaluate(workerJob{
		Mode:          workerModeRun,
		Configuration: config{File: "policy.cue"},
		Context:       stepContext{WorkflowDir: root},
	})
	if err != nil {
		t.Fatalf("evaluate() error = %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Fatalf("evaluate() = %s", data)
	}
}

func TestEvaluateMultiFilePackage(t *testing.T) {
	root := t.TempDir()
	writeCUEFile(t, root, "cue.mod/module.cue", `module: "example.com/workflow@v0"
language: version: "v0.17.0"
`)
	writeCUEFile(t, root, "policy/candidate.cue", `package policy

candidate: {
	name: "api-\(wuko.vars.environment)"
	replicas: int & >=1 & wuko.vars.replicas
}
`)
	writeCUEFile(t, root, "policy/output.cue", `package policy

output: candidate & {approved: true}
`)
	data, err := evaluate(workerJob{
		Mode:          workerModeRun,
		Configuration: config{Package: "policy"},
		Context: stepContext{WorkflowDir: root, Vars: map[string]any{
			"environment": "staging", "replicas": json.Number("3"),
		}},
	})
	if err != nil {
		t.Fatalf("evaluate() error = %v", err)
	}
	if string(data) != `{"approved":true,"name":"api-staging","replicas":3}` {
		t.Fatalf("evaluate() = %s", data)
	}
}

func TestEvaluateModuleBuiltInImportAndLanguageVersion(t *testing.T) {
	root := t.TempDir()
	writeCUEFile(t, root, "cue.mod/module.cue", `module: "example.com/workflow@v0"
language: version: "v0.9.0"
`)
	writeCUEFile(t, root, "policy/policy.cue", `package policy

import "strings"

output: strings.ToLower("API")
`)
	data, err := evaluate(workerJob{
		Mode:          workerModeRun,
		Configuration: config{Package: "policy"},
		Context:       stepContext{WorkflowDir: root},
	})
	if err != nil {
		t.Fatalf("evaluate() error = %v", err)
	}
	if string(data) != `"api"` {
		t.Fatalf("evaluate() = %s", data)
	}
}

func TestEvaluateModuleErrors(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string) config
		wantErr string
	}{
		{
			name: "missing package",
			prepare: func(_ *testing.T, _ string) config {
				return config{Package: "missing"}
			},
			wantErr: "cannot find package",
		},
		{
			name: "external dependency",
			prepare: func(t *testing.T, root string) config {
				writeCUEFile(t, root, "cue.mod/module.cue", `module: "example.com/workflow@v0"
language: version: "v0.17.0"
`)
				writeCUEFile(t, root, "policy.cue", "package policy\nimport external \"registry.example/external@v0\"\noutput: external.value\n")
				return config{File: "policy.cue"}
			},
			wantErr: "external CUE module dependencies are disabled",
		},
		{
			name: "tool import in package",
			prepare: func(t *testing.T, root string) config {
				writeCUEFile(t, root, "policy/policy.cue", "package policy\nimport \"tool/file\"\noutput: true\n")
				return config{Package: "policy"}
			},
			wantErr: "tool packages are not supported",
		},
		{
			name: "ambiguous package",
			prepare: func(t *testing.T, root string) config {
				writeCUEFile(t, root, "policy/one.cue", "package one\noutput: true\n")
				writeCUEFile(t, root, "policy/two.cue", "package two\noutput: true\n")
				return config{Package: "policy"}
			},
			wantErr: "found packages",
		},
		{
			name: "oversized file",
			prepare: func(t *testing.T, root string) config {
				writeCUEFile(t, root, "large.cue", strings.Repeat(" ", maxSourceSize+1))
				return config{File: "large.cue"}
			},
			wantErr: "1 MiB",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			configuration := test.prepare(t, root)
			_, err := evaluate(workerJob{Mode: workerModeRun, Configuration: configuration, Context: stepContext{WorkflowDir: root}})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("evaluate() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestEvaluateRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeCUEFile(t, outside, "secret.cue", "output: true\n")
	if err := os.Symlink(filepath.Join(outside, "secret.cue"), filepath.Join(root, "policy.cue")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	_, err := evaluate(workerJob{Mode: workerModeRun, Configuration: config{File: "policy.cue"}, Context: stepContext{WorkflowDir: root}})
	if err == nil {
		t.Fatal("evaluate() followed a symlink outside the workflow directory")
	}
}

func TestBoundedFSAggregateLimit(t *testing.T) {
	files := make(fstest.MapFS)
	for index := range 11 {
		files[string(rune('a'+index))] = &fstest.MapFile{Data: bytes.Repeat([]byte("x"), maxSourceSize)}
	}
	bounded := newBoundedFS(files)
	for index := range 10 {
		file, err := bounded.Open(string(rune('a' + index)))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		_ = file.Close()
	}
	_, err := bounded.Open("k")
	if err == nil || !strings.Contains(err.Error(), "10 MiB") {
		t.Fatalf("Open() error = %v", err)
	}
}

func writeCUEFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluateRejectsOversizedOutput(t *testing.T) {
	err := validateOutputSize(make([]byte, maxOutputSize+1))
	if err == nil || !strings.Contains(err.Error(), "protocol limit") {
		t.Fatalf("evaluate() error = %v", err)
	}
}
