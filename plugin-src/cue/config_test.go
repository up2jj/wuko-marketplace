package main

import (
	"encoding/json"
	"errors"
	"os"
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
		{name: "neither", raw: `{}`, wantErr: "exactly one"},
		{name: "both", raw: `{"source":"output: true","file":"policy.cue"}`, wantErr: "exactly one"},
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

func TestLoadSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "policy.cue"), []byte("output: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, filename, err := loadSource(config{File: "policy.cue"}, root)
	if err != nil {
		t.Fatalf("loadSource() error = %v", err)
	}
	if source != "output: true\n" || filename != "policy.cue" {
		t.Fatalf("loadSource() = %q, %q", source, filename)
	}
	if err := os.WriteFile(filepath.Join(root, "large.cue"), []byte(strings.Repeat("x", maxSourceSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory.cue"), 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		configuration config
		wantErr       string
	}{
		{name: "absolute", configuration: config{File: filepath.Join(root, "policy.cue")}, wantErr: "relative"},
		{name: "parent traversal", configuration: config{File: "../policy.cue"}, wantErr: "parent traversal"},
		{name: "extension", configuration: config{File: "policy.yaml"}, wantErr: ".cue extension"},
		{name: "large inline", configuration: config{Source: strings.Repeat("x", maxSourceSize+1)}, wantErr: "1 MiB"},
		{name: "large file", configuration: config{File: "large.cue"}, wantErr: "1 MiB"},
		{name: "non regular", configuration: config{File: "directory.cue"}, wantErr: "not a regular file"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := loadSource(test.configuration, root)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("loadSource() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadSourceRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.cue")
	if err := os.WriteFile(target, []byte("output: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "policy.cue")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	_, _, err := loadSource(config{File: "policy.cue"}, root)
	if err == nil || !strings.Contains(err.Error(), "outside the workflow directory") {
		t.Fatalf("loadSource() error = %v", err)
	}
}

func TestRedactError(t *testing.T) {
	err := redactError(errors.New("token super-secret-value is invalid"), map[string]any{"TOKEN": "super-secret-value"})
	if strings.Contains(err.Error(), "super-secret-value") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("redactError() = %q", err)
	}
}

func TestValidationMustBeDeferred(t *testing.T) {
	if !(config{Source: `output: "{{ .vars.value }}"`}).validationMustBeDeferred() {
		t.Fatal("inline template should defer validation")
	}
	if !(config{File: `{{ .vars.policy }}`}).validationMustBeDeferred() {
		t.Fatal("templated file should defer validation")
	}
	if (config{File: "policy.cue"}).validationMustBeDeferred() {
		t.Fatal("static file should not defer validation")
	}
}
