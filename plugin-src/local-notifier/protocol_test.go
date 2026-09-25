package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/up2jj/wuko/plugin/sdk"
)

type testFrame struct {
	ID     string          `json:"id"`
	Event  string          `json:"event"`
	Data   string          `json:"data"`
	Result json.RawMessage `json:"result"`
	Error  *testWireError  `json:"error"`
}

type testWireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type pluginHarness struct {
	input   *io.PipeWriter
	frames  *bufio.Scanner
	done    chan error
	encoder *json.Encoder
}

func newPluginHarness(t *testing.T, notify notifyFunc) *pluginHarness {
	t.Helper()
	plugin, err := newPlugin(notify)
	if err != nil {
		t.Fatal(err)
	}
	pluginInput, hostInput := io.Pipe()
	hostOutput, pluginOutput := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- plugin.Serve(context.Background(), pluginInput, pluginOutput)
		_ = pluginOutput.Close()
	}()
	harness := &pluginHarness{input: hostInput, frames: bufio.NewScanner(hostOutput), done: done, encoder: json.NewEncoder(hostInput)}
	harness.call(t, "init", "initialize", map[string]any{"protocol": sdk.Protocol})
	frame := harness.next(t)
	if frame.ID != "init" || frame.Error != nil {
		t.Fatalf("initialize frame = %+v", frame)
	}
	return harness
}

func (harness *pluginHarness) call(t *testing.T, id, method string, params any) {
	t.Helper()
	if err := harness.encoder.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		t.Fatal(err)
	}
}

func (harness *pluginHarness) next(t *testing.T) testFrame {
	t.Helper()
	if !harness.frames.Scan() {
		t.Fatalf("plugin output closed: %v", harness.frames.Err())
	}
	var frame testFrame
	if err := json.Unmarshal(harness.frames.Bytes(), &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func (harness *pluginHarness) close(t *testing.T) {
	t.Helper()
	harness.call(t, "shutdown", "shutdown", map[string]any{})
	frame := harness.next(t)
	if frame.ID != "shutdown" || frame.Error != nil {
		t.Errorf("shutdown frame = %+v", frame)
	}
	if err := harness.input.Close(); err != nil {
		t.Error(err)
	}
	if err := <-harness.done; err != nil {
		t.Errorf("Serve() error = %v", err)
	}
}

func TestSDKServesTerminalNotification(t *testing.T) {
	notifier := newNotifier("linux", &fakeCommandRunner{})
	harness := newPluginHarness(t, notifier.notify)
	defer harness.close(t)

	harness.call(t, "run", "step.run", map[string]any{
		"type":    stepType,
		"with":    map[string]any{"message": "done", "delivery": "terminal"},
		"context": map[string]any{"workflow_name": "build"},
	})
	streamed := harness.next(t)
	data, err := base64.StdEncoding.DecodeString(streamed.Data)
	if err != nil {
		t.Fatal(err)
	}
	if streamed.ID != "run" || streamed.Event != "stdout" || string(data) != "build: done\n" {
		t.Fatalf("event = %+v, data = %q", streamed, data)
	}
	result := harness.next(t)
	if result.ID != "run" || result.Error != nil || !strings.Contains(string(result.Result), `"delivery":"terminal"`) {
		t.Fatalf("result = %+v", result)
	}
}

func TestSDKServesCancellation(t *testing.T) {
	started := make(chan struct{})
	notify := func(ctx context.Context, _ config, _ string, _ func([]byte) error) (notificationResult, error) {
		close(started)
		<-ctx.Done()
		return notificationResult{}, ctx.Err()
	}
	harness := newPluginHarness(t, notify)
	defer harness.close(t)

	harness.call(t, "run", "step.run", map[string]any{
		"type": stepType, "with": map[string]any{"message": "done"}, "context": map[string]any{},
	})
	<-started
	if err := harness.encoder.Encode(map[string]any{"method": "cancel", "params": map[string]any{"id": "run"}}); err != nil {
		t.Fatal(err)
	}
	frame := harness.next(t)
	if frame.ID != "run" || frame.Error == nil || frame.Error.Code != "canceled" {
		t.Fatalf("canceled frame = %+v", frame)
	}
}

func TestSDKPreservesStrictStepConfiguration(t *testing.T) {
	notifier := newNotifier("linux", &fakeCommandRunner{})
	harness := newPluginHarness(t, notifier.notify)
	defer harness.close(t)

	harness.call(t, "validate", "step.validate", map[string]any{
		"type":    stepType,
		"with":    map[string]any{"message": "done", "unknown": true},
		"context": map[string]any{},
	})
	frame := harness.next(t)
	if frame.Error == nil || frame.Error.Code != "notification_failed" || !strings.Contains(frame.Error.Message, "unknown field") {
		t.Fatalf("validation frame = %+v", frame)
	}
}
