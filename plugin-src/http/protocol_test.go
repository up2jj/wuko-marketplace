package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/up2jj/wuko/plugin/sdk"
	"github.com/up2jj/wuko/plugin/sdk/sdktest"
)

type callbackCounts struct {
	validates atomic.Int32
	renders   atomic.Int32
	functions atomic.Int32
}

func startProtocolPlugin(t *testing.T) (*sdktest.Harness, *callbackCounts) {
	t.Helper()
	plugin, err := newPlugin()
	if err != nil {
		t.Fatal(err)
	}
	counts := &callbackCounts{}
	host := sdktest.Start(t, plugin, sdktest.WithCallbacks(sdktest.Callbacks{
		ValidateTemplate: func(context.Context, string) error {
			counts.validates.Add(1)
			return nil
		},
		RenderTemplate: func(_ context.Context, content string, extra map[string]any) (string, error) {
			counts.renders.Add(1)
			if request, ok := extra["request"].(map[string]any); ok {
				content = strings.ReplaceAll(content, "{{ .request.path }}", fmt.Sprint(request["path"]))
			}
			return content, nil
		},
		CallFunction: func(_ context.Context, name string, args []any) (any, error) {
			counts.functions.Add(1)
			switch name {
			case "secret":
				return "secret-value", nil
			case "decorate":
				return fmt.Sprint(args[0]) + "!", nil
			default:
				return nil, fmt.Errorf("unknown function %q", name)
			}
		},
	}))
	return host, counts
}

func TestProtocolDeclaresManagedHTTPServices(t *testing.T) {
	host, _ := startProtocolPlugin(t)

	for _, stepType := range []string{"http.forward_proxy", "http.mock_server"} {
		configuration := map[string]any{}
		if stepType == "http.forward_proxy" {
			configuration["keep_alive"] = true
		} else {
			configuration["expectations"] = []string{"unused.yaml"}
		}
		response := host.CallStep(sdktest.StepRequest{Operation: sdktest.StepService, Type: stepType, With: configuration}).Await()
		if response.Error != nil {
			t.Fatalf("%s service declaration failed: %s", stepType, response.Error.Message)
		}
		var result sdk.Service
		if err := response.DecodeResult(&result); err != nil {
			t.Fatal(err)
		}
		if !result.FailFast || result.Kind == "" || (stepType == "http.forward_proxy" && !result.KeepAlive) {
			t.Fatalf("unexpected %s service policy: %+v", stepType, result)
		}
	}
}

func TestNormalizeProtocolNumbersPreservesIntegerPrecision(t *testing.T) {
	values := normalizeMap(map[string]any{
		"small": json.Number("1024"), "unsigned": json.Number("18446744073709551615"),
		"too_large": json.Number("18446744073709551616"), "decimal": json.Number("1.25"),
	})
	if values["small"] != int(1024) || values["unsigned"] != uint64(18446744073709551615) || values["decimal"] != 1.25 {
		t.Fatalf("normalized numbers = %#v", values)
	}
	if values["too_large"] != json.Number("18446744073709551616") {
		t.Fatalf("large integer lost precision: %#v", values["too_large"])
	}
}

