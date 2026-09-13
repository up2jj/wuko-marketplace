package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestInitialize(t *testing.T) {
	s := &server{worker: func(context.Context, workerJob) (json.RawMessage, error) { return nil, nil }}
	result, err := s.dispatch(t.Context(), request{Method: "initialize", Params: json.RawMessage(`{"protocol":"wuko.plugin/v1","host_version":"v0.14.0"}`)})
	if err != nil {
		t.Fatalf("dispatch() error = %v", err)
	}
	declaration := result.(map[string]any)
	if declaration["namespace"] != "cue" || declaration["protocol"] != protocolVersion {
		t.Fatalf("initialize result = %#v", declaration)
	}
	steps := declaration["steps"].([]any)
	if steps[0].(map[string]any)["type"] != stepType {
		t.Fatalf("initialize steps = %#v", steps)
	}
	if !s.runtimeAllowed.Load() {
		t.Fatal("compatible host did not enable runtime operations")
	}
}

func TestHostVersionCompatibility(t *testing.T) {
	for _, test := range []struct {
		version string
		allowed bool
		wantErr bool
	}{
		{version: ""},
		{version: "dev", allowed: true},
		{version: "v0.14.0", allowed: true},
		{version: "0.14.0", allowed: true},
		{version: "v0.15.0", allowed: true},
		// "git describe" output for a build made after the v0.14.0 tag.
		{version: "v0.14.0-12-gabc1234", allowed: true},
		{version: "v0.14.0-rc.1", allowed: true},
		{version: "v0.13.0", wantErr: true},
		{version: "v0.13.0-70-gabc1234", wantErr: true},
		// An unrecognized version leaves the handshake successful and the runtime disabled.
		{version: "next"},
		{version: "625a6aa"},
	} {
		t.Run(test.version, func(t *testing.T) {
			allowed, err := validateHostVersion(test.version)
			if (err != nil) != test.wantErr || allowed != test.allowed {
				t.Fatalf("validateHostVersion(%q) = %t, %v", test.version, allowed, err)
			}
		})
	}
}

func TestVersionlessHandshakeDoesNotAllowRuntime(t *testing.T) {
	s := &server{worker: func(context.Context, workerJob) (json.RawMessage, error) { return nil, nil }}
	if _, err := s.dispatch(t.Context(), request{Method: "initialize", Params: json.RawMessage(`{"protocol":"wuko.plugin/v1"}`)}); err != nil {
		t.Fatalf("verification handshake error = %v", err)
	}
	_, err := s.dispatch(t.Context(), request{
		Method: "step.run",
		Params: json.RawMessage(`{"type":"cue.eval","with":{"source":"output: true"},"context":{}}`),
	})
	if err == nil || !strings.Contains(err.Error(), minHostVersion) {
		t.Fatalf("runtime error = %v", err)
	}
}

func TestServeConcurrentRequestsAndCancellation(t *testing.T) {
	started := make(chan string, 2)
	release := map[string]chan struct{}{"first": make(chan struct{}), "second": make(chan struct{})}
	worker := func(ctx context.Context, job workerJob) (json.RawMessage, error) {
		source := job.Configuration.Source
		started <- source
		select {
		case <-release[source]:
			return json.RawMessage(`{"source":"` + source + `"}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serve(requestReader, responseWriter, worker)
		responseWriter.Close()
	}()
	encoder := json.NewEncoder(requestWriter)
	decoder := json.NewDecoder(responseReader)
	if err := encoder.Encode(map[string]any{"id": "init", "method": "initialize", "params": map[string]any{"protocol": protocolVersion, "host_version": "dev"}}); err != nil {
		t.Fatal(err)
	}
	var initialized response
	if err := decoder.Decode(&initialized); err != nil || initialized.Error != nil {
		t.Fatalf("initialize response = %#v, error = %v", initialized, err)
	}

	writeStepRequest(t, encoder, "1", "first")
	writeStepRequest(t, encoder, "2", "second")
	waitForStarted(t, started)
	waitForStarted(t, started)
	close(release["second"])
	var second response
	if err := decoder.Decode(&second); err != nil {
		t.Fatal(err)
	}
	if second.ID != "2" || second.Error != nil {
		t.Fatalf("second response = %#v", second)
	}
	if err := encoder.Encode(map[string]any{"method": "cancel", "params": map[string]any{"id": "1"}}); err != nil {
		t.Fatal(err)
	}
	var first response
	if err := decoder.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first.ID != "1" || first.Error == nil || first.Error.Code != "canceled" {
		t.Fatalf("first response = %#v", first)
	}
	if err := encoder.Encode(map[string]any{"id": "3", "method": "shutdown", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	var shutdown response
	if err := decoder.Decode(&shutdown); err != nil {
		t.Fatal(err)
	}
	if shutdown.ID != "3" || shutdown.Error != nil {
		t.Fatalf("shutdown response = %#v", shutdown)
	}
	requestWriter.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func writeStepRequest(t *testing.T, encoder *json.Encoder, id, source string) {
	t.Helper()
	request := map[string]any{
		"id": id, "method": "step.run",
		"params": map[string]any{
			"type":    stepType,
			"with":    map[string]any{"source": source},
			"context": map[string]any{},
		},
	}
	if err := encoder.Encode(request); err != nil {
		t.Fatal(err)
	}
}

func waitForStarted(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case value := <-started:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start")
		return ""
	}
}

func TestStrictStepConfiguration(t *testing.T) {
	s := &server{worker: func(context.Context, workerJob) (json.RawMessage, error) { return nil, nil }}
	s.runtimeAllowed.Store(true)
	_, err := s.dispatch(t.Context(), request{
		Method: "step.run",
		Params: json.RawMessage(`{"type":"cue.eval","with":{"source":"output: true","unknown":1},"context":{}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("dispatch() error = %v", err)
	}
}

func TestProtocolPreservesLargeContextInteger(t *testing.T) {
	s := &server{worker: func(_ context.Context, job workerJob) (json.RawMessage, error) {
		if job.Context.Vars["account_id"] != json.Number("9007199254740993") {
			t.Fatalf("worker account_id = %#v", job.Context.Vars["account_id"])
		}
		return json.RawMessage(`9007199254740993`), nil
	}}
	s.runtimeAllowed.Store(true)
	result, err := s.dispatch(t.Context(), request{
		Method: "step.run",
		Params: json.RawMessage(`{"type":"cue.eval","with":{"source":"output: wuko.vars.account_id"},"context":{"vars":{"account_id":9007199254740993}}}`),
	})
	if err != nil {
		t.Fatalf("dispatch() error = %v", err)
	}
	value := result.(map[string]any)["outputs"].(map[string]any)["value"].(json.RawMessage)
	if string(value) != "9007199254740993" {
		t.Fatalf("result = %s", value)
	}
}
