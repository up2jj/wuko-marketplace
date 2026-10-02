package main

import (
	"context"
	"strings"
	"testing"

	"github.com/up2jj/wuko/plugin/sdk"
	"github.com/up2jj/wuko/plugin/sdk/sdktest"
)

func startPluginHarness(t *testing.T, notify notifyFunc) *sdktest.Harness {
	t.Helper()
	plugin, err := newPlugin(notify)
	if err != nil {
		t.Fatal(err)
	}
	return sdktest.Start(t, plugin)
}

func TestSDKServesTerminalNotification(t *testing.T) {
	notifier := newNotifier("linux", &fakeCommandRunner{})
	host := startPluginHarness(t, notifier.notify)

	call := host.CallStep(sdktest.StepRequest{
		Type:    stepType,
		With:    map[string]any{"message": "done", "delivery": "terminal"},
		Context: sdk.StepContext{WorkflowName: "build"},
	})
	streamed := call.AwaitEvent("stdout")
	if string(streamed.Data) != "build: done\n" {
		t.Fatalf("event = %+v", streamed)
	}
	result := call.Await()
	if result.Error != nil || !strings.Contains(string(result.Result), `"delivery":"terminal"`) {
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
	host := startPluginHarness(t, notify)

	call := host.CallStep(sdktest.StepRequest{Type: stepType, With: map[string]any{"message": "done"}})
	<-started
	call.Cancel()
	response := call.Await()
	if response.Error == nil || response.Error.Code != "canceled" {
		t.Fatalf("canceled response = %+v", response)
	}
}

func TestSDKPreservesStrictStepConfiguration(t *testing.T) {
	notifier := newNotifier("linux", &fakeCommandRunner{})
	host := startPluginHarness(t, notifier.notify)

	response := host.CallStep(sdktest.StepRequest{
		Operation: sdktest.StepValidate,
		Type:      stepType,
		With:      map[string]any{"message": "done", "unknown": true},
	}).Await()
	if response.Error == nil || response.Error.Code != "notification_failed" || !strings.Contains(response.Error.Message, "unknown field") {
		t.Fatalf("validation response = %+v", response)
	}
}
