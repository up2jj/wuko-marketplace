package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/up2jj/wuko-marketplace/plugin-src/http/forwardproxy"
	"github.com/up2jj/wuko-marketplace/plugin-src/http/mockserver"
	"github.com/up2jj/wuko/helper"
	"github.com/up2jj/wuko/plugin/sdk"
	"github.com/up2jj/wuko/provider"
	"github.com/up2jj/wuko/step"
)

const namespace = "http"

type managedServiceRunner interface {
	step.Runner
	ServiceOptions() (string, step.ServiceOptions)
}

func newPlugin() (*sdk.Plugin, error) {
	plugin, err := sdk.New(namespace)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"forward_proxy", "mock_server"} {
		stepType := namespace + "." + name
		err := sdk.RegisterStep(plugin, name, sdk.StepHandler[json.RawMessage]{
			Validate: func(ctx context.Context, request sdk.StepRequest[json.RawMessage]) error {
				runner, err := buildRunner(stepType, request.Config)
				if err != nil {
					return pluginError(err)
				}
				validator, ok := runner.(step.Validator)
				if !ok {
					return nil
				}
				return pluginError(validator.Validate(ctx, stepRequest(ctx, request, nil)))
			},
			Service: func(_ context.Context, request sdk.StepRequest[json.RawMessage]) (sdk.Service, error) {
				runner, err := buildRunner(stepType, request.Config)
				if err != nil {
					return sdk.Service{}, pluginError(err)
				}
				serviceRunner, ok := runner.(managedServiceRunner)
				if !ok {
					return sdk.Service{}, pluginError(fmt.Errorf("step is not a managed service"))
				}
				kind, options := serviceRunner.ServiceOptions()
				return sdk.Service{Kind: kind, KeepAlive: options.KeepAlive, FailFast: options.FailFast, ExitOnEnd: options.ExitOnEnd}, nil
			},
			Run: func(ctx context.Context, request sdk.StepRequest[json.RawMessage]) (sdk.Result, error) {
				runner, err := buildRunner(stepType, request.Config)
				if err != nil {
					return sdk.Result{}, pluginError(err)
				}
				result, err := runService(ctx, request, runner)
				return result, pluginError(err)
			},
			Cleanup: func(ctx context.Context, request sdk.CleanupRequest[json.RawMessage]) error {
				runner, err := buildRunner(stepType, request.Config)
				if err != nil {
					return pluginError(err)
				}
				cleaner, ok := runner.(step.Cleaner)
				if !ok {
					return pluginError(fmt.Errorf("step does not support cleanup"))
				}
				return pluginError(cleaner.Cleanup(ctx, step.Result{Outputs: request.Result.Outputs, Variables: request.Result.Variables}))
			},
		}, sdk.WithHostCallbacks(sdk.HostTemplateValidate, sdk.HostTemplateRender, sdk.HostFunctionCall))
		if err != nil {
			return nil, err
		}
	}
	return plugin, nil
}

func runService(ctx context.Context, request sdk.StepRequest[json.RawMessage], runner step.Runner) (sdk.Result, error) {
	serviceRunner, ok := runner.(managedServiceRunner)
	if !ok {
		return sdk.Result{}, fmt.Errorf("step is not a managed service")
	}
	kind, options := serviceRunner.ServiceOptions()
	launcher := &capturingServices{kind: kind, options: options}
	result, err := runner.Run(ctx, stepRequest(ctx, request, launcher))
	if err != nil {
		return sdk.Result{}, err
	}
	service, err := launcher.service()
	if err != nil {
		return sdk.Result{}, err
	}

	serviceCtx, stopService := context.WithCancel(context.WithoutCancel(ctx))
	serviceStarted := make(chan struct{})
	serviceDone := make(chan error, 1)
	go func() {
		close(serviceStarted)
		serviceDone <- service(serviceCtx)
	}()
	<-serviceStarted
	wire := sdk.Result{Outputs: result.Outputs, Variables: result.Variables}
	if err := request.Ready(wire); err != nil {
		stopService()
		return sdk.Result{}, errors.Join(err, <-serviceDone)
	}
	var serviceErr error
	select {
	case serviceErr = <-serviceDone:
		stopService()
	case <-ctx.Done():
		stopService()
		serviceErr = <-serviceDone
	}
	if ctx.Err() != nil && cancellationOnly(serviceErr) {
		return wire, nil
	}
	return wire, serviceErr
}

func buildRunner(stepType string, raw json.RawMessage) (step.Runner, error) {
	var configuration map[string]any
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		configuration = map[string]any{}
	} else if err := decodeRaw(raw, &configuration); err != nil {
		return nil, fmt.Errorf("decoding %s configuration: %w", stepType, err)
	}
	switch stepType {
	case "http.forward_proxy":
		return forwardproxy.New(configuration)
	case "http.mock_server":
		return mockserver.New(configuration)
	default:
		return nil, fmt.Errorf("unsupported step type %q", stepType)
	}
}

