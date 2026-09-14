package mockserver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	wukoexpr "github.com/up2jj/wuko/expression"
	"github.com/up2jj/wuko/step"
)

type dynamicKind uint8

const (
	dynamicLiteral dynamicKind = iota
	dynamicTemplate
	dynamicExpression
	dynamicList
	dynamicObject
)

type dynamicValue struct {
	kind    dynamicKind
	literal any
	text    string
	program *vm.Program
	list    []*dynamicValue
	object  map[string]*dynamicValue
}

type compiledResponse struct {
	status  int
	headers map[string][]string
	body    compiledBody
}

type compiledBody struct {
	kind    string
	text    string
	dynamic *dynamicValue
}

type renderedResponse struct {
	status  int
	headers http.Header
	body    []byte
}

func compileDynamic(value any, shape map[string]any, renderer step.TemplateRenderer) (*dynamicValue, error) {
	switch typed := value.(type) {
	case string:
		if renderer != nil {
			if err := renderer.ValidateContent(typed); err != nil {
				return nil, err
			}
		}
		return &dynamicValue{kind: dynamicTemplate, text: typed}, nil
	case []any:
		items := make([]*dynamicValue, len(typed))
		for index, item := range typed {
			compiled, err := compileDynamic(item, shape, renderer)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", index, err)
			}
			items[index] = compiled
		}
		return &dynamicValue{kind: dynamicList, list: items}, nil
	case map[string]any:
		if len(typed) == 1 {
			if source, exists := typed["literal"]; exists {
				// The escape hatch for payloads that are data rather than instructions: a mock whose
				// response genuinely contains an "expr" key, or text whose template delimiters must
				// reach the client verbatim. Nothing below it is compiled or rendered.
				return &dynamicValue{kind: dynamicLiteral, literal: source}, nil
			}
			if source, exists := typed["expr"]; exists {
				text, ok := source.(string)
				if !ok || strings.TrimSpace(text) == "" {
					return nil, fmt.Errorf("expr must be a non-empty string; use {literal: ...} for a payload that carries its own expr key")
				}
				program, err := wukoexpr.Compile(text, expr.Env(shape), expr.AllowUndefinedVariables())
				if err != nil {
					return nil, fmt.Errorf("compiling expr: %w", err)
				}
				return &dynamicValue{kind: dynamicExpression, text: text, program: program}, nil
			}
		}
		fields := make(map[string]*dynamicValue, len(typed))
		for name, item := range typed {
			compiled, err := compileDynamic(item, shape, renderer)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", name, err)
			}
			fields[name] = compiled
		}
		return &dynamicValue{kind: dynamicObject, object: fields}, nil
	default:
		return &dynamicValue{kind: dynamicLiteral, literal: value}, nil
	}
}

func (value *dynamicValue) resolve(environment map[string]any, renderer step.DataTemplateRenderer, extra map[string]any) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch value.kind {
	case dynamicLiteral:
		return value.literal, nil
	case dynamicTemplate:
		return renderer.RenderContentWith(value.text, extra)
	case dynamicExpression:
		resolved, err := expr.Run(value.program, environment)
		if err != nil {
			return nil, fmt.Errorf("evaluating expr %q: %w", value.text, err)
		}
		return resolved, nil
	case dynamicList:
		result := make([]any, len(value.list))
		for index, item := range value.list {
			resolved, err := item.resolve(environment, renderer, extra)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", index, err)
			}
			result[index] = resolved
		}
		return result, nil
	case dynamicObject:
		result := make(map[string]any, len(value.object))
		for name, item := range value.object {
			resolved, err := item.resolve(environment, renderer, extra)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", name, err)
			}
			result[name] = resolved
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported dynamic value")
	}
}

