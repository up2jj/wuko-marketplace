package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeConfig(t *testing.T) {
	tests := []struct {
		name           string
		raw            string
		allowTemplates bool
		wantDelivery   string
		wantErr        string
	}{
		{name: "message only", raw: `{"message":"done"}`, wantDelivery: deliveryAuto},
		{name: "terminal", raw: `{"message":"done","title":"Build","delivery":"terminal"}`, wantDelivery: deliveryTerminal},
		{name: "missing message", raw: `{}`, wantErr: "message is required"},
		{name: "blank message", raw: `{"message":"  "}`, wantErr: "message is required"},
		{name: "blank title", raw: `{"message":"done","title":" "}`, wantErr: "title must not be empty"},
		{name: "bad delivery", raw: `{"message":"done","delivery":"desktop"}`, wantErr: "delivery must be"},
		{name: "unknown field", raw: `{"message":"done","sound":"Ping"}`, wantErr: "unknown field"},
		{name: "templated delivery during validation", raw: `{"message":"{{ .vars.message }}","title":"{{ .vars.title }}","delivery":"{{ .vars.delivery }}"}`, allowTemplates: true, wantDelivery: "{{ .vars.delivery }}"},
		{name: "templated delivery after rendering", raw: `{"message":"done","delivery":"{{ .vars.delivery }}"}`, wantErr: "delivery must be"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configuration, err := decodeConfig(json.RawMessage(test.raw), test.allowTemplates)
			if test.wantErr == "" && err != nil {
				t.Fatalf("decodeConfig() error = %v", err)
			}
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("decodeConfig() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if configuration.Delivery != test.wantDelivery {
				t.Fatalf("delivery = %q, want %q", configuration.Delivery, test.wantDelivery)
			}
		})
	}
}

func TestResolvedTitle(t *testing.T) {
	explicit := "Release"
	tests := []struct {
		name          string
		configuration config
		workflowName  string
		want          string
	}{
		{name: "explicit", configuration: config{Title: &explicit}, workflowName: "deploy", want: "Release"},
		{name: "workflow", workflowName: "deploy", want: "deploy"},
		{name: "wuko fallback", want: "Wuko"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.configuration.resolvedTitle(test.workflowName); got != test.want {
				t.Fatalf("resolvedTitle() = %q, want %q", got, test.want)
			}
		})
	}
}