func TestMockServerProtocolLifecycleAndHostCallbacks(t *testing.T) {
	directory := t.TempDir()
	expectation := `version: 1
expectations:
  - name: callback_request
    times: 1
    when: 'request.method == "GET" && request.path == "/probe" && vars.limit == 1024 && secret("fixture") == "secret-value" && decorate("ok") == "ok!"'
    respond:
      body: 'served {{ .request.path }}'
`
	if err := os.WriteFile(filepath.Join(directory, "expectations.yaml"), []byte(expectation), 0o600); err != nil {
		t.Fatal(err)
	}

	host, counts := startProtocolPlugin(t)
	call := host.CallStep(mockRunRequest(directory, "expectations.yaml"))
	ready := call.AwaitEvent("ready")
	var result sdk.Result
	if err := ready.DecodeResult(&result); err != nil {
		t.Fatal(err)
	}
	url, _ := result.Outputs["url"].(string)
	response, err := http.Get(url + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "served /probe" {
		t.Fatalf("unexpected mock response: status=%d body=%q", response.StatusCode, body)
	}
	call.Cancel()
	final := call.Await()
	if final.Error != nil {
		t.Fatalf("service final response failed: %s", final.Error.Message)
	}
	if counts.validates.Load() == 0 || counts.renders.Load() == 0 || counts.functions.Load() < 2 {
		t.Fatalf("callbacks not exercised: validate=%d render=%d function=%d", counts.validates.Load(), counts.renders.Load(), counts.functions.Load())
	}
}

func TestMockServerProtocolPreservesVerificationErrors(t *testing.T) {
	directory := t.TempDir()
	expectation := `version: 1
expectations:
  - name: required_request
    times: 1
    when: 'request.path == "/required"'
    respond: ok
`
	if err := os.WriteFile(filepath.Join(directory, "expectations.yaml"), []byte(expectation), 0o600); err != nil {
		t.Fatal(err)
	}

	host, _ := startProtocolPlugin(t)
	call := host.CallStep(mockRunRequest(directory, "expectations.yaml"))
	_ = call.AwaitEvent("ready")
	call.Cancel()
	final := call.Await()
	if final.Error == nil || !strings.Contains(final.Error.Message, "required_request") {
		t.Fatalf("expected verification error, got %+v", final.Error)
	}
}

func TestProtocolRunsMultipleServiceInstances(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"first", "second"} {
		content := fmt.Sprintf("version: 1\nexpectations:\n  - name: %s\n    times: 1\n    when: 'request.path == \"/%s\"'\n    respond: %s\n", name, name, name)
		if err := os.WriteFile(filepath.Join(directory, name+".yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	host, _ := startProtocolPlugin(t)
	calls := map[string]*sdktest.Call{
		"first":  host.CallStep(mockRunRequest(directory, "first.yaml")),
		"second": host.CallStep(mockRunRequest(directory, "second.yaml")),
	}
	urls := map[string]string{}
	for _, id := range []string{"first", "second"} {
		ready := calls[id].AwaitEvent("ready")
		var result sdk.Result
		if err := ready.DecodeResult(&result); err != nil {
			t.Fatal(err)
		}
		urls[id], _ = result.Outputs["url"].(string)
	}
	for _, id := range []string{"first", "second"} {
		response, err := http.Get(urls[id] + "/" + id)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s returned %d", id, response.StatusCode)
		}
		calls[id].Cancel()
	}
	for _, id := range []string{"first", "second"} {
		response := calls[id].Await()
		if response.Error != nil {
			t.Fatalf("%s failed: %s", id, response.Error.Message)
		}
	}
}

func mockRunRequest(directory, source string) sdktest.StepRequest {
	return sdktest.StepRequest{
		Type: "http.mock_server",
		With: map[string]any{"expectations": []string{source}},
		Context: sdk.StepContext{
			StepID: "mock", WorkflowName: "protocol-test", WorkflowSource: filepath.Join(directory, "wuko.yaml"),
			WorkflowDir: directory, RunDir: directory, Vars: map[string]any{"limit": 1024}, PresetVars: map[string]any{},
			Inputs: map[string]any{}, Env: map[string]string{}, Steps: map[string]any{}, Dependencies: map[string]map[string]any{},
			Bindings: map[string]any{}, Providers: map[string]any{}, Helpers: []string{"decorate"},
		},
	}
}

func TestNormalizeValueRestoresIntegers(t *testing.T) {
	values := normalizeMap(map[string]any{"count": float64(1234567), "ratio": 1.5, "items": []any{float64(2)}})
	if values["count"] != 1234567 || values["ratio"] != 1.5 || values["items"].([]any)[0] != 2 {
		t.Fatalf("unexpected normalized values: %#v", values)
	}
}
