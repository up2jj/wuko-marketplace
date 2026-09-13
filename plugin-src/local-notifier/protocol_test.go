package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestInitialize(t *testing.T) {
	s := &server{}
	result, err := s.dispatch(t.Context(), request{Method: "initialize", Params: json.RawMessage(`{"protocol":"wuko.plugin/v1"}`)})
	if err != nil {
		t.Fatalf("dispatch() error = %v", err)
	}
	declaration := result.(map[string]any)
	if declaration["namespace"] != namespace || declaration["protocol"] != protocolVersion {
		t.Fatalf("initialize result = %#v", declaration)
	}
	steps := declaration["steps"].([]any)
	if steps[0].(map[string]any)["type"] != stepType {
		t.Fatalf("initialize steps = %#v", steps)
	}
}

func TestServeTerminalNotification(t *testing.T) {
	input := strings.NewReader(
		`{"id":"1","method":"step.run","params":{"type":"local-notifier.notify","with":{"message":"done","delivery":"terminal"},"context":{"workflow_name":"build"}}}` + "\n" +
			`{"id":"2","method":"shutdown","params":{}}` + "\n",
	)
	var output bytes.Buffer
	notifier := newNotifier("linux", &fakeCommandRunner{})
	if err := serve(input, &output, notifier.notify); err != nil {
		t.Fatalf("serve() error = %v", err)
	}
	decoder := json.NewDecoder(&output)
	var streamed event
	if err := decoder.Decode(&streamed); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(streamed.Data)
	if err != nil {
		t.Fatal(err)
	}
	if streamed.ID != "1" || streamed.Event != "stdout" || string(data) != "build: done\n" {
		t.Fatalf("event = %#v, data = %q", streamed, data)
	}
	var step response
	if err := decoder.Decode(&step); err != nil {
		t.Fatal(err)
	}
	if step.ID != "1" || step.Error != nil {
		t.Fatalf("step response = %#v", step)
	}
	var shutdown response
	if err := decoder.Decode(&shutdown); err != nil {
		t.Fatal(err)
	}
	if shutdown.ID != "2" || shutdown.Error != nil {
		t.Fatalf("shutdown response = %#v", shutdown)
	}
}

func TestServeCancellation(t *testing.T) {
	started := make(chan struct{})
	notify := func(ctx context.Context, _ config, _ string, _ func([]byte) error) (notificationResult, error) {
		close(started)
		<-ctx.Done()
		return notificationResult{}, ctx.Err()
	}
	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serve(requestReader, responseWriter, notify)
		responseWriter.Close()
	}()
	encoder := json.NewEncoder(requestWriter)
	decoder := json.NewDecoder(responseReader)
	if err := encoder.Encode(map[string]any{
		"id": "1", "method": "step.run",
		"params": map[string]any{"type": stepType, "with": map[string]any{"message": "done"}, "context": map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("notification did not start")
	}
	if err := encoder.Encode(map[string]any{"method": "cancel", "params": map[string]any{"id": "1"}}); err != nil {
		t.Fatal(err)
	}
	var canceled response
	if err := decoder.Decode(&canceled); err != nil {
		t.Fatal(err)
	}
	if canceled.ID != "1" || canceled.Error == nil || canceled.Error.Code != "canceled" {
		t.Fatalf("canceled response = %#v", canceled)
	}
	if err := encoder.Encode(map[string]any{"id": "2", "method": "shutdown", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	var shutdown response
	if err := decoder.Decode(&shutdown); err != nil {
		t.Fatal(err)
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

func TestStrictStepConfiguration(t *testing.T) {
	s := &server{}
	_, err := s.dispatch(t.Context(), request{
		Method: "step.validate",
		Params: json.RawMessage(`{"type":"local-notifier.notify","with":{"message":"done","unknown":true},"context":{}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("dispatch() error = %v", err)
	}
}
