package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/up2jj/wuko/plugin/sdk"
)

func TestSDKDeclaresHelpersAndSteps(t *testing.T) {
	plugin, err := newPlugin(newService())
	if err != nil {
		t.Fatal(err)
	}
	pluginInput, hostInput := io.Pipe()
	hostOutput, pluginOutput := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- plugin.Serve(context.Background(), pluginInput, pluginOutput) }()
	encoder := json.NewEncoder(hostInput)
	frames := bufio.NewScanner(hostOutput)
	if err := encoder.Encode(map[string]any{
		"id": "init", "method": "initialize", "params": map[string]any{"protocol": sdk.Protocol},
	}); err != nil {
		t.Fatal(err)
	}
	if !frames.Scan() {
		t.Fatalf("initialize response: %v", frames.Err())
	}
	var initialized struct {
		Result struct {
			Protocol  string              `json:"protocol"`
			Namespace string              `json:"namespace"`
			Steps     []map[string]any    `json:"steps"`
			Helpers   []map[string]string `json:"helpers"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(frames.Bytes(), &initialized); err != nil {
		t.Fatal(err)
	}
	if initialized.Error != nil || initialized.Result.Protocol != sdk.Protocol || initialized.Result.Namespace != namespace || len(initialized.Result.Steps) != 4 || len(initialized.Result.Helpers) != 4 {
		t.Fatalf("initialize response = %+v", initialized)
	}
	if err := encoder.Encode(map[string]any{"id": "shutdown", "method": "shutdown", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if !frames.Scan() {
		t.Fatalf("shutdown response: %v", frames.Err())
	}
	if err := hostInput.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHelperAndStepShareStateAndSemantics(t *testing.T) {
	adapter := &pluginAdapter{service: newService()}
	helperResult, err := adapter.callHelper(t.Context(), helperParams{
		Name: "generate", Args: []json.RawMessage{json.RawMessage(`{"sub":"helper"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	token := helperResult.(string)
	configuration, err := json.Marshal(map[string]any{"token": token, "subject": "helper"})
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := adapter.callStep(t.Context(), "jwt.verify", configuration, false)
	if err != nil {
		t.Fatalf("step verify error = %v", err)
	}
	claims := outputs["claims"].(map[string]any)
	if claims["sub"] != "helper" {
		t.Fatalf("claims = %#v", claims)
	}

	outputs, err = adapter.callStep(t.Context(), "jwt.is_valid", json.RawMessage(`{"token":"bad"}`), false)
	if err != nil || outputs["valid"] != false {
		t.Fatalf("is_valid outputs = %#v, error = %v", outputs, err)
	}
}

func TestStrictStepConfigurationAndDeferredTemplate(t *testing.T) {
	adapter := &pluginAdapter{service: newService()}
	_, err := adapter.callStep(t.Context(), "jwt.generate", json.RawMessage(`{"unknown":true}`), true)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	_, err = adapter.callStep(t.Context(), "jwt.verify", json.RawMessage(`{"token":"{{ .vars.token }}","jwks_url":"{{ .vars.url }}"}`), true)
	if err != nil {
		t.Fatalf("templated validation error = %v", err)
	}
}

func TestOptionCollectionsRejectEmptyAndDuplicateValues(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"audience":["api",""]}`),
		json.RawMessage(`{"audience":["api","api"]}`),
		json.RawMessage(`{"required_claims":["exp","exp"]}`),
	} {
		if _, err := decodeVerifyOptions(raw); err == nil {
			t.Fatalf("decodeVerifyOptions(%s) succeeded", raw)
		}
	}
}

func TestIsValidPreservesOperationalErrors(t *testing.T) {
	adapter := &pluginAdapter{service: newService()}
	_, err := adapter.callHelper(t.Context(), helperParams{
		Name: "is_valid",
		Args: []json.RawMessage{json.RawMessage(`"bad"`), json.RawMessage(`{"jwks_url":"http://example.com/keys"}`)},
	})
	if err == nil || isTokenError(err) {
		t.Fatalf("configuration error = %T %v", err, err)
	}
}
