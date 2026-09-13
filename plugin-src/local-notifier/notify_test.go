package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type commandInvocation struct {
	name string
	args []string
}

type fakeCommandRunner struct {
	path        string
	lookupErr   error
	runErr      error
	invocations []commandInvocation
}

func (r *fakeCommandRunner) LookPath(string) (string, error) {
	return r.path, r.lookupErr
}

func (r *fakeCommandRunner) Run(_ context.Context, name string, args ...string) error {
	r.invocations = append(r.invocations, commandInvocation{name: name, args: append([]string(nil), args...)})
	return r.runErr
}

func TestTerminalNotification(t *testing.T) {
	runner := &fakeCommandRunner{}
	notifier := newNotifier("linux", runner)
	var output bytes.Buffer
	result, err := notifier.notify(t.Context(), config{Message: "finished", Delivery: deliveryTerminal}, "deploy", func(data []byte) error {
		_, err := output.Write(data)
		return err
	})
	if err != nil {
		t.Fatalf("notify() error = %v", err)
	}
	if result != (notificationResult{Delivery: deliveryTerminal}) {
		t.Fatalf("result = %#v", result)
	}
	if output.String() != "deploy: finished\n" {
		t.Fatalf("terminal output = %q", output.String())
	}
	if len(runner.invocations) != 0 {
		t.Fatalf("system invocations = %#v", runner.invocations)
	}
}

func TestSystemNotificationCommands(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		path     string
		wantName string
		wantArgs []string
	}{
		{
			name: "darwin", goos: "darwin", wantName: "/usr/bin/osascript",
			wantArgs: []string{"-e", appleScript, "--", "-Release", `Done "safely"`},
		},
		{
			name: "linux", goos: "linux", path: "/usr/bin/notify-send", wantName: "/usr/bin/notify-send",
			wantArgs: []string{"--app-name=Wuko", "--", "-Release", `Done "safely"`},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeCommandRunner{path: test.path}
			notifier := newNotifier(test.goos, runner)
			title := "-Release"
			result, err := notifier.notify(t.Context(), config{Message: `Done "safely"`, Title: &title, Delivery: deliverySystem}, "ignored", func([]byte) error {
				t.Fatal("system delivery emitted terminal output")
				return nil
			})
			if err != nil {
				t.Fatalf("notify() error = %v", err)
			}
			if result != (notificationResult{Delivery: deliverySystem}) {
				t.Fatalf("result = %#v", result)
			}
			if len(runner.invocations) != 1 {
				t.Fatalf("invocations = %#v", runner.invocations)
			}
			invocation := runner.invocations[0]
			if invocation.name != test.wantName || !reflect.DeepEqual(invocation.args, test.wantArgs) {
				t.Fatalf("invocation = %#v, want name %q args %#v", invocation, test.wantName, test.wantArgs)
			}
		})
	}
}

func TestAutoFallsBackToTerminal(t *testing.T) {
	runner := &fakeCommandRunner{lookupErr: errors.New("not found")}
	notifier := newNotifier("linux", runner)
	var output bytes.Buffer
	result, err := notifier.notify(t.Context(), config{Message: "finished", Delivery: deliveryAuto}, "deploy", func(data []byte) error {
		_, err := output.Write(data)
		return err
	})
	if err != nil {
		t.Fatalf("notify() error = %v", err)
	}
	if result != (notificationResult{Delivery: deliveryTerminal, Fallback: true}) {
		t.Fatalf("result = %#v", result)
	}
	if output.String() != "deploy: finished\n" {
		t.Fatalf("terminal output = %q", output.String())
	}
}

func TestSystemFailureIsStrict(t *testing.T) {
	tests := []struct {
		name    string
		runner  *fakeCommandRunner
		wantErr string
	}{
		{name: "missing command", runner: &fakeCommandRunner{lookupErr: errors.New("not found")}, wantErr: "require notify-send"},
		{name: "command failed", runner: &fakeCommandRunner{path: "/usr/bin/notify-send", runErr: errors.New("no notification daemon")}, wantErr: "sending Linux notification"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			notifier := newNotifier("linux", test.runner)
			_, err := notifier.notify(t.Context(), config{Message: "finished", Delivery: deliverySystem}, "deploy", func([]byte) error {
				t.Fatal("strict system failure emitted terminal output")
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("notify() error = %v", err)
			}
		})
	}
}

func TestAutoDoesNotFallbackAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runner := &fakeCommandRunner{path: "/usr/bin/notify-send", runErr: context.Canceled}
	notifier := newNotifier("linux", runner)
	_, err := notifier.notify(ctx, config{Message: "finished", Delivery: deliveryAuto}, "deploy", func([]byte) error {
		t.Fatal("canceled notification emitted terminal output")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("notify() error = %v", err)
	}
}

func TestUnsupportedSystem(t *testing.T) {
	notifier := newNotifier("windows", &fakeCommandRunner{})
	_, err := notifier.notify(t.Context(), config{Message: "finished", Delivery: deliverySystem}, "deploy", func([]byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "not supported on windows") {
		t.Fatalf("notify() error = %v", err)
	}
}
