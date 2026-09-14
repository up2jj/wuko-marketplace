package forwardproxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/up2jj/wuko/step"
	glua "github.com/yuin/gopher-lua"
)

func (runner *Runner) loadLua(ctx context.Context, request step.Request) (*glua.FunctionProto, error) {
	if runner.config.Lua == nil {
		return nil, nil
	}
	source := runner.config.Lua.Source
	name := fmt.Sprintf("%s:%s:forward-proxy.lua", request.WorkflowName, request.StepID)
	if runner.config.Lua.File != "" {
		path, err := workflowFile(request.WorkflowDir, runner.config.Lua.File)
		if err != nil {
			return nil, fmt.Errorf("lua file: %w", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading Lua file %s: %w", path, err)
		}
		source, name = string(data), path
	}
	state := newTransformState()
	defer state.Close()
	state.SetContext(ctx)
	// A compiled chunk is immutable, so it is parsed once here and then run in a fresh state per
	// request. Compiling the source again on every proxied request costs more than running it.
	function, err := state.Load(bytes.NewReader([]byte(source)), name)
	if err != nil {
		return nil, fmt.Errorf("compiling Lua request transform: %w", err)
	}
	if err := loadTransform(state, function.Proto); err != nil {
		return nil, err
	}
	return function.Proto, nil
}

// workflowFile resolves a package-relative path and confines it to the workflow directory. A
// marketplace package is third-party content: without this, lua.file reads any file the process
// can, and gopher-lua echoes the offending line back through the step error.
func workflowFile(workflowDir, configured string) (string, error) {
	if filepath.IsAbs(configured) {
		return "", fmt.Errorf("must be relative to the workflow package")
	}
	path := filepath.Clean(filepath.Join(workflowDir, filepath.FromSlash(configured)))
	relative, err := filepath.Rel(workflowDir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("must not escape the workflow package")
	}
	root, err := filepath.EvalSymlinks(workflowDir)
	if err != nil {
		return "", err
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	relative, err = filepath.Rel(root, physical)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s escapes the workflow package", configured)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s must be a regular non-symlink file", configured)
	}
	return path, nil
}

func (proxy *proxyServer) applyLua(ctx context.Context, original, current requestValue, matched []string) (requestValue, error) {
	state := newTransformState()
	defer state.Close()
	state.SetContext(ctx)
	if err := loadTransform(state, proxy.luaProto); err != nil {
		return current, err
	}
	function := state.GetGlobal("handle")
	if function.Type() != glua.LTFunction {
		return current, fmt.Errorf("Lua transform must define handle(original, request, matched_rules)")
	}
	originalValue, err := toLua(state, requestMap(original))
	if err != nil {
		return current, err
	}
	requestValue, err := toLua(state, requestMap(current))
	if err != nil {
		return current, err
	}
	matchedValue, err := toLua(state, matched)
	if err != nil {
		return current, err
	}
	if err := state.CallByParam(glua.P{Fn: function, NRet: 1, Protect: true}, originalValue, requestValue, matchedValue); err != nil {
		return current, fmt.Errorf("running Lua request transform: %w", err)
	}
	returned := state.Get(-1)
	state.Pop(1)
	if returned == glua.LNil {
		return current, nil
	}
	converted, err := fromLua(returned, make(map[*glua.LTable]bool))
	if err != nil {
		return current, fmt.Errorf("reading Lua request transform result: %w", err)
	}
	fields, ok := converted.(map[string]any)
	if !ok {
		return current, fmt.Errorf("Lua handle must return nil or a request table")
	}
	return applyLuaFields(current, fields)
}

