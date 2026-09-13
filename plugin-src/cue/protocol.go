package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/mod/semver"
)

const (
	protocolVersion = "wuko.plugin/v1"
	namespace       = "cue"
	stepType        = "cue.eval"
	maxFrameSize    = 10 << 20
	minHostVersion  = "v0.14.0"
)

type request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type response struct {
	ID     string    `json:"id"`
	Result any       `json:"result,omitempty"`
	Error  *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type cancelParams struct {
	ID string `json:"id"`
}

type initializeParams struct {
	Protocol    string `json:"protocol"`
	HostVersion string `json:"host_version,omitempty"`
}

type stepParams struct {
	Type    string          `json:"type"`
	With    json.RawMessage `json:"with"`
	Context stepContext     `json:"context"`
}

type stepContext struct {
	StepID       string         `json:"step_id"`
	WorkflowName string         `json:"workflow_name"`
	WorkflowDir  string         `json:"workflow_dir"`
	RunDir       string         `json:"run_dir"`
	Vars         map[string]any `json:"vars"`
	Inputs       map[string]any `json:"inputs"`
	Env          map[string]any `json:"env"`
	Steps        map[string]any `json:"steps"`
	Dependencies map[string]any `json:"dependencies"`
	Attempt      int            `json:"attempt"`
	MaxAttempts  int            `json:"max_attempts"`
	OperationID  string         `json:"operation_id"`
}

type workerFunc func(context.Context, workerJob) (json.RawMessage, error)

type server struct {
	worker workerFunc
	writer *json.Encoder

	writeMu sync.Mutex
	jobsMu  sync.Mutex
	jobs    map[string]context.CancelFunc
	jobsWG  sync.WaitGroup

	runtimeAllowed atomic.Bool
}

func serve(input io.Reader, output io.Writer, worker workerFunc) error {
	s := &server{worker: worker, writer: json.NewEncoder(output), jobs: make(map[string]context.CancelFunc)}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), maxFrameSize)
	for scanner.Scan() {
		var request request
		if err := decodeStrict(scanner.Bytes(), &request); err != nil {
			s.cancelAll()
			s.jobsWG.Wait()
			return fmt.Errorf("decoding request: %w", err)
		}
		if request.Method == "cancel" {
			var params cancelParams
			if err := decodeRawStrict(request.Params, &params); err != nil {
				return fmt.Errorf("decoding cancel notification: %w", err)
			}
			s.cancel(params.ID)
			continue
		}
		if request.ID == "" {
			return fmt.Errorf("request ID is required for method %q", request.Method)
		}
		if request.Method == "shutdown" {
			s.cancelAll()
			s.jobsWG.Wait()
			return s.write(response{ID: request.ID, Result: map[string]any{}})
		}
		if err := s.start(request); err != nil {
			if writeErr := s.write(response{ID: request.ID, Error: &rpcError{Code: "invalid_request", Message: err.Error()}}); writeErr != nil {
				return writeErr
			}
		}
	}
	s.cancelAll()
	s.jobsWG.Wait()
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading request: %w", err)
	}
	return nil
}

func (s *server) start(request request) error {
	ctx, cancel := context.WithCancel(context.Background())
	s.jobsMu.Lock()
	if _, exists := s.jobs[request.ID]; exists {
		s.jobsMu.Unlock()
		cancel()
		return fmt.Errorf("request ID %q is already in flight", request.ID)
	}
	s.jobs[request.ID] = cancel
	s.jobsMu.Unlock()

	s.jobsWG.Go(func() {
		defer func() {
			cancel()
			s.jobsMu.Lock()
			delete(s.jobs, request.ID)
			s.jobsMu.Unlock()
		}()
		result, err := s.dispatch(ctx, request)
		reply := response{ID: request.ID, Result: result}
		if err != nil {
			reply.Result = nil
			code := "evaluation_failed"
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				code = "canceled"
			}
			reply.Error = &rpcError{Code: code, Message: err.Error()}
		}
		_ = s.write(reply)
	})
	return nil
}

