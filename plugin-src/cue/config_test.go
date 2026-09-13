package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeConfig(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "inline", raw: `{"source":"output: true"}`},
		{name: "file", raw: `{"file":"policy.cue"}`},
		{name: "package", raw: `{"package":"policy"}`},
		{name: "neither", raw: `{}`, wantErr: "exactly one"},
		{name: "source and file", raw: `{"source":"output: true","file":"policy.cue"}`, wantErr: "exactly one"},
		{name: "file and package", raw: `{"file":"policy.cue","package":"policy"}`, wantErr: "exactly one"},
		{name: "empty source", raw: `{"source":""}`, wantErr: "exactly one"},
		{name: "unknown", raw: `{"source":"output: true","extra":true}`, wantErr: "unknown field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeConfig(json.RawMessage(test.raw))
			if test.wantErr == "" && err != nil {
				t.Fatalf("decodeConfig() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("decodeConfig() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestModuleTarget(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name          string
		configuration config
		want          string
		packageMode   bool
		wantErr       string
	}{
		{name: "file", configuration: config{File: "policy/main.cue"}, want: "policy/main.cue"},
		{name: "package", configuration: config{Package: "policy"}, want: "policy", packageMode: true},
		{name: "root package", configuration: config{Package: "."}, want: ".", packageMode: true},
		{name: "absolute file", configuration: config{File: filepath.Join(root, "policy.cue")}, wantErr: "relative"},
		{name: "absolute package", configuration: config{Package: root}, wantErr: "relative"},
		{name: "file traversal", configuration: config{File: "policy/../secret.cue"}, wantErr: "parent traversal"},
		{name: "package traversal", configuration: config{Package: "../policy"}, wantErr: "parent traversal"},
		{name: "extension", configuration: config{File: "policy.yaml"}, wantErr: ".cue extension"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, packageMode, err := moduleTarget(test.configuration)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("moduleTarget() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want || packageMode != test.packageMode {
				t.Fatalf("moduleTarget() = %q, %t, %v", got, packageMode, err)
			}
		})
	}
}

func TestRedactError(t *testing.T) {
	err := redactError(errors.New("token super-secret-value is invalid"), map[string]any{"TOKEN": "super-secret-value"})
	if strings.Contains(err.Error(), "super-secret-value") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("redactError() = %q", err)
	}
}

func TestValidationMustBeDeferred(t *testing.T) {
	for _, configuration := range []config{
		{Source: `output: "{{ .vars.value }}"`},
		{File: `{{ .vars.policy }}`},
		{Package: `{{ .vars.package }}`},
	} {
		if !configuration.validationMustBeDeferred() {
			t.Fatalf("configuration %#v should defer validation", configuration)
		}
	}
	if (config{Package: "policy"}).validationMustBeDeferred() {
		t.Fatal("static package should not defer validation")
	}
}