func stepRequest(ctx context.Context, request sdk.StepRequest[json.RawMessage], services step.ServiceLauncher) step.Request {
	value := request.Context
	helpers := make(helper.Set, len(value.Helpers))
	for _, name := range value.Helpers {
		helpers[name] = func(callCtx context.Context, args []any) (any, error) {
			value, err := request.Host.CallFunction(callCtx, name, args)
			return normalizeValue(value), err
		}
	}
	renderer := &hostRenderer{ctx: ctx, host: request.Host}
	result := step.Request{
		StepID: value.StepID, WorkflowName: value.WorkflowName, WorkflowSource: value.WorkflowSource,
		WorkflowDir: value.WorkflowDir, WorkflowDirBorrowed: value.WorkflowDirBorrowed, WorkflowTimezone: value.WorkflowTimezone,
		RunDir: value.RunDir, EnvironmentLoaders: value.EnvironmentLoaders, LocalValueDir: value.LocalValueDir, GlobalValueDir: value.GlobalValueDir,
		Vars: normalizeMap(value.Vars), PresetVars: normalizeMap(value.PresetVars), Inputs: normalizeMap(value.Inputs), Env: value.Env, Steps: normalizeMap(value.Steps),
		Dependencies: normalizeNestedMap(value.Dependencies), Bindings: normalizeMap(value.Bindings), Providers: provider.Set{Values: providerValues(value.Providers)},
		Stdout: request.Stdout, Stderr: request.Stderr, Attempt: value.Attempt, MaxAttempts: value.MaxAttempts, OperationID: value.OperationID,
		TemplateRenderer: renderer, Helpers: helpers, HelperContext: ctx, Services: services,
	}
	result.Secret = func(reference string) (string, error) { return request.Host.Secret(ctx, reference) }
	if value.PreviousAttempt != nil {
		result.PreviousAttempt = &step.Result{Outputs: normalizeMap(value.PreviousAttempt.Outputs), Variables: normalizeMap(value.PreviousAttempt.Variables)}
	}
	return result
}

func providerValues(values map[string]any) map[string]map[string]any {
	result := make(map[string]map[string]any, len(values))
	for name, value := range values {
		if fields, ok := value.(map[string]any); ok {
			result[name] = normalizeMap(fields)
		}
	}
	return result
}

func normalizeMap(values map[string]any) map[string]any {
	for name, value := range values {
		values[name] = normalizeValue(value)
	}
	return values
}

func normalizeNestedMap(values map[string]map[string]any) map[string]map[string]any {
	for name, value := range values {
		values[name] = normalizeMap(value)
	}
	return values
}

// The SDK decodes protocol JSON numbers as float64, while Wuko's in-process workflow data uses
// Go ints for integers. Restore integral values to int so Expr comparisons, arithmetic, and
// string conversions behave identically on either side of the plugin boundary.
func normalizeValue(value any) any {
	switch typed := value.(type) {
	case float64:
		if typed == math.Trunc(typed) && typed >= -(1<<53) && typed <= 1<<53 {
			return int(typed)
		}
		return typed
	case json.Number:
		if !strings.ContainsAny(string(typed), ".eE") {
			if number, err := strconv.ParseInt(string(typed), 10, 64); err == nil {
				return int(number)
			}
			if number, err := strconv.ParseUint(string(typed), 10, 64); err == nil {
				return number
			}
			return typed
		}
		if number, err := strconv.ParseFloat(string(typed), 64); err == nil {
			return number
		}
		return typed
	case map[string]any:
		return normalizeMap(typed)
	case []any:
		for index, item := range typed {
			typed[index] = normalizeValue(item)
		}
		return typed
	default:
		return value
	}
}

type capturingServices struct {
	mu      sync.Mutex
	kind    string
	options step.ServiceOptions
	run     func(context.Context) error
}

func (services *capturingServices) StartService(_ string, kind string, options step.ServiceOptions, run func(context.Context) error) error {
	services.mu.Lock()
	defer services.mu.Unlock()
	if services.run != nil {
		return fmt.Errorf("service registered managed work more than once")
	}
	if kind != services.kind || options != services.options {
		return fmt.Errorf("service policy changed between declaration and startup")
	}
	services.run = run
	return nil
}

func (services *capturingServices) service() (func(context.Context) error, error) {
	services.mu.Lock()
	defer services.mu.Unlock()
	if services.run == nil {
		return nil, fmt.Errorf("service step did not register managed work")
	}
	return services.run, nil
}

type hostRenderer struct {
	ctx  context.Context
	host sdk.Host
}

func (renderer *hostRenderer) Validate(value string) error { return renderer.ValidateContent(value) }
func (renderer *hostRenderer) Render(value string) (string, error) {
	return renderer.RenderContent(value)
}
func (renderer *hostRenderer) ValidateContent(value string) error {
	return renderer.host.ValidateTemplate(renderer.ctx, value)
}
func (renderer *hostRenderer) RenderContent(value string) (string, error) {
	return renderer.RenderContentWith(value, nil)
}
func (renderer *hostRenderer) RenderWith(value string, extra map[string]any) (string, error) {
	return renderer.RenderContentWith(value, extra)
}
func (renderer *hostRenderer) RenderContentWith(value string, extra map[string]any) (string, error) {
	return renderer.host.RenderTemplate(renderer.ctx, value, extra)
}
func (renderer *hostRenderer) Snapshot() step.DataTemplateRenderer       { return renderer }
func (renderer *hostRenderer) WithoutSecrets() step.DataTemplateRenderer { return renderer }

func decodeRaw(data json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("value contains trailing data")
	}
	return nil
}

func pluginError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sdk.Coded("canceled", err)
	}
	return sdk.Coded("step_failed", err)
}

func cancellationOnly(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !cancellationOnly(child) {
				return false
			}
		}
		return true
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return cancellationOnly(wrapped)
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