func newTransformState() *glua.LState {
	// A state is built per proxied request, so the library defaults -- a fixed 5120-slot registry
	// and a fixed 256-frame call stack, both allocated up front -- dominate the cost of running a
	// transform. The ceilings are unchanged: the registry still grows on demand to the same 5120
	// slots, and the call stack to the same 256 frames.
	state := glua.NewState(glua.Options{SkipOpenLibs: true, RegistrySize: 256,
		RegistryMaxSize: glua.RegistrySize, RegistryGrowStep: 512, MinimizeStackMemory: true})
	glua.OpenBase(state)
	glua.OpenTable(state)
	glua.OpenString(state)
	glua.OpenMath(state)
	for _, name := range []string{"dofile", "load", "loadfile", "loadstring", "print"} {
		state.SetGlobal(name, glua.LNil)
	}
	return state
}

func loadTransform(state *glua.LState, proto *glua.FunctionProto) error {
	state.Push(state.NewFunctionFromProto(proto))
	if err := state.PCall(0, 0, nil); err != nil {
		return fmt.Errorf("loading Lua request transform: %w", err)
	}
	if state.GetGlobal("handle").Type() != glua.LTFunction {
		return fmt.Errorf("Lua transform must define handle(original, request, matched_rules)")
	}
	return nil
}

func requestMap(value requestValue) map[string]any {
	return map[string]any{"method": value.Method, "url": value.URL, "scheme": value.Scheme, "host": value.Host,
		"path": value.Path, "query": cloneValues(value.Query), "headers": cloneHeader(value.Headers),
		"body": value.Body, "body_base64": value.BodyBase64}
}

func applyLuaFields(current requestValue, fields map[string]any) (requestValue, error) {
	result := current
	parsed, err := url.Parse(current.URL)
	if err != nil {
		return current, err
	}
	if value, changed, err := changedString(fields, "url", current.URL); err != nil {
		return current, err
	} else if changed {
		parsed, err = url.Parse(value)
		if err != nil {
			return current, fmt.Errorf("parsing Lua request url: %w", err)
		}
	}
	if value, changed, err := changedString(fields, "scheme", current.Scheme); err != nil {
		return current, err
	} else if changed {
		parsed.Scheme = value
	}
	if value, changed, err := changedString(fields, "host", current.Host); err != nil {
		return current, err
	} else if changed {
		parsed.Host = value
	}
	if value, changed, err := changedString(fields, "path", current.Path); err != nil {
		return current, err
	} else if changed {
		parsed.Path, parsed.RawPath = value, ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return current, fmt.Errorf("Lua request url must use http or https and include a host")
	}
	if value, exists := fields["method"]; exists {
		method, ok := value.(string)
		if !ok || strings.TrimSpace(method) == "" {
			return current, fmt.Errorf("Lua request method must be a non-empty string")
		}
		result.Method = method
	}
	if value, exists := fields["headers"]; exists {
		result.Headers, err = stringLists[http.Header](value, "headers")
		if err != nil {
			return current, err
		}
	}
	// The transform is handed the whole request table, so a handler that returns it unmodified
	// still carries a "query" field. Re-encoding it would rewrite the raw query of every proxied
	// request -- sorting keys, turning %20 into +, and dropping semicolon-separated pairs -- so
	// only a query the handler actually changed replaces what the client sent.
	queryChanged := false
	if value, exists := fields["query"]; exists {
		updated, err := stringLists[url.Values](value, "query")
		if err != nil {
			return current, err
		}
		if !maps.EqualFunc(updated, current.Query, slices.Equal) {
			result.Query, parsed.RawQuery, queryChanged = updated, updated.Encode(), true
		}
	}
	body, bodyChanged, err := changedString(fields, "body", current.Body)
	if err != nil {
		return current, err
	}
	body64, base64Changed, err := changedString(fields, "body_base64", current.BodyBase64)
	if err != nil {
		return current, err
	}
	switch {
	case bodyChanged && base64Changed:
		return current, fmt.Errorf("Lua request must change only one of body or body_base64")
	case bodyChanged:
		result.Body, result.BodyBase64 = body, base64.StdEncoding.EncodeToString([]byte(body))
	case base64Changed:
		decoded, err := base64.StdEncoding.DecodeString(body64)
		if err != nil {
			return current, fmt.Errorf("Lua request body_base64 is invalid: %w", err)
		}
		result.Body, result.BodyBase64 = string(decoded), body64
	}
	result.URL, result.Scheme, result.Host, result.Path = parsed.String(), parsed.Scheme, parsed.Host, parsed.Path
	if !queryChanged {
		result.Query = cloneValues(parsed.Query())
	}
	return result, nil
}

