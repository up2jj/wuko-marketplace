package mockserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/up2jj/wuko/step"
	"gopkg.in/yaml.v3"
)

func (runner *Runner) loadInitialState(request step.Request, renderer step.TemplateRenderer, mode loadMode) (map[string]any, error) {
	// An inline initial_state needs no work here: the engine renders every step's configuration
	// before it builds the runner, so the value already holds its final text. Rendering it again
	// would expand a second round of delimiters that arrived from workflow data.
	value := runner.config.InitialState
	if runner.config.InitialStateFile != "" {
		path := runner.config.InitialStateFile
		if mode == loadValidate && containsTemplate(path) {
			// The path still holds template text, so it cannot name a file until the run renders it.
			return map[string]any{}, nil
		}
		if filepath.IsAbs(path) {
			return nil, fmt.Errorf("initial_state_file must be relative")
		}
		cleaned := filepath.Clean(filepath.FromSlash(path))
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("initial_state_file must not escape the workflow package")
		}
		path = filepath.Clean(filepath.Join(request.WorkflowDir, cleaned))
		root, err := filepath.EvalSymlinks(request.WorkflowDir)
		if err != nil {
			return nil, err
		}
		if err := ensureWithin(root, path); err != nil {
			return nil, fmt.Errorf("initial_state_file: %w", err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("initial_state_file %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("initial_state_file %s must be a regular non-symlink file", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading initial_state_file %s: %w", path, err)
		}
		var decoded any
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		if err := decoder.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("decoding initial_state_file %s: %w", path, err)
		}
		var extra any
		if err := decoder.Decode(&extra); err == nil {
			return nil, fmt.Errorf("decoding initial_state_file %s: multiple YAML documents are not supported", path)
		} else if !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("decoding initial_state_file %s: %w", path, err)
		}
		// File content never passes through the engine, so the step renders it itself: parse-checked
		// during validation, when no step has produced output yet, and rendered once at run.
		resolved, err := renderStateFile(decoded, renderer, mode)
		if err != nil {
			return nil, fmt.Errorf("initial state: %w", err)
		}
		value = resolved
	}
	if value == nil {
		value = map[string]any{}
	}
	state, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("initial state must be an object")
	}
	if _, err := json.Marshal(state); err != nil {
		return nil, fmt.Errorf("initial state is not JSON-compatible: %w", err)
	}
	return cloneObject(state), nil
}

// renderStateFile renders the strings of a decoded initial_state_file. It takes the plain
// TemplateRenderer rather than the data-aware one because seed state is read once, before the
// server accepts traffic, and has no request of its own to overlay.
func renderStateFile(value any, renderer step.TemplateRenderer, mode loadMode) (any, error) {
	switch typed := value.(type) {
	case string:
		if renderer == nil {
			return typed, nil
		}
		if mode == loadValidate {
			if err := renderer.ValidateContent(typed); err != nil {
				return nil, err
			}
			return typed, nil
		}
		return renderer.RenderContent(typed)
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			resolved, err := renderStateFile(item, renderer, mode)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", index, err)
			}
			result[index] = resolved
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for name, item := range typed {
			resolved, err := renderStateFile(item, renderer, mode)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", name, err)
			}
			result[name] = resolved
		}
		return result, nil
	default:
		return value, nil
	}
}

func applyPatches(expectation *compiledExpectation, execution step.Request, request requestValue, state map[string]any, renderer step.DataTemplateRenderer) (map[string]any, error) {
	// A read-only expectation leaves the state untouched, so neither the deep copy nor the
	// JSON round-trip below buys anything: both are proportional to the whole state and would
	// otherwise run on every request a stateful mock serves.
	if len(expectation.patches) == 0 {
		return state, nil
	}
	candidate := cloneObject(state)
	requestCopy := request
	environment := requestEnvironment(execution, &requestCopy, state)
	extra := map[string]any{"request": request.templateValue(), "state": state}
	for index, patch := range expectation.patches {
		path, err := renderer.RenderContentWith(patch.config.Path, extra)
		if err != nil {
			return nil, fmt.Errorf("update[%d] path: %w", index, err)
		}
		tokens, err := parseJSONPointer(path)
		if err != nil {
			return nil, fmt.Errorf("update[%d] path: %w", index, err)
		}
		if len(tokens) == 0 {
			return nil, fmt.Errorf("update[%d] cannot modify the state root", index)
		}
		switch patch.config.Op {
		case "set":
			value, err := patch.value.resolve(environment, renderer, extra)
			if err != nil {
				return nil, fmt.Errorf("update[%d] value: %w", index, err)
			}
			updated, err := setPointer(candidate, tokens, cloneJSON(value), true)
			if err != nil {
				return nil, fmt.Errorf("update[%d]: %w", index, err)
			}
			candidate = updated.(map[string]any)
		case "append":
			value, err := patch.value.resolve(environment, renderer, extra)
			if err != nil {
				return nil, fmt.Errorf("update[%d] value: %w", index, err)
			}
			current, err := getPointer(candidate, tokens)
			if err != nil {
				return nil, fmt.Errorf("update[%d]: %w", index, err)
			}
			list, ok := current.([]any)
			if !ok {
				return nil, fmt.Errorf("update[%d] append target must be an array", index)
			}
			updated, err := setPointer(candidate, tokens, append(list, cloneJSON(value)), false)
			if err != nil {
				return nil, fmt.Errorf("update[%d]: %w", index, err)
			}
			candidate = updated.(map[string]any)
		case "remove":
			updated, err := removePointer(candidate, tokens)
			if err != nil {
				return nil, fmt.Errorf("update[%d]: %w", index, err)
			}
			candidate = updated.(map[string]any)
		}
	}
	if _, err := json.Marshal(candidate); err != nil {
		return nil, fmt.Errorf("updated state is not JSON-compatible: %w", err)
	}
	return candidate, nil
}

func parseJSONPointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("JSON Pointer must start with /")
	}
	parts := strings.Split(pointer[1:], "/")
	for index, part := range parts {
		var decoded strings.Builder
		for offset := 0; offset < len(part); offset++ {
			if part[offset] != '~' {
				decoded.WriteByte(part[offset])
				continue
			}
			if offset+1 >= len(part) || part[offset+1] != '0' && part[offset+1] != '1' {
				return nil, fmt.Errorf("invalid JSON Pointer escape in %q", part)
			}
			offset++
			if part[offset] == '0' {
				decoded.WriteByte('~')
			} else {
				decoded.WriteByte('/')
			}
		}
		parts[index] = decoded.String()
	}
	return parts, nil
}

func getPointer(node any, tokens []string) (any, error) {
	current := node
	for _, token := range tokens {
		switch typed := current.(type) {
		case map[string]any:
			value, exists := typed[token]
			if !exists {
				return nil, fmt.Errorf("state path does not exist")
			}
			current = value
		case []any:
			index, err := pointerIndex(token, len(typed))
			if err != nil {
				return nil, err
			}
			current = typed[index]
		default:
			return nil, fmt.Errorf("state path traverses a scalar")
		}
	}
	return current, nil
}

func setPointer(node any, tokens []string, value any, create bool) (any, error) {
	token := tokens[0]
	if len(tokens) == 1 {
		switch typed := node.(type) {
		case map[string]any:
			typed[token] = value
			return typed, nil
		case []any:
			index, err := pointerIndex(token, len(typed))
			if err != nil {
				return nil, err
			}
			typed[index] = value
			return typed, nil
		default:
			return nil, fmt.Errorf("state path parent is not an object or array")
		}
	}
	switch typed := node.(type) {
	case map[string]any:
		child, exists := typed[token]
		if !exists {
			if !create {
				return nil, fmt.Errorf("state path does not exist")
			}
			child = map[string]any{}
		}
		updated, err := setPointer(child, tokens[1:], value, create)
		if err != nil {
			return nil, err
		}
		typed[token] = updated
		return typed, nil
	case []any:
		index, err := pointerIndex(token, len(typed))
		if err != nil {
			return nil, err
		}
		updated, err := setPointer(typed[index], tokens[1:], value, create)
		if err != nil {
			return nil, err
		}
		typed[index] = updated
		return typed, nil
	default:
		return nil, fmt.Errorf("state path traverses a scalar")
	}
}

func removePointer(node any, tokens []string) (any, error) {
	token := tokens[0]
	if len(tokens) == 1 {
		switch typed := node.(type) {
		case map[string]any:
			if _, exists := typed[token]; !exists {
				return nil, fmt.Errorf("state path does not exist")
			}
			delete(typed, token)
			return typed, nil
		case []any:
			index, err := pointerIndex(token, len(typed))
			if err != nil {
				return nil, err
			}
			return append(typed[:index], typed[index+1:]...), nil
		default:
			return nil, fmt.Errorf("state path parent is not an object or array")
		}
	}
	switch typed := node.(type) {
	case map[string]any:
		child, exists := typed[token]
		if !exists {
			return nil, fmt.Errorf("state path does not exist")
		}
		updated, err := removePointer(child, tokens[1:])
		if err != nil {
			return nil, err
		}
		typed[token] = updated
		return typed, nil
	case []any:
		index, err := pointerIndex(token, len(typed))
		if err != nil {
			return nil, err
		}
		updated, err := removePointer(typed[index], tokens[1:])
		if err != nil {
			return nil, err
		}
		typed[index] = updated
		return typed, nil
	default:
		return nil, fmt.Errorf("state path traverses a scalar")
	}
}

func pointerIndex(value string, length int) (int, error) {
	index, err := strconv.Atoi(value)
	if err != nil || index < 0 || index >= length {
		return 0, fmt.Errorf("array index %q is out of range", value)
	}
	return index, nil
}

func cloneObject(source map[string]any) map[string]any {
	return cloneJSON(source).(map[string]any)
}

func cloneJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for name, item := range typed {
			result[name] = cloneJSON(item)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = cloneJSON(item)
		}
		return result
	default:
		return value
	}
}
