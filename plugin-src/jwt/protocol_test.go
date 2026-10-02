package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/up2jj/wuko/plugin/sdk"
	"github.com/up2jj/wuko/plugin/sdk/sdktest"
)

func TestSDKDeclaresHelpersAndSteps(t *testing.T) {
	plugin, err := newPlugin(newService())
	if err != nil {
		t.Fatal(err)
	}
	host := sdktest.Start(t, plugin)
	initialized := host.Initialization()
	if initialized.Protocol != sdk.Protocol || initialized.Namespace != namespace || len(initialized.Steps) != 4 || len(initialized.Helpers) != 4 {
		t.Fatalf("initialize response = %+v", initialized)
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