func changedString(fields map[string]any, name, old string) (string, bool, error) {
	value, exists := fields[name]
	if !exists {
		return old, false, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", false, fmt.Errorf("Lua request %s must be a string", name)
	}
	return text, text != old, nil
}

func stringLists[T ~map[string][]string](value any, name string) (T, error) {
	if list, ok := value.([]any); ok && len(list) == 0 {
		return make(T), nil
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Lua request %s must be a table", name)
	}
	result := make(T, len(fields))
	for key, item := range fields {
		list, ok := item.([]any)
		if !ok {
			return nil, fmt.Errorf("Lua request %s.%s must be a list of strings", name, key)
		}
		values := make([]string, len(list))
		for index, element := range list {
			text, ok := element.(string)
			if !ok {
				return nil, fmt.Errorf("Lua request %s.%s item %d must be a string", name, key, index)
			}
			values[index] = text
		}
		result[key] = values
	}
	return result, nil
}

func toLua(state *glua.LState, value any) (glua.LValue, error) {
	switch value := value.(type) {
	case nil:
		return glua.LNil, nil
	case string:
		return glua.LString(value), nil
	case bool:
		return glua.LBool(value), nil
	case []string:
		table := state.NewTable()
		for _, item := range value {
			table.Append(glua.LString(item))
		}
		return table, nil
	case []any:
		table := state.NewTable()
		for _, item := range value {
			converted, err := toLua(state, item)
			if err != nil {
				return nil, err
			}
			table.Append(converted)
		}
		return table, nil
	case map[string]any:
		table := state.NewTable()
		for key, item := range value {
			converted, err := toLua(state, item)
			if err != nil {
				return nil, err
			}
			table.RawSetString(key, converted)
		}
		return table, nil
	case http.Header:
		return toLua(state, mapStringLists(value))
	case url.Values:
		return toLua(state, mapStringLists(value))
	default:
		return nil, fmt.Errorf("unsupported Lua value %T", value)
	}
}

func mapStringLists[T ~map[string][]string](value T) map[string]any {
	result := make(map[string]any, len(value))
	for key, items := range value {
		result[key] = slices.Clone(items)
	}
	return result
}

func fromLua(value glua.LValue, seen map[*glua.LTable]bool) (any, error) {
	switch value := value.(type) {
	case *glua.LNilType:
		return nil, nil
	case glua.LString:
		return string(value), nil
	case glua.LBool:
		return bool(value), nil
	case glua.LNumber:
		return float64(value), nil
	case *glua.LTable:
		if seen[value] {
			return nil, fmt.Errorf("Lua table contains a cycle")
		}
		seen[value] = true
		defer delete(seen, value)
		array := make([]any, 0, value.Len())
		isArray := true
		value.ForEach(func(key, item glua.LValue) {
			if !isArray {
				return
			}
			index, ok := key.(glua.LNumber)
			if !ok || int(index) != len(array)+1 {
				isArray = false
				return
			}
			converted, err := fromLua(item, seen)
			if err != nil {
				isArray = false
				return
			}
			array = append(array, converted)
		})
		if isArray && len(array) == value.Len() {
			return array, nil
		}
		object := make(map[string]any)
		var conversionErr error
		value.ForEach(func(key, item glua.LValue) {
			if conversionErr != nil {
				return
			}
			name, ok := key.(glua.LString)
			if !ok {
				conversionErr = fmt.Errorf("Lua object keys must be strings")
				return
			}
			object[string(name)], conversionErr = fromLua(item, seen)
		})
		return object, conversionErr
	default:
		return nil, fmt.Errorf("unsupported Lua value %s", value.Type())
	}
}