func (s *server) dispatch(ctx context.Context, request request) (any, error) {
	switch request.Method {
	case "initialize":
		var params initializeParams
		if err := decodeRawStrict(request.Params, &params); err != nil {
			return nil, fmt.Errorf("decoding initialize parameters: %w", err)
		}
		if params.Protocol != protocolVersion {
			return nil, fmt.Errorf("unsupported protocol %q", params.Protocol)
		}
		allowed, err := validateHostVersion(params.HostVersion)
		if err != nil {
			return nil, err
		}
		s.runtimeAllowed.Store(allowed)
		return map[string]any{
			"protocol": protocolVersion, "namespace": namespace,
			"steps": []any{map[string]any{"type": stepType}}, "executors": []any{}, "helpers": []any{},
		}, nil
	case "step.validate":
		if !s.runtimeAllowed.Load() {
			return nil, minimumHostVersionError()
		}
		params, configuration, err := decodeStep(request.Params)
		if err != nil {
			return nil, err
		}
		if configuration.validationMustBeDeferred() {
			return map[string]any{}, nil
		}
		_, err = s.worker(ctx, workerJob{Mode: workerModeValidate, Configuration: configuration, Context: params.Context})
		if err != nil {
			return nil, redactError(err, params.Context.Env)
		}
		return map[string]any{}, nil
	case "step.run":
		if !s.runtimeAllowed.Load() {
			return nil, minimumHostVersionError()
		}
		params, configuration, err := decodeStep(request.Params)
		if err != nil {
			return nil, err
		}
		value, err := s.worker(ctx, workerJob{Mode: workerModeRun, Configuration: configuration, Context: params.Context})
		if err != nil {
			return nil, redactError(err, params.Context.Env)
		}
		return map[string]any{"outputs": map[string]any{"value": value}}, nil
	default:
		return nil, fmt.Errorf("unknown method %q", request.Method)
	}
}

func validateHostVersion(version string) (bool, error) {
	if version == "" {
		return false, nil
	}
	if version == "dev" {
		return true, nil
	}
	release, ok := releaseVersion(version)
	if !ok {
		// An unrecognized version must not fail the handshake. Runtime operations still
		// report the documented minimum-version error.
		return false, nil
	}
	if semver.Compare(release, minHostVersion) < 0 {
		return false, minimumHostVersionError()
	}
	return true, nil
}

// releaseVersion reduces a Wuko version to the release it derives from. Release builds report
// "vX.Y.Z", while development builds report "git describe" output such as "vX.Y.Z-12-gabc1234",
// which is newer than the vX.Y.Z tag it was described from. Semver orders any prerelease before
// its release, so the suffix is dropped before comparing.
func releaseVersion(version string) (string, bool) {
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	if !semver.IsValid(version) {
		return "", false
	}
	canonical := semver.Canonical(version)
	return strings.TrimSuffix(canonical, semver.Prerelease(canonical)), true
}

func minimumHostVersionError() error {
	return fmt.Errorf("cue plugin v0.2 requires Wuko %s or newer", minHostVersion)
}

func decodeStep(raw json.RawMessage) (stepParams, config, error) {
	var params stepParams
	if err := decodeRawStrict(raw, &params); err != nil {
		return stepParams{}, config{}, fmt.Errorf("decoding step parameters: %w", err)
	}
	if params.Type != stepType {
		return stepParams{}, config{}, fmt.Errorf("unsupported step type %q", params.Type)
	}
	configuration, err := decodeConfig(params.With)
	if err != nil {
		return stepParams{}, config{}, err
	}
	return params, configuration, nil
}

func (s *server) cancel(id string) {
	s.jobsMu.Lock()
	cancel := s.jobs[id]
	s.jobsMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *server) cancelAll() {
	s.jobsMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.jobs))
	for _, cancel := range s.jobs {
		cancels = append(cancels, cancel)
	}
	s.jobsMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *server) write(reply response) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.writer.Encode(reply)
}

func decodeStrict(data []byte, target any) error {
	return decodeReaderStrict(data, target)
}

func decodeRawStrict(data json.RawMessage, target any) error {
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	return decodeReaderStrict(data, target)
}

func decodeReaderStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("multiple JSON values are not supported")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