func compileResponse(request step.Request, source string, configured responseSpec, templates responseTemplates, shape map[string]any, renderer step.TemplateRenderer) (compiledResponse, error) {
	if !configured.defined {
		return compiledResponse{}, fmt.Errorf("respond is required")
	}
	response := configured
	if response.HeadersTemplate != "" {
		template, exists := templates.Headers[response.HeadersTemplate]
		if !exists {
			return compiledResponse{}, fmt.Errorf("headers_template %q is not defined in this file", response.HeadersTemplate)
		}
		response.Headers = mergeHeaders(template, response.Headers)
	}
	if response.BodyTemplate != "" {
		template, exists := templates.Bodies[response.BodyTemplate]
		if !exists {
			return compiledResponse{}, fmt.Errorf("body_template %q is not defined in this file", response.BodyTemplate)
		}
		response.bodySpec = mergeBody(template, response.bodySpec)
	}
	status := response.Status
	if status == 0 {
		status = http.StatusOK
	}
	if status < 200 || status > 599 {
		return compiledResponse{}, fmt.Errorf("response status must be between 200 and 599")
	}
	headers := make(map[string][]string, len(response.Headers))
	for name, values := range response.Headers {
		if !validHeaderName(name) {
			return compiledResponse{}, fmt.Errorf("response header name %q is invalid", name)
		}
		headers[name] = append([]string(nil), values...)
		for _, value := range values {
			if renderer != nil {
				if err := renderer.ValidateContent(value); err != nil {
					return compiledResponse{}, fmt.Errorf("header %s: %w", name, err)
				}
			}
		}
	}
	body, err := compileBody(request, source, response.bodySpec, shape, renderer)
	if err != nil {
		return compiledResponse{}, err
	}
	return compiledResponse{status: status, headers: headers, body: body}, nil
}

func compileBody(request step.Request, source string, body bodySpec, shape map[string]any, renderer step.TemplateRenderer) (compiledBody, error) {
	count := 0
	for _, present := range []bool{body.Body != nil, body.LiteralBody != nil, body.hasJSON, body.BodyFile != "", body.BodyBase64 != nil} {
		if present {
			count++
		}
	}
	if count > 1 {
		return compiledBody{}, fmt.Errorf("response body, literal_body, json, body_file, and body_base64 are mutually exclusive")
	}
	switch {
	case body.Body != nil:
		if renderer != nil {
			if err := renderer.ValidateContent(*body.Body); err != nil {
				return compiledBody{}, fmt.Errorf("body template: %w", err)
			}
		}
		return compiledBody{kind: "body", text: *body.Body}, nil
	case body.LiteralBody != nil:
		return compiledBody{kind: "literal", text: *body.LiteralBody}, nil
	case body.hasJSON:
		value, err := compileDynamic(body.JSON, shape, renderer)
		if err != nil {
			return compiledBody{}, fmt.Errorf("json response: %w", err)
		}
		return compiledBody{kind: "json", dynamic: value}, nil
	case body.BodyFile != "":
		path := body.BodyFile
		if filepath.IsAbs(path) {
			return compiledBody{}, fmt.Errorf("body_file must be relative to the expectation file")
		}
		path = filepath.Clean(filepath.Join(filepath.Dir(source), filepath.FromSlash(path)))
		relative, relativeErr := filepath.Rel(request.WorkflowDir, path)
		if relativeErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return compiledBody{}, fmt.Errorf("body_file must not escape the workflow package")
		}
		root, err := filepath.EvalSymlinks(request.WorkflowDir)
		if err != nil {
			return compiledBody{}, err
		}
		if err := ensureWithin(root, path); err != nil {
			return compiledBody{}, fmt.Errorf("body_file: %w", err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return compiledBody{}, fmt.Errorf("body_file %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return compiledBody{}, fmt.Errorf("body_file %s must be a regular non-symlink file", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return compiledBody{}, fmt.Errorf("reading body_file %s: %w", path, err)
		}
		if !utf8.Valid(data) {
			return compiledBody{}, fmt.Errorf("body_file %s must contain UTF-8 text; use body_base64 for binary data", path)
		}
		text := string(data)
		if renderer != nil {
			if err := renderer.ValidateContent(text); err != nil {
				return compiledBody{}, fmt.Errorf("body_file %s template: %w", path, err)
			}
		}
		return compiledBody{kind: "body", text: text}, nil
	case body.BodyBase64 != nil:
		if renderer != nil {
			if err := renderer.ValidateContent(*body.BodyBase64); err != nil {
				return compiledBody{}, fmt.Errorf("body_base64 template: %w", err)
			}
		}
		return compiledBody{kind: "base64", text: *body.BodyBase64}, nil
	default:
		return compiledBody{}, nil
	}
}

func (response compiledResponse) render(environment map[string]any, renderer step.DataTemplateRenderer, request requestValue, state map[string]any) (renderedResponse, error) {
	extra := map[string]any{"request": request.templateValue(), "state": state}
	headers := make(http.Header, len(response.headers))
	for name, values := range response.headers {
		for _, value := range values {
			rendered, err := renderer.RenderContentWith(value, extra)
			if err != nil {
				return renderedResponse{}, fmt.Errorf("rendering response header %s: %w", name, err)
			}
			if !validHeaderValue(rendered) {
				return renderedResponse{}, fmt.Errorf("rendered response header %s contains invalid control characters", name)
			}
			headers.Add(name, rendered)
		}
	}
	var data []byte
	switch response.body.kind {
	case "":
	case "literal":
		data = []byte(response.body.text)
	case "body":
		value, err := renderer.RenderContentWith(response.body.text, extra)
		if err != nil {
			return renderedResponse{}, fmt.Errorf("rendering response body: %w", err)
		}
		data = []byte(value)
	case "base64":
		value, err := renderer.RenderContentWith(response.body.text, extra)
		if err != nil {
			return renderedResponse{}, fmt.Errorf("rendering response body_base64: %w", err)
		}
		data, err = base64.StdEncoding.DecodeString(value)
		if err != nil {
			return renderedResponse{}, fmt.Errorf("decoding response body_base64: %w", err)
		}
	case "json":
		value, err := response.body.dynamic.resolve(environment, renderer, extra)
		if err != nil {
			return renderedResponse{}, fmt.Errorf("resolving JSON response: %w", err)
		}
		data, err = json.Marshal(value)
		if err != nil {
			return renderedResponse{}, fmt.Errorf("encoding JSON response: %w", err)
		}
		if headers.Get("Content-Type") == "" {
			headers.Set("Content-Type", "application/json")
		}
	default:
		return renderedResponse{}, fmt.Errorf("unsupported response body kind %q", response.body.kind)
	}
	return renderedResponse{status: response.status, headers: headers, body: data}, nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for index := range len(name) {
		character := name[index]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			continue
		}
		return false
	}
	return true
}

