package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type protocolHarness struct {
	input     *io.PipeWriter
	frames    chan testFrame
	done      chan error
	writeMu   sync.Mutex
	validates atomic.Int32
	renders   atomic.Int32
	functions atomic.Int32
}

type testFrame struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Event  string          `json:"event"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *wireError      `json:"error"`
}

func newProtocolHarness(t *testing.T) *protocolHarness {
	t.Helper()
	pluginInput, hostInput := io.Pipe()
	hostOutput, pluginOutput := io.Pipe()
	harness := &protocolHarness{input: hostInput, frames: make(chan testFrame, 32), done: make(chan error, 1)}
	go func() {
		harness.done <- serve(pluginInput, pluginOutput)
		close(harness.done)
	}()
	go harness.read(t, hostOutput)
	harness.call(t, "init", "initialize", map[string]any{"protocol": protocolVersion, "host_version": "test"})
	frame := harness.await(t, func(frame testFrame) bool { return frame.ID == "init" })
	if frame.Error != nil {
		t.Fatalf("initialize failed: %s", frame.Error.Message)
	}
	return harness
}

func (harness *protocolHarness) read(t *testing.T, input io.Reader) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), maxFrameSize)
	for scanner.Scan() {
		var frame testFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Errorf("decode plugin frame: %v", err)
			return
		}
		if strings.HasPrefix(frame.Method, "host.") {
			harness.answerCallback(t, frame)
			continue
		}
		harness.frames <- frame
	}
	if err := scanner.Err(); err != nil {
		t.Errorf("read plugin output: %v", err)
	}
	close(harness.frames)
}

func (harness *protocolHarness) answerCallback(t *testing.T, frame testFrame) {
	t.Helper()
	var params struct {
		ParentID string         `json:"parent_id"`
		Content  string         `json:"content"`
		Extra    map[string]any `json:"extra"`
		Name     string         `json:"name"`
		Args     []any          `json:"args"`
	}
	if err := json.Unmarshal(frame.Params, &params); err != nil {
		t.Errorf("decode callback %s: %v", frame.Method, err)
		return
	}
	if params.ParentID == "" {
		t.Errorf("callback %s omitted parent_id", frame.Method)
	}
	var result any = map[string]any{}
	switch frame.Method {
	case "host.template.validate":
		harness.validates.Add(1)
	case "host.template.render":
		harness.renders.Add(1)
		value := params.Content
		if request, ok := params.Extra["request"].(map[string]any); ok {
			value = strings.ReplaceAll(value, "{{ .request.path }}", fmt.Sprint(request["path"]))
		}
		result = map[string]any{"value": value}
	case "host.function.call":
		harness.functions.Add(1)
		switch params.Name {
		case "secret":
			result = map[string]any{"value": "secret-value"}
		case "decorate":
			result = map[string]any{"value": fmt.Sprint(params.Args[0]) + "!"}
		default:
			harness.respond(frame.ID, nil, &wireError{Code: "unknown_function", Message: "unknown function"})
			return
		}
	default:
		harness.respond(frame.ID, nil, &wireError{Code: "unknown_method", Message: "unknown callback"})
		return
	}
	harness.respond(frame.ID, result, nil)
}

func (harness *protocolHarness) call(t *testing.T, id, method string, params any) {
	t.Helper()
	harness.write(t, map[string]any{"id": id, "method": method, "params": params})
}

func (harness *protocolHarness) cancel(t *testing.T, id string) {
	t.Helper()
	harness.write(t, map[string]any{"method": "cancel", "params": map[string]any{"id": id}})
}

func (harness *protocolHarness) respond(id string, result any, protocolError *wireError) {
	frame := outgoingResponseFrame{ID: id, Result: result, Error: protocolError}
	data, err := json.Marshal(frame)
	if err != nil {
		panic(err)
	}
	harness.writeMu.Lock()
	_, _ = harness.input.Write(append(data, '\n'))
	harness.writeMu.Unlock()
}

func (harness *protocolHarness) write(t *testing.T, frame any) {
	t.Helper()
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	harness.writeMu.Lock()
	defer harness.writeMu.Unlock()
	if _, err := harness.input.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (harness *protocolHarness) await(t *testing.T, matches func(testFrame) bool) testFrame {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case frame, ok := <-harness.frames:
			if !ok {
				t.Fatal("plugin output closed before expected frame")
			}
			if matches(frame) {
				return frame
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for plugin frame")
		}
	}
}

func (harness *protocolHarness) close(t *testing.T) {
	t.Helper()
	harness.call(t, "shutdown", "shutdown", map[string]any{})
	frame := harness.await(t, func(frame testFrame) bool { return frame.ID == "shutdown" })
	if frame.Error != nil {
		t.Errorf("shutdown failed: %s", frame.Error.Message)
	}
	if err := <-harness.done; err != nil {
		t.Errorf("serve failed: %v", err)
	}
}

func TestProtocolDeclaresManagedHTTPServices(t *testing.T) {
	harness := newProtocolHarness(t)
	defer harness.close(t)

	for index, stepType := range []string{"http.forward_proxy", "http.mock_server"} {
		id := fmt.Sprintf("service-%d", index)
		configuration := map[string]any{}
		if stepType == "http.forward_proxy" {
			configuration["keep_alive"] = true
		} else {
			configuration["expectations"] = []string{"unused.yaml"}
		}
		harness.call(t, id, "step.service", map[string]any{"type": stepType, "with": configuration, "context": map[string]any{}})
		frame := harness.await(t, func(frame testFrame) bool { return frame.ID == id })
		if frame.Error != nil {
			t.Fatalf("%s service declaration failed: %s", stepType, frame.Error.Message)
		}
		var result struct {
			Kind      string `json:"kind"`
			KeepAlive bool   `json:"keep_alive"`
			FailFast  bool   `json:"fail_fast"`
		}
		if err := json.Unmarshal(frame.Result, &result); err != nil {
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

	harness := newProtocolHarness(t)
	defer harness.close(t)
	harness.call(t, "run", "step.run", mockRunParams(directory, "expectations.yaml"))
	ready := harness.await(t, func(frame testFrame) bool { return frame.ID == "run" })
	if ready.Error != nil || ready.Event != "ready" {
		t.Fatalf("mock did not become ready: event=%q error=%+v", ready.Event, ready.Error)
	}
	var result wireResult
	if err := json.Unmarshal(ready.Result, &result); err != nil {
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
	harness.cancel(t, "run")
	final := harness.await(t, func(frame testFrame) bool { return frame.ID == "run" && frame.Event == "" })
	if final.Error != nil {
		t.Fatalf("service final response failed: %s", final.Error.Message)
	}
	if harness.validates.Load() == 0 || harness.renders.Load() == 0 || harness.functions.Load() < 2 {
		t.Fatalf("callbacks not exercised: validate=%d render=%d function=%d", harness.validates.Load(), harness.renders.Load(), harness.functions.Load())
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

	harness := newProtocolHarness(t)
	defer harness.close(t)
	harness.call(t, "verify", "step.run", mockRunParams(directory, "expectations.yaml"))
	ready := harness.await(t, func(frame testFrame) bool { return frame.ID == "verify" })
	if ready.Error != nil || ready.Event != "ready" {
		t.Fatalf("mock did not become ready: event=%q error=%+v", ready.Event, ready.Error)
	}
	harness.cancel(t, "verify")
	final := harness.await(t, func(frame testFrame) bool { return frame.ID == "verify" && frame.Event == "" })
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

	harness := newProtocolHarness(t)
	defer harness.close(t)
	harness.call(t, "first", "step.run", mockRunParams(directory, "first.yaml"))
	harness.call(t, "second", "step.run", mockRunParams(directory, "second.yaml"))
	urls := map[string]string{}
	for len(urls) < 2 {
		frame := harness.await(t, func(frame testFrame) bool { return frame.ID == "first" || frame.ID == "second" })
		if frame.Error != nil || frame.Event != "ready" {
			t.Fatalf("%s did not become ready: event=%q error=%+v", frame.ID, frame.Event, frame.Error)
		}
		var result wireResult
		if err := json.Unmarshal(frame.Result, &result); err != nil {
			t.Fatal(err)
		}
		urls[frame.ID], _ = result.Outputs["url"].(string)
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
		harness.cancel(t, id)
	}
	completed := map[string]bool{}
	for len(completed) < 2 {
		frame := harness.await(t, func(frame testFrame) bool { return frame.Event == "" && (frame.ID == "first" || frame.ID == "second") })
		if frame.Error != nil {
			t.Fatalf("%s failed: %s", frame.ID, frame.Error.Message)
		}
		completed[frame.ID] = true
	}
}

func mockRunParams(directory, source string) map[string]any {
	return map[string]any{
		"type": "http.mock_server",
		"with": map[string]any{"expectations": []string{source}},
		"context": map[string]any{
			"step_id": "mock", "workflow_name": "protocol-test", "workflow_source": filepath.Join(directory, "wuko.yaml"),
			"workflow_dir": directory, "run_dir": directory, "vars": map[string]any{"limit": 1024}, "preset_vars": map[string]any{},
			"inputs": map[string]any{}, "env": map[string]string{}, "steps": map[string]any{}, "dependencies": map[string]any{},
			"bindings": map[string]any{}, "providers": map[string]any{}, "helpers": []string{"decorate"},
		},
	}
}
