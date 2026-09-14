package mockserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/up2jj/wuko/step"
)

var (
	errBodyTooLarge    = errors.New("request body exceeds max_body_bytes")
	errInvalidJSON     = errors.New("request body is not valid JSON")
	routeParameterName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type requestValue struct {
	Method     string                      `json:"method" expr:"method"`
	URL        string                      `json:"url" expr:"url"`
	Scheme     string                      `json:"scheme" expr:"scheme"`
	Host       string                      `json:"host" expr:"host"`
	Path       string                      `json:"path" expr:"path"`
	Headers    map[string][]string         `json:"headers,omitempty" expr:"headers"`
	Body       string                      `json:"body,omitempty" expr:"body"`
	BodyBase64 string                      `json:"body_base64,omitempty" expr:"body_base64"`
	Params     map[string]string           `json:"params,omitempty" expr:"params"`
	Query      map[string]string           `json:"query,omitempty" expr:"query"`
	QueryAll   map[string][]string         `json:"query_all,omitempty" expr:"query_all"`
	JSON       any                         `json:"json,omitempty" expr:"json"`
	Form       map[string]string           `json:"form,omitempty" expr:"form"`
	FormAll    map[string][]string         `json:"form_all,omitempty" expr:"form_all"`
	Files      map[string][]map[string]any `json:"files,omitempty" expr:"files"`
	jsonErr    error
}

// readRequest materializes the request body for expressions and templates. encodeBase64 carries
// whether anything in this server actually reads a base64 body: the encoding is a third copy of
// every uploaded byte, so it is skipped unless an expectation or the traffic log asks for it.
func readRequest(request *http.Request, limit int64, encodeBase64 bool) (requestValue, error) {
	var source io.Reader = http.NoBody
	if request.Body != nil {
		source = request.Body
	}
	// One byte past the limit distinguishes "at the limit" from "over it", except at the very top
	// of the range, where limit+1 would wrap negative and make LimitReader report every body empty.
	readLimit := limit
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	body, err := io.ReadAll(io.LimitReader(source, readLimit))
	if err != nil {
		return requestValue{}, fmt.Errorf("reading request body: %w", err)
	}
	if int64(len(body)) > limit {
		return requestValue{}, errBodyTooLarge
	}
	requestURL := cloneURL(request.URL)
	if requestURL.Scheme == "" {
		requestURL.Scheme = "http"
		if request.TLS != nil {
			requestURL.Scheme = "https"
		}
	}
	if requestURL.Host == "" {
		requestURL.Host = request.Host
	}
	queryAll := cloneStringLists(requestURL.Query())
	value := requestValue{
		Method: request.Method, URL: requestURL.String(), Scheme: requestURL.Scheme, Host: requestURL.Host,
		Path: requestURL.Path, Headers: cloneStringLists(request.Header), Body: string(body),
		BodyBase64: encodedBody(body, encodeBase64), Params: map[string]string{},
		Query: firstValues(queryAll), QueryAll: queryAll, Form: map[string]string{},
		FormAll: map[string][]string{}, Files: map[string][]map[string]any{},
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &value.JSON); err != nil {
			value.jsonErr = fmt.Errorf("%w: %v", errInvalidJSON, err)
		}
	} else {
		value.jsonErr = errInvalidJSON
	}
	if err := parseMultipart(request.Header.Get("Content-Type"), body, &value, encodeBase64); err != nil {
		return requestValue{}, err
	}
	return value, nil
}

func encodedBody(body []byte, encode bool) string {
	if !encode || len(body) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(body)
}