func validHeaderValue(value string) bool {
	for index := range len(value) {
		character := value[index]
		if character == '\t' || character >= 0x20 && character != 0x7f {
			continue
		}
		return false
	}
	return true
}

func mergeHeaders(base, overlay map[string]stringList) map[string]stringList {
	result := make(map[string]stringList, len(base)+len(overlay))
	for name, values := range base {
		result[name] = append(stringList(nil), values...)
	}
	for name, values := range overlay {
		for existing := range result {
			if strings.EqualFold(existing, name) {
				delete(result, existing)
			}
		}
		result[name] = append(stringList(nil), values...)
	}
	return result
}

func mergeBody(base, overlay bodySpec) bodySpec {
	if bodyCount(overlay) == 0 {
		return base
	}
	if base.hasJSON && overlay.hasJSON {
		overlay.JSON = mergeJSON(base.JSON, overlay.JSON)
	}
	return overlay
}

func bodyCount(body bodySpec) int {
	count := 0
	for _, present := range []bool{body.Body != nil, body.LiteralBody != nil, body.hasJSON, body.BodyFile != "", body.BodyBase64 != nil} {
		if present {
			count++
		}
	}
	return count
}

func mergeJSON(base, overlay any) any {
	baseMap, baseOK := base.(map[string]any)
	overlayMap, overlayOK := overlay.(map[string]any)
	if !baseOK || !overlayOK {
		return overlay
	}
	result := make(map[string]any, len(baseMap)+len(overlayMap))
	for name, value := range baseMap {
		result[name] = value
	}
	for name, value := range overlayMap {
		if previous, exists := result[name]; exists {
			result[name] = mergeJSON(previous, value)
		} else {
			result[name] = value
		}
	}
	return result
}
