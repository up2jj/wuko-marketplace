package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/up2jj/wuko-marketplace/plugin-src/http/forwardproxy"
	"github.com/up2jj/wuko-marketplace/plugin-src/http/mockserver"
	"github.com/up2jj/wuko/helper"
	"github.com/up2jj/wuko/provider"
	"github.com/up2jj/wuko/step"
)

const (
	protocolVersion = "wuko.plugin/v2"
	namespace       = "http"
	maxFrameSize    = 10 << 20
)

type requestFrame struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type outgoingRequestFrame struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type responseFrame struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}

type outgoingResponseFrame struct {
	ID     string     `json:"id"`
	Result any        `json:"result,omitempty"`
	Error  *wireError `json:"error,omitempty"`
}

type eventFrame struct {
	ID     string `json:"id"`
	Event  string `json:"event"`
	Data   string `json:"data,omitempty"`
	Result any    `json:"result,omitempty"`
}

type wireError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type stepParams struct {
	Type    string          `json:"type"`
	With    json.RawMessage `json:"with"`
	Context stepContext     `json:"context"`
}

type stepContext struct {
	StepID              string                    `json:"step_id"`
	WorkflowName        string                    `json:"workflow_name"`
	WorkflowSource      string                    `json:"workflow_source"`
	WorkflowDir         string                    `json:"workflow_dir"`
	WorkflowDirBorrowed bool                      `json:"workflow_dir_borrowed"`
	WorkflowTimezone    string                    `json:"workflow_timezone"`
	RunDir              string                    `json:"run_dir"`
	EnvironmentLoaders  []string                  `json:"environment_loaders"`
	LocalValueDir       string                    `json:"local_value_dir"`
	GlobalValueDir      string                    `json:"global_value_dir"`
	Vars                map[string]any            `json:"vars"`
	PresetVars          map[string]any            `json:"preset_vars"`
	Inputs              map[string]any            `json:"inputs"`
	Env                 map[string]string         `json:"env"`
	Steps               map[string]any            `json:"steps"`
	Dependencies        map[string]map[string]any `json:"dependencies"`
	Bindings            map[string]any            `json:"bindings"`
	Providers           map[string]map[string]any `json:"providers"`
	Helpers             []string                  `json:"helpers"`
	Attempt             int                       `json:"attempt"`
	MaxAttempts         int                       `json:"max_attempts"`
	OperationID         string                    `json:"operation_id"`
	PreviousAttempt     *wireResult               `json:"previous_attempt,omitempty"`
}

type wireResult struct {
	Outputs   map[string]any `json:"outputs"`
	Variables map[string]any `json:"variables"`
}

type pendingHostCall struct {
	response  chan responseFrame
	abandoned bool
}

type server struct {
	output io.Writer

	writeMu sync.Mutex
	jobsMu  sync.Mutex
	jobs    map[string]context.CancelFunc
	jobsWG  sync.WaitGroup

	hostMu      sync.Mutex
	hostCalls   map[string]pendingHostCall
	nextHostID  atomic.Uint64
	initialized atomic.Bool
}

func serve(input io.Reader, output io.Writer) error {
	server := &server{output: output, jobs: make(map[string]context.CancelFunc), hostCalls: make(map[string]pendingHostCall)}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), maxFrameSize)
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		var header struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(line, &header); err != nil || header.ID == "" && header.Method != "cancel" {
			server.cancelAll()
			server.jobsWG.Wait()
			return fmt.Errorf("decoding protocol frame")
		}
		if header.Method == "" {
			var response responseFrame
			if err := decodeStrict(line, &response); err != nil || response.Error == nil && response.Result == nil || response.Error != nil && response.Result != nil {
				return fmt.Errorf("host callback response must contain exactly one of result or error")
			}
			if !server.deliverHostResponse(response) {
				return fmt.Errorf("host sent unknown callback response id %q", response.ID)
			}
			continue
		}
		var request requestFrame
		if err := decodeStrict(line, &request); err != nil {
			return fmt.Errorf("decoding host request: %w", err)
		}
		switch request.Method {
		case "cancel":
			var params struct {
				ID string `json:"id"`
			}
			if err := decodeRawStrict(request.Params, &params); err != nil || params.ID == "" {
				return fmt.Errorf("decoding cancel notification")
			}
			server.cancel(params.ID)
		case "shutdown":
			server.cancelAll()
			server.jobsWG.Wait()
			return server.write(outgoingResponseFrame{ID: request.ID, Result: map[string]any{}})
		default:
			if err := server.start(request); err != nil {
				if writeErr := server.write(outgoingResponseFrame{ID: request.ID, Error: &wireError{Code: "invalid_request", Message: err.Error()}}); writeErr != nil {
					return writeErr
				}
			}
		}
	}
	server.cancelAll()
	server.jobsWG.Wait()
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading protocol: %w", err)
	}
	return nil
}

