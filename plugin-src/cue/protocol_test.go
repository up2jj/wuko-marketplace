package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/up2jj/wuko/plugin/sdk"
)

type testResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *testWireError  `json:"error"`
}

type testWireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func TestSDKServesConcurrentRequestsAndCancellation(t *testing.T) {
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
	plugin, err := newPlugin(worker)
	if err != nil {
		t.Fatal(err)
	}
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- plugin.Serve(context.Background(), requestReader, responseWriter)
		_ = responseWriter.Close()
	}()
	encoder := json.NewEncoder(requestWriter)
	frames := bufio.NewScanner(responseReader)
	writeRequest(t, encoder, "init", "initialize", map[string]any{"protocol": sdk.Protocol})
	initialized := readResponse(t, frames)
	if initialized.ID != "init" || initialized.Error != nil || !strings.Contains(string(initialized.Result), `"protocol":"wuko.plugin/v3"`) {
		t.Fatalf("initialize response = %+v", initialized)
	}

	writeStepRequest(t, encoder, "1", "first")
	writeStepRequest(t, encoder, "2", "second")
	waitForStarted(t, started)
	waitForStarted(t, started)
	close(release["second"])
	second := readResponse(t, frames)
	if second.ID != "2" || second.Error != nil {
		t.Fatalf("second response = %+v", second)
	}
	if err := encoder.Encode(map[string]any{"method": "cancel", "params": map[string]any{"id": "1"}}); err != nil {
		t.Fatal(err)
	}
	first := readResponse(t, frames)
	if first.ID != "1" || first.Error == nil || first.Error.Code != "canceled" {
		t.Fatalf("first response = %+v", first)
	}
	writeRequest(t, encoder, "shutdown", "shutdown", map[string]any{})
	shutdown := readResponse(t, frames)
	if shutdown.ID != "shutdown" || shutdown.Error != nil {
		t.Fatalf("shutdown response = %+v", shutdown)
	}
	if err := requestWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestSDKPreservesStrictStepConfiguration(t *testing.T) {
	plugin, err := newPlugin(func(context.Context, workerJob) (json.RawMessage, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- plugin.Serve(context.Background(), requestReader, responseWriter) }()
	encoder := json.NewEncoder(requestWriter)
	frames := bufio.NewScanner(responseReader)
	writeRequest(t, encoder, "init", "initialize", map[string]any{"protocol": sdk.Protocol})
	_ = readResponse(t, frames)
	writeRequest(t, encoder, "validate", "step.validate", map[string]any{
		"type": stepType, "with": map[string]any{"source": "output: true", "unknown": 1}, "context": map[string]any{},
	})
	response := readResponse(t, frames)
	if response.Error == nil || response.Error.Code != "evaluation_failed" || !strings.Contains(response.Error.Message, "unknown field") {
		t.Fatalf("validation response = %+v", response)
	}
	writeRequest(t, encoder, "shutdown", "shutdown", map[string]any{})
	_ = readResponse(t, frames)
	_ = requestWriter.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func writeStepRequest(t *testing.T, encoder *json.Encoder, id, source string) {
	t.Helper()
	writeRequest(t, encoder, id, "step.run", map[string]any{
		"type": stepType, "with": map[string]any{"source": source}, "context": map[string]any{},
	})
}

func writeRequest(t *testing.T, encoder *json.Encoder, id, method string, params any) {
	t.Helper()
	if err := encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		t.Fatal(err)
	}
}

func readResponse(t *testing.T, frames *bufio.Scanner) testResponse {
	t.Helper()
	if !frames.Scan() {
		t.Fatalf("plugin output closed: %v", frames.Err())
	}
	var response testResponse
	if err := json.Unmarshal(frames.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
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
