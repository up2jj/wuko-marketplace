package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInitializeDeclaresHelpersAndSteps(t *testing.T) {
	s := &server{service: newService()}
	result, err := s.dispatch(t.Context(), request{Method: "initialize", Params: json.RawMessage(`{"protocol":"wuko.plugin/v1","host_version":"v0.14.0"}`)})
	if err != nil {
		t.Fatal(err)
	}
	declaration := result.(map[string]any)
	if declaration["namespace"] != "jwt" || len(declaration["helpers"].([]any)) != 4 || len(declaration["steps"].([]any)) != 4 {
		t.Fatalf("declaration = %#v", declaration)
	}
	if !s.runtimeAllowed.Load() {
		t.Fatal("compatible host did not enable runtime")
	}
}

func TestVersionlessHandshakeIsPure(t *testing.T) {
	s := &server{service: newService()}
	if _, err := s.dispatch(t.Context(), request{Method: "initialize", Params: json.RawMessage(`{"protocol":"wuko.plugin/v1"}`)}); err != nil {
		t.Fatal(err)
	}
	if s.service.key != nil {
		t.Fatal("initialize generated a signing key")
	}
	_, err := s.dispatch(t.Context(), request{Method: "helper.call", Params: json.RawMessage(`{"name":"jwks","args":[]}`)})
	if err == nil || !strings.Contains(err.Error(), minHostVersion) {
		t.Fatalf("runtime error = %v", err)
	}
}

func TestHelperAndStepShareStateAndSemantics(t *testing.T) {
	s := &server{service: newService()}
	s.runtimeAllowed.Store(true)
	helperResult, err := s.dispatch(t.Context(), request{
		Method: "helper.call",
		Params: json.RawMessage(`{"name":"generate","args":[{"sub":"helper"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	token := helperResult.(map[string]any)["value"].(string)
	config, _ := json.Marshal(map[string]any{"token": token, "subject": "helper"})
	stepResult, err := s.dispatch(t.Context(), request{
		Method: "step.run",
		Params: mustJSON(t, map[string]any{"type": "jwt.verify", "with": json.RawMessage(config), "context": map[string]any{}}),
	})
	if err != nil {
		t.Fatalf("step verify error = %v", err)
	}
	claims := stepResult.(map[string]any)["outputs"].(map[string]any)["claims"].(map[string]any)
	if claims["sub"] != "helper" {
		t.Fatalf("claims = %#v", claims)
	}

	invalidResult, err := s.dispatch(t.Context(), request{
		Method: "step.run",
		Params: json.RawMessage(`{"type":"jwt.is_valid","with":{"token":"bad"},"context":{}}`),
	})
	if err != nil {
		t.Fatalf("is_valid error = %v", err)
	}
	if invalidResult.(map[string]any)["outputs"].(map[string]any)["valid"] != false {
		t.Fatalf("is_valid result = %#v", invalidResult)
	}
}

func TestStrictStepConfigurationAndDeferredTemplate(t *testing.T) {
	s := &server{service: newService()}
	s.runtimeAllowed.Store(true)
	_, err := s.dispatch(t.Context(), request{
		Method: "step.validate",
		Params: json.RawMessage(`{"type":"jwt.generate","with":{"unknown":true},"context":{}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	_, err = s.dispatch(t.Context(), request{
		Method: "step.validate",
		Params: json.RawMessage(`{"type":"jwt.verify","with":{"token":"{{ .vars.token }}","jwks_url":"{{ .vars.url }}"},"context":{}}`),
	})
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

func TestProtocolRejectsDuplicateMembers(t *testing.T) {
	var request request
	err := decodeProtocolFrame([]byte(`{"id":"1","id":"2","method":"initialize","params":{}}`), &request)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate protocol member error = %v", err)
	}
}

func TestIsValidPreservesOperationalErrors(t *testing.T) {
	s := &server{service: newService()}
	s.runtimeAllowed.Store(true)
	_, err := s.callHelper(t.Context(), helperParams{
		Name: "is_valid",
		Args: []json.RawMessage{json.RawMessage(`"bad"`), json.RawMessage(`{"jwks_url":"http://example.com/keys"}`)},
	})
	if err == nil || isTokenError(err) {
		t.Fatalf("configuration error = %T %v", err, err)
	}
}

func TestHostVersionCompatibility(t *testing.T) {
	for _, test := range []struct {
		version string
		allowed bool
		wantErr bool
	}{
		{version: "", allowed: false},
		{version: "dev", allowed: true},
		{version: "v0.14.0", allowed: true},
		{version: "0.15.0", allowed: true},
		{version: "v0.14.0+build.7", allowed: true},
		{version: "v0.14.0-rc.1", allowed: true},
		{version: "v0.13.0-70-gabc", wantErr: true},
		{version: "next"},
	} {
		t.Run(test.version, func(t *testing.T) {
			allowed, err := validateHostVersion(test.version)
			if allowed != test.allowed || (err != nil) != test.wantErr {
				t.Fatalf("validateHostVersion(%q) = %t, %v", test.version, allowed, err)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
