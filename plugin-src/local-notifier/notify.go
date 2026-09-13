package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const appleScript = `on run argv
	display notification (item 2 of argv) with title (item 1 of argv)
end run`

type notificationResult struct {
	Delivery string
	Fallback bool
}

type notifyFunc func(context.Context, config, string, func([]byte) error) (notificationResult, error)

type commandRunner interface {
	LookPath(string) (string, error)
	Run(context.Context, string, ...string) error
}

type osCommandRunner struct{}

func (osCommandRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (osCommandRunner) Run(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return err
		}
		return errors.New(detail)
	}
	return nil
}

type notifier struct {
	goos   string
	runner commandRunner
}

func newNotifier(goos string, runner commandRunner) *notifier {
	return &notifier{goos: goos, runner: runner}
}

func (n *notifier) notify(ctx context.Context, configuration config, workflowName string, emit func([]byte) error) (notificationResult, error) {
	title := configuration.resolvedTitle(workflowName)
	switch configuration.Delivery {
	case deliveryTerminal:
		if err := emitTerminal(emit, title, configuration.Message); err != nil {
			return notificationResult{}, err
		}
		return notificationResult{Delivery: deliveryTerminal}, nil
	case deliverySystem:
		if err := n.notifySystem(ctx, title, configuration.Message); err != nil {
			return notificationResult{}, err
		}
		return notificationResult{Delivery: deliverySystem}, nil
	case deliveryAuto:
		if err := n.notifySystem(ctx, title, configuration.Message); err == nil {
			return notificationResult{Delivery: deliverySystem}, nil
		}
		if ctx.Err() != nil {
			return notificationResult{}, ctx.Err()
		}
		if err := emitTerminal(emit, title, configuration.Message); err != nil {
			return notificationResult{}, err
		}
		return notificationResult{Delivery: deliveryTerminal, Fallback: true}, nil
	default:
		return notificationResult{}, fmt.Errorf("unsupported notification delivery %q", configuration.Delivery)
	}
}

func emitTerminal(emit func([]byte) error, title, message string) error {
	if err := emit([]byte(fmt.Sprintf("%s: %s\n", title, message))); err != nil {
		return fmt.Errorf("writing terminal notification: %w", err)
	}
	return nil
}

func (n *notifier) notifySystem(ctx context.Context, title, message string) error {
	switch n.goos {
	case "darwin":
		if err := n.runner.Run(ctx, "/usr/bin/osascript", "-e", appleScript, "--", title, message); err != nil {
			return fmt.Errorf("sending macOS notification: %w", err)
		}
		return nil
	case "linux":
		path, err := n.runner.LookPath("notify-send")
		if err != nil {
			return fmt.Errorf("system notifications require notify-send: %w", err)
		}
		if err := n.runner.Run(ctx, path, "--app-name=Wuko", "--", title, message); err != nil {
			return fmt.Errorf("sending Linux notification: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("system notifications are not supported on %s", n.goos)
	}
}
