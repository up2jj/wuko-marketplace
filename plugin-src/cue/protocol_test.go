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
	result, err := s.dispatch(t.Context(), request{Method: "initialize", Params: json.RawMessage(`{"protocol":"wuko.plugin/v1"}`)})
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
}

func TestServeConcurrentRequestsAndCancellation(t *testing.T) {
	started := make(chan string, 2)
	release := map[string]chan struct{}{"first": make(chan struct{}), "second": make(chan struct{})}
	worker := func(ctx context.Context, job workerJob) (json.RawMessage, error) {
		started <- job.Source
		select {
		case <-release[job.Source]:
			return json.RawMessage(`{"source":"` + job.Source + `"}`), nil
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
	_, err := s.dispatch(t.Context(), request{
		Method: "step.run",
		Params: json.RawMessage(`{"type":"cue.eval","with":{"source":"output: true","unknown":1},"context":{}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("dispatch() error = %v", err)
	}
}
