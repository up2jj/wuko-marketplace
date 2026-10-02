package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/up2jj/wuko/plugin/sdk"
	"github.com/up2jj/wuko/plugin/sdk/sdktest"
)

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
	host := sdktest.Start(t, plugin)
	if initialized := host.Initialization(); initialized.Protocol != sdk.Protocol || initialized.Namespace != namespace {
		t.Fatalf("initialization = %+v", initialized)
	}

	first := host.CallStep(cueStepRequest("first"))
	second := host.CallStep(cueStepRequest("second"))
	waitForStarted(t, started)
	waitForStarted(t, started)
	close(release["second"])
	if response := second.Await(); response.Error != nil {
		t.Fatalf("second response = %+v", response)
	}
	first.Cancel()
	if response := first.Await(); response.Error == nil || response.Error.Code != "canceled" {
		t.Fatalf("first response = %+v", response)
	}
}

func TestSDKPreservesStrictStepConfiguration(t *testing.T) {
	plugin, err := newPlugin(func(context.Context, workerJob) (json.RawMessage, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	host := sdktest.Start(t, plugin)
	response := host.CallStep(sdktest.StepRequest{
		Operation: sdktest.StepValidate,
		Type:      stepType,
		With:      map[string]any{"source": "output: true", "unknown": 1},
	}).Await()
	if response.Error == nil || response.Error.Code != "evaluation_failed" || !strings.Contains(response.Error.Message, "unknown field") {
		t.Fatalf("validation response = %+v", response)
	}
}

func cueStepRequest(source string) sdktest.StepRequest {
	return sdktest.StepRequest{Type: stepType, With: map[string]any{"source": source}}
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