func (server *server) start(request requestFrame) error {
	if request.ID == "" {
		return fmt.Errorf("request ID is required for method %q", request.Method)
	}
	if request.Method != "initialize" && !server.initialized.Load() {
		return fmt.Errorf("plugin is not initialized")
	}
	ctx, cancel := context.WithCancel(context.Background())
	server.jobsMu.Lock()
	if _, exists := server.jobs[request.ID]; exists {
		server.jobsMu.Unlock()
		cancel()
		return fmt.Errorf("request ID %q is already active", request.ID)
	}
	server.jobs[request.ID] = cancel
	server.jobsMu.Unlock()
	server.jobsWG.Go(func() {
		defer func() {
			cancel()
			server.jobsMu.Lock()
			delete(server.jobs, request.ID)
			server.jobsMu.Unlock()
		}()
		result, err := server.dispatch(ctx, request)
		response := outgoingResponseFrame{ID: request.ID, Result: result}
		if err != nil {
			response.Result = nil
			code := "step_failed"
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				code = "canceled"
			}
			response.Error = &wireError{Code: code, Message: err.Error()}
		}
		_ = server.write(response)
	})
	return nil
}

func (server *server) dispatch(ctx context.Context, request requestFrame) (any, error) {
	switch request.Method {
	case "initialize":
		var params struct {
			Protocol    string `json:"protocol"`
			HostVersion string `json:"host_version,omitempty"`
		}
		if err := decodeRawStrict(request.Params, &params); err != nil {
			return nil, fmt.Errorf("decoding initialize parameters: %w", err)
		}
		if params.Protocol != protocolVersion {
			return nil, fmt.Errorf("unsupported protocol %q", params.Protocol)
		}
		if !server.initialized.CompareAndSwap(false, true) {
			return nil, fmt.Errorf("plugin is already initialized")
		}
		callbacks := []string{"host.template.validate", "host.template.render", "host.function.call"}
		return map[string]any{
			"protocol":  protocolVersion,
			"namespace": namespace,
			"steps": []any{
				map[string]any{"type": "http.forward_proxy", "cleanup": true, "service": true, "host_callbacks": callbacks},
				map[string]any{"type": "http.mock_server", "cleanup": true, "service": true, "host_callbacks": callbacks},
			},
			"executors": []any{},
			"helpers":   []any{},
		}, nil
	case "step.validate":
		params, runner, err := decodeStep(request.Params)
		if err != nil {
			return nil, err
		}
		if validator, ok := runner.(step.Validator); ok {
			if err := validator.Validate(ctx, server.stepRequest(ctx, request.ID, params.Context)); err != nil {
				return nil, err
			}
		}
		return map[string]any{}, nil
	case "step.service":
		_, runner, err := decodeStep(request.Params)
		if err != nil {
			return nil, err
		}
		serviceRunner, ok := runner.(managedServiceRunner)
		if !ok {
			return nil, fmt.Errorf("step is not a managed service")
		}
		kind, options := serviceRunner.ServiceOptions()
		return map[string]any{"kind": kind, "keep_alive": options.KeepAlive, "fail_fast": options.FailFast, "exit_on_end": options.ExitOnEnd}, nil
	case "step.run":
		params, runner, err := decodeStep(request.Params)
		if err != nil {
			return nil, err
		}
		serviceRunner, ok := runner.(managedServiceRunner)
		if !ok {
			return nil, fmt.Errorf("step is not a managed service")
		}
		kind, options := serviceRunner.ServiceOptions()
		launcher := &capturingServices{kind: kind, options: options}
		execution := server.stepRequest(ctx, request.ID, params.Context)
		execution.Services = launcher
		result, err := runner.Run(ctx, execution)
		if err != nil {
			return nil, err
		}
		service, err := launcher.service()
		if err != nil {
			return nil, err
		}
		// Start the captured service against an uncanceled context before publishing readiness.
		// The parent cancellation is relayed only after the ready frame has been written. This
		// preserves the native StartService handoff: an immediate scope cancellation must stop a
		// committed service and run its final verification, not race its pre-commit abort path.
		serviceCtx, stopService := context.WithCancel(context.WithoutCancel(ctx))
		serviceStarted := make(chan struct{})
		serviceDone := make(chan error, 1)
		go func() {
			close(serviceStarted)
			serviceDone <- service(serviceCtx)
		}()
		<-serviceStarted
		wire := encodeResult(result)
		if err := server.write(eventFrame{ID: request.ID, Event: "ready", Result: wire}); err != nil {
			stopService()
			return nil, errors.Join(err, <-serviceDone)
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
	case "step.cleanup":
		var params struct {
			Type   string          `json:"type"`
			With   json.RawMessage `json:"with"`
			Result wireResult      `json:"result"`
		}
		if err := decodeRawStrict(request.Params, &params); err != nil {
			return nil, fmt.Errorf("decoding cleanup parameters: %w", err)
		}
		runner, err := buildRunner(params.Type, params.With)
		if err != nil {
			return nil, err
		}
		cleaner, ok := runner.(step.Cleaner)
		if !ok {
			return nil, fmt.Errorf("step does not support cleanup")
		}
		if err := cleaner.Cleanup(ctx, step.Result{Outputs: params.Result.Outputs, Variables: params.Result.Variables}); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	default:
		return nil, fmt.Errorf("unknown method %q", request.Method)
	}
}

type managedServiceRunner interface {
	step.Runner
	ServiceOptions() (string, step.ServiceOptions)
}

func decodeStep(raw json.RawMessage) (stepParams, step.Runner, error) {
	var params stepParams
	if err := decodeRawStrict(raw, &params); err != nil {
		return stepParams{}, nil, fmt.Errorf("decoding step parameters: %w", err)
	}
	runner, err := buildRunner(params.Type, params.With)
	if err != nil {
		return stepParams{}, nil, err
	}
	return params, runner, nil
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

func (server *server) stepRequest(ctx context.Context, parentID string, value stepContext) step.Request {
	helpers := make(helper.Set, len(value.Helpers))
	for _, name := range value.Helpers {
		name := name
		helpers[name] = func(callCtx context.Context, args []any) (any, error) {
			return server.callFunction(callCtx, parentID, name, args)
		}
	}
	renderer := &hostRenderer{server: server, ctx: ctx, parentID: parentID}
	request := step.Request{
		StepID: value.StepID, WorkflowName: value.WorkflowName, WorkflowSource: value.WorkflowSource,
		WorkflowDir: value.WorkflowDir, WorkflowDirBorrowed: value.WorkflowDirBorrowed, WorkflowTimezone: value.WorkflowTimezone,
		RunDir: value.RunDir, EnvironmentLoaders: value.EnvironmentLoaders, LocalValueDir: value.LocalValueDir, GlobalValueDir: value.GlobalValueDir,
		Vars: normalizeMap(value.Vars), PresetVars: normalizeMap(value.PresetVars), Inputs: normalizeMap(value.Inputs), Env: value.Env, Steps: normalizeMap(value.Steps),
		Dependencies: normalizeNestedMap(value.Dependencies), Bindings: normalizeMap(value.Bindings), Providers: provider.Set{Values: normalizeNestedMap(value.Providers)},
		Stdout: eventWriter{server: server, id: parentID, event: "stdout"}, Stderr: eventWriter{server: server, id: parentID, event: "stderr"},
		Attempt: value.Attempt, MaxAttempts: value.MaxAttempts, OperationID: value.OperationID,
		TemplateRenderer: renderer, Helpers: helpers, HelperContext: ctx,
	}
	request.Secret = func(reference string) (string, error) {
		value, err := server.callFunction(ctx, parentID, "secret", []any{reference})
		if err != nil {
			return "", err
		}
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("secret callback returned %T, expected string", value)
		}
		return text, nil
	}
	if value.PreviousAttempt != nil {
		request.PreviousAttempt = &step.Result{Outputs: normalizeMap(value.PreviousAttempt.Outputs), Variables: normalizeMap(value.PreviousAttempt.Variables)}
	}
	return request
}

// Protocol JSON has one number token type, while Wuko's in-process workflow data uses Go ints
// and floats. Restore that shape before feeding values to Expr so comparisons behave identically
// on either side of the plugin boundary without sacrificing integers beyond int64.
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

func normalizeValue(value any) any {
	switch typed := value.(type) {
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

type eventWriter struct {
	server *server
	id     string
	event  string
}

func (writer eventWriter) Write(data []byte) (int, error) {
	if err := writer.server.write(eventFrame{ID: writer.id, Event: writer.event, Data: base64.StdEncoding.EncodeToString(data)}); err != nil {
		return 0, err
	}
	return len(data), nil
}

type hostRenderer struct {
	server   *server
	ctx      context.Context
	parentID string
}

func (renderer *hostRenderer) Validate(value string) error { return renderer.ValidateContent(value) }
func (renderer *hostRenderer) Render(value string) (string, error) {
	return renderer.RenderContent(value)
}
func (renderer *hostRenderer) ValidateContent(value string) error {
	return renderer.server.callHost(renderer.ctx, renderer.parentID, "host.template.validate", map[string]any{"content": value}, &struct{}{})
}
func (renderer *hostRenderer) RenderContent(value string) (string, error) {
	return renderer.RenderContentWith(value, nil)
}
func (renderer *hostRenderer) RenderWith(value string, extra map[string]any) (string, error) {
	return renderer.RenderContentWith(value, extra)
}
func (renderer *hostRenderer) RenderContentWith(value string, extra map[string]any) (string, error) {
	var result struct {
		Value string `json:"value"`
	}
	err := renderer.server.callHost(renderer.ctx, renderer.parentID, "host.template.render", map[string]any{"content": value, "extra": extra}, &result)
	return result.Value, err
}
func (renderer *hostRenderer) Snapshot() step.DataTemplateRenderer       { return renderer }
func (renderer *hostRenderer) WithoutSecrets() step.DataTemplateRenderer { return renderer }

func (server *server) callFunction(ctx context.Context, parentID, name string, args []any) (any, error) {
	var result struct {
		Value any `json:"value"`
	}
	if err := server.callHost(ctx, parentID, "host.function.call", map[string]any{"name": name, "args": args}, &result); err != nil {
		return nil, err
	}
	return normalizeValue(result.Value), nil
}

func (server *server) callHost(ctx context.Context, parentID, method string, params map[string]any, result any) error {
	id := fmt.Sprintf("plugin-%d", server.nextHostID.Add(1))
	params["parent_id"] = parentID
	response := make(chan responseFrame, 1)
	server.hostMu.Lock()
	server.hostCalls[id] = pendingHostCall{response: response}
	server.hostMu.Unlock()
	if err := server.write(outgoingRequestFrame{ID: id, Method: method, Params: params}); err != nil {
		server.removeHostCall(id)
		return err
	}
	select {
	case frame := <-response:
		if frame.Error != nil {
			return errors.New(frame.Error.Message)
		}
		if result != nil {
			if err := decodeRaw(frame.Result, result); err != nil {
				return fmt.Errorf("decoding host callback response: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		server.abandonHostCall(id)
		return ctx.Err()
	}
}

func (server *server) removeHostCall(id string) {
	server.hostMu.Lock()
	delete(server.hostCalls, id)
	server.hostMu.Unlock()
}

func (server *server) abandonHostCall(id string) {
	server.hostMu.Lock()
	if pending, ok := server.hostCalls[id]; ok {
		pending.abandoned = true
		server.hostCalls[id] = pending
	}
	server.hostMu.Unlock()
}

func (server *server) deliverHostResponse(frame responseFrame) bool {
	server.hostMu.Lock()
	pending, ok := server.hostCalls[frame.ID]
	if ok {
		delete(server.hostCalls, frame.ID)
	}
	server.hostMu.Unlock()
	if !ok {
		return false
	}
	if !pending.abandoned {
		pending.response <- frame
	}
	return true
}

func (server *server) cancel(id string) {
	server.jobsMu.Lock()
	cancel := server.jobs[id]
	server.jobsMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (server *server) cancelAll() {
	server.jobsMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(server.jobs))
	for _, cancel := range server.jobs {
		cancels = append(cancels, cancel)
	}
	server.jobsMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (server *server) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encoding protocol frame: %w", err)
	}
	if len(data) > maxFrameSize {
		return fmt.Errorf("protocol frame exceeds 10 MiB")
	}
	server.writeMu.Lock()
	defer server.writeMu.Unlock()
	if _, err := server.output.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("writing protocol frame: %w", err)
	}
	return nil
}

func encodeResult(result step.Result) wireResult {
	return wireResult{Outputs: result.Outputs, Variables: result.Variables}
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("protocol frame contains trailing data")
	}
	return nil
}

func decodeRawStrict(data json.RawMessage, target any) error {
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	return decodeStrict(data, target)
}

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