func parseMultipart(contentType string, body []byte, request *requestValue, encodeBase64 bool) error {
	normalized := strings.ToLower(strings.TrimSpace(contentType))
	if !strings.HasPrefix(normalized, "multipart/form-data") {
		return nil
	}
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("malformed multipart request: invalid content type or boundary")
	}
	if !strings.EqualFold(mediaType, "multipart/form-data") {
		return nil
	}
	if parameters["boundary"] == "" {
		return fmt.Errorf("malformed multipart request: invalid content type or boundary")
	}
	reader := multipart.NewReader(bytes.NewReader(body), parameters["boundary"])
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("malformed multipart request: %w", err)
		}
		data, readErr := io.ReadAll(part)
		closeErr := part.Close()
		if readErr != nil {
			return fmt.Errorf("malformed multipart part: %w", readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("closing multipart part: %w", closeErr)
		}
		field := part.FormName()
		if field == "" {
			continue
		}
		if filename := part.FileName(); filename != "" {
			file := map[string]any{
				"filename": filename, "content_type": part.Header.Get("Content-Type"),
				"size": len(data), "body_base64": encodedBody(data, encodeBase64),
			}
			if utf8.Valid(data) {
				file["body"] = string(data)
			}
			request.Files[field] = append(request.Files[field], file)
			continue
		}
		request.FormAll[field] = append(request.FormAll[field], string(data))
	}
	request.Form = firstValues(request.FormAll)
	return nil
}

func (request requestValue) templateValue() map[string]any {
	value := map[string]any{
		"method": request.Method, "url": request.URL, "scheme": request.Scheme, "host": request.Host,
		"path": request.Path, "headers": request.Headers, "body": request.Body,
		"body_base64": request.BodyBase64, "params": request.Params,
		"query": request.Query, "query_all": request.QueryAll,
		"form": request.Form, "form_all": request.FormAll, "files": request.Files,
	}
	if request.jsonErr == nil {
		value["json"] = request.JSON
	}
	return value
}

func requestEnvironment(execution step.Request, request *requestValue, state map[string]any) map[string]any {
	environment := execution.ExpressionEnvironment(map[string]any{"request": request, "state": state})
	environment["header"] = func(name string) string { return firstHeader(request.Headers, name) }
	environment["query"] = func(name string) string { return request.Query[name] }
	environment["form"] = func(name string) string { return request.Form[name] }
	environment["jsonBody"] = func() (any, error) {
		if request.jsonErr != nil {
			return nil, request.jsonErr
		}
		return request.JSON, nil
	}
	environment["route"] = func(pattern string) (bool, error) {
		matched, params, err := matchRoute(pattern, request.Path)
		if matched {
			request.Params = params
		}
		return matched, err
	}
	return environment
}

func matchRoute(pattern, path string) (bool, map[string]string, error) {
	if !strings.HasPrefix(pattern, "/") {
		return false, nil, fmt.Errorf("route pattern must start with /")
	}
	patternSegments := strings.Split(pattern, "/")
	pathSegments := strings.Split(path, "/")
	if len(patternSegments) != len(pathSegments) {
		return false, nil, nil
	}
	params := make(map[string]string)
	for index, patternSegment := range patternSegments {
		if strings.HasPrefix(patternSegment, "{") && strings.HasSuffix(patternSegment, "}") {
			name := strings.TrimSuffix(strings.TrimPrefix(patternSegment, "{"), "}")
			if !routeParameterName.MatchString(name) {
				return false, nil, fmt.Errorf("invalid route parameter %q", name)
			}
			if _, exists := params[name]; exists {
				return false, nil, fmt.Errorf("duplicate route parameter %q", name)
			}
			if pathSegments[index] == "" {
				return false, nil, nil
			}
			params[name] = pathSegments[index]
			continue
		}
		if strings.ContainsAny(patternSegment, "{}") {
			return false, nil, fmt.Errorf("route parameters must occupy a complete path segment")
		}
		if patternSegment != pathSegments[index] {
			return false, nil, nil
		}
	}
	return true, params, nil
}

func firstHeader(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func firstValues(values map[string][]string) map[string]string {
	result := make(map[string]string, len(values))
	for name, items := range values {
		if len(items) > 0 {
			result[name] = items[0]
		}
	}
	return result
}

func cloneStringLists[T ~map[string][]string](values T) map[string][]string {
	result := make(map[string][]string, len(values))
	for name, items := range values {
		result[name] = append([]string(nil), items...)
	}
	return result
}

func cloneURL(source *url.URL) *url.URL {
	if source == nil {
		return &url.URL{}
	}
	copy := *source
	return &copy
}
