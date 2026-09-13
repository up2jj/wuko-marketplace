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
	"sync"
)

const (
	protocolVersion = "wuko.plugin/v1"
	namespace       = "local-notifier"
	stepType        = "local-notifier.notify"
	maxFrameSize    = 10 << 20
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

type event struct {
	ID    string `json:"id"`
	Event string `json:"event"`
	Data  string `json:"data,omitempty"`
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

type server struct {
	notify notifyFunc
	writer *json.Encoder

	writeMu sync.Mutex
	jobsMu  sync.Mutex
	jobs    map[string]context.CancelFunc
	jobsWG  sync.WaitGroup
}

func serve(input io.Reader, output io.Writer, notify notifyFunc) error {
	s := &server{notify: notify, writer: json.NewEncoder(output), jobs: make(map[string]context.CancelFunc)}
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
			code := "notification_failed"
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
		return map[string]any{
			"protocol":  protocolVersion,
			"namespace": namespace,
			"steps":     []any{map[string]any{"type": stepType}},
			"executors": []any{},
			"helpers":   []any{},
		}, nil
	case "step.validate":
		if _, _, err := decodeStep(request.Params, true); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	case "step.run":
		params, configuration, err := decodeStep(request.Params, false)
		if err != nil {
			return nil, err
		}
		result, err := s.notify(ctx, configuration, params.Context.WorkflowName, func(data []byte) error {
			return s.write(event{ID: request.ID, Event: "stdout", Data: base64.StdEncoding.EncodeToString(data)})
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"outputs": map[string]any{"delivery": result.Delivery, "fallback": result.Fallback}}, nil
	default:
		return nil, fmt.Errorf("unknown method %q", request.Method)
	}
}

func decodeStep(raw json.RawMessage, allowTemplates bool) (stepParams, config, error) {
	var params stepParams
	if err := decodeRawStrict(raw, &params); err != nil {
		return stepParams{}, config{}, fmt.Errorf("decoding step parameters: %w", err)
	}
	if params.Type != stepType {
		return stepParams{}, config{}, fmt.Errorf("unsupported step type %q", params.Type)
	}
	configuration, err := decodeConfig(params.With, allowTemplates)
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

func (s *server) write(value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.writer.Encode(value)
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
