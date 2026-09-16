package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	protocolVersion = "wuko.plugin/v1"
	namespace       = "jwt"
	pluginVersion   = "0.1.0"
	minHostVersion  = "0.14.0"
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

type cancelParams struct {
	ID string `json:"id"`
}

type initializeParams struct {
	Protocol    string `json:"protocol"`
	HostVersion string `json:"host_version,omitempty"`
}

type helperParams struct {
	Name string            `json:"name"`
	Args []json.RawMessage `json:"args"`
}

type stepParams struct {
	Type    string          `json:"type"`
	With    json.RawMessage `json:"with"`
	Context json.RawMessage `json:"context"`
}

type server struct {
	service *service
	writer  *json.Encoder

	writeMu sync.Mutex
	jobsMu  sync.Mutex
	jobs    map[string]context.CancelFunc
	jobsWG  sync.WaitGroup

	runtimeAllowed atomic.Bool
}

func serve(input io.Reader, output io.Writer, service *service) error {
	s := &server{service: service, writer: json.NewEncoder(output), jobs: make(map[string]context.CancelFunc)}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), maxFrameSize)
	for scanner.Scan() {
		var current request
		if err := decodeProtocolFrame(scanner.Bytes(), &current); err != nil {
			s.cancelAll()
			s.jobsWG.Wait()
			return fmt.Errorf("decoding request: %w", err)
		}
		if current.Method == "cancel" {
			var params cancelParams
			if err := decodeProtocolRaw(current.Params, &params); err != nil {
				return fmt.Errorf("decoding cancel notification: %w", err)
			}
			s.cancel(params.ID)
			continue
		}
		if current.ID == "" {
			return fmt.Errorf("request ID is required for method %q", current.Method)
		}
		if current.Method == "shutdown" {
			s.cancelAll()
			s.jobsWG.Wait()
			return s.write(response{ID: current.ID, Result: map[string]any{}})
		}
		if err := s.start(current); err != nil {
			if writeErr := s.write(response{ID: current.ID, Error: &rpcError{Code: "invalid_request", Message: err.Error()}}); writeErr != nil {
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

func (s *server) start(current request) error {
	ctx, cancel := context.WithCancel(context.Background())
	s.jobsMu.Lock()
	if _, exists := s.jobs[current.ID]; exists {
		s.jobsMu.Unlock()
		cancel()
		return fmt.Errorf("request ID %q is already in flight", current.ID)
	}
	s.jobs[current.ID] = cancel
	s.jobsMu.Unlock()
	s.jobsWG.Go(func() {
		defer func() {
			cancel()
			s.jobsMu.Lock()
			delete(s.jobs, current.ID)
			s.jobsMu.Unlock()
		}()
		result, err := s.dispatch(ctx, current)
		reply := response{ID: current.ID, Result: result}
		if err != nil {
			reply.Result = nil
			code := "operation_failed"
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				code = "canceled"
			}
			reply.Error = &rpcError{Code: code, Message: err.Error()}
		}
		_ = s.write(reply)
	})
	return nil
}

func (s *server) dispatch(ctx context.Context, current request) (any, error) {
	switch current.Method {
	case "initialize":
		var params initializeParams
		if err := decodeProtocolRaw(current.Params, &params); err != nil {
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
			"steps": []any{
				map[string]any{"type": "jwt.generate"}, map[string]any{"type": "jwt.jwks"},
				map[string]any{"type": "jwt.verify"}, map[string]any{"type": "jwt.is_valid"},
			},
			"executors": []any{},
			"helpers": []any{
				map[string]any{"name": "generate"}, map[string]any{"name": "jwks"},
				map[string]any{"name": "verify"}, map[string]any{"name": "is_valid"},
			},
		}, nil
	case "helper.call":
		if err := s.requireRuntime(); err != nil {
			return nil, err
		}
		var params helperParams
		if err := decodeProtocolRaw(current.Params, &params); err != nil {
			return nil, fmt.Errorf("decoding helper parameters: %w", err)
		}
		value, err := s.callHelper(ctx, params)
		if err != nil {
			return nil, err
		}
		return map[string]any{"value": value}, nil
	case "step.validate", "step.run":
		if err := s.requireRuntime(); err != nil {
			return nil, err
		}
		var params stepParams
		if err := decodeProtocolRaw(current.Params, &params); err != nil {
			return nil, fmt.Errorf("decoding step parameters: %w", err)
		}
		validateOnly := current.Method == "step.validate"
		outputs, err := s.callStep(ctx, params.Type, params.With, validateOnly)
		if err != nil {
			return nil, err
		}
		if validateOnly {
			return map[string]any{}, nil
		}
		return map[string]any{"outputs": outputs}, nil
	default:
		return nil, fmt.Errorf("unknown method %q", current.Method)
	}
}

func (s *server) requireRuntime() error {
	if !s.runtimeAllowed.Load() {
		return fmt.Errorf("jwt plugin v%s requires Wuko %s or newer", pluginVersion, minHostVersion)
	}
	return nil
}

func validateHostVersion(version string) (bool, error) {
	if version == "" {
		return false, nil
	}
	if version == "dev" {
		return true, nil
	}
	current, ok := parseVersion(version)
	if !ok {
		return false, nil
	}
	minimum, _ := parseVersion(minHostVersion)
	if current < minimum {
		return false, fmt.Errorf("jwt plugin v%s requires Wuko %s or newer", pluginVersion, minHostVersion)
	}
	return true, nil
}

func parseVersion(version string) (int64, bool) {
	version = strings.TrimPrefix(version, "v")
	version, _, _ = strings.Cut(version, "+")
	core, _, _ := strings.Cut(version, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return 0, false
	}
	values := make([]int64, 3)
	for index, part := range parts {
		value, err := strconv.ParseInt(part, 10, 20)
		if err != nil || value < 0 {
			return 0, false
		}
		values[index] = value
	}
	return values[0]<<40 | values[1]<<20 | values[2], true
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

func decodeProtocolFrame(data []byte, target any) error {
	if err := validateJSON(data); err != nil {
		return err
	}
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

func decodeProtocolRaw(data json.RawMessage, target any) error {
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	return decodeProtocolFrame(data, target)
}
