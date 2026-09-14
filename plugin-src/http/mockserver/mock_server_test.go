package mockserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/up2jj/wuko/step"
	"github.com/up2jj/wuko/workflow"
)

type testRenderer struct {
	renderer *workflow.Renderer
	data     map[string]any
}

func newTestRenderer(t *testing.T, data map[string]any) *testRenderer {
	t.Helper()
	renderer, err := workflow.NewRenderer(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &testRenderer{renderer: renderer, data: data}
}

func (renderer *testRenderer) Validate(value string) error {
	return renderer.renderer.Validate(value)
}

func (renderer *testRenderer) Render(value string) (string, error) {
	return renderer.renderer.Render(value, renderer.data)
}

func (renderer *testRenderer) ValidateContent(value string) error {
	return renderer.renderer.ValidateUncached(value)
}

func (renderer *testRenderer) RenderContent(value string) (string, error) {
	return renderer.renderer.RenderUncached(value, renderer.data)
}

func (renderer *testRenderer) RenderWith(value string, extra map[string]any) (string, error) {
	return renderer.renderer.Render(value, overlay(renderer.data, extra))
}

func (renderer *testRenderer) RenderContentWith(value string, extra map[string]any) (string, error) {
	return renderer.renderer.RenderUncached(value, overlay(renderer.data, extra))
}

func (renderer *testRenderer) Snapshot() step.DataTemplateRenderer {
	return &testRenderer{renderer: renderer.renderer, data: workflow.CloneMap(renderer.data)}
}

func (renderer *testRenderer) WithoutSecrets() step.DataTemplateRenderer {
	return &testRenderer{renderer: renderer.renderer.WithoutSecrets(), data: renderer.data}
}

func overlay(base, extra map[string]any) map[string]any {
	result := maps.Clone(base)
	maps.Copy(result, extra)
	return result
}

func TestAssertionsRollbackAndStatefulResponse(t *testing.T) {
	directory := t.TempDir()
	writeExpectation(t, directory, "users.yaml", `
version: 1
templates:
  headers:
    json_response:
      Content-Type: application/json
      X-User: '{{ .request.params.user_id }}'
  bodies:
    user:
      json: {expr: "state.users[request.params.user_id]"}
expectations:
  - name: store_user
    times: 1
    when: request.method == "PUT" && route("/users/{user_id}") && query("mode") == "save"
    assertions:
      - name: authorized
        expr: header("Authorization") == "Bearer " + vars.mock_token
        message: Expected the configured mock bearer token
      - name: valid_name
        expr: jsonBody().name != ""
      - name: supported_role
        expr: request.json.role in ["admin", "member"]
        message: 'Unsupported role {{ .request.json.role }}'
      - name: empty_initial_state
        expr: len(state.users) == 0 && request.params.user_id != ""
    update:
      - op: set
        path: '/users/{{ jsonPointerEscape .request.params.user_id }}'
        value: {expr: request.json}
    respond:
      status: 201
      headers_template: json_response
      body_template: user
`)
	renderer := newTestRenderer(t, map[string]any{"vars": map[string]any{"mock_token": "secret"}})
	runner := loadRunner(t, map[string]any{"users": map[string]any{}})
	server := loadServer(t, runner, directory, renderer)

	bad := httptest.NewRequest(http.MethodPut, "http://mock/users/alice?mode=save", strings.NewReader(`{"name":"Alice","role":"guest"}`))
	bad.Header.Set("Authorization", "Bearer wrong")
	badRequest, err := readRequest(bad, defaultMaxBodyBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	badResult := server.transact(badRequest)
	if badResult.response.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", badResult.response.status, badResult.response.body)
	}
	var failurePayload struct {
		Assertions []assertionFailure `json:"assertions"`
	}
	if err := json.Unmarshal(badResult.response.body, &failurePayload); err != nil {
		t.Fatal(err)
	}
	if len(failurePayload.Assertions) != 2 || failurePayload.Assertions[1].Message != "Unsupported role guest" {
		t.Fatalf("assertions = %#v", failurePayload.Assertions)
	}
	if server.expectations[0].used != 0 || len(server.state["users"].(map[string]any)) != 0 {
		t.Fatalf("failed assertion consumed transaction: used=%d state=%#v", server.expectations[0].used, server.state)
	}

	good := httptest.NewRequest(http.MethodPut, "http://mock/users/a~b?mode=save", strings.NewReader(`{"name":"Alice","role":"admin"}`))
	good.Header.Set("Authorization", "Bearer secret")
	goodRequest, err := readRequest(good, defaultMaxBodyBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	goodResult := server.transact(goodRequest)
	if goodResult.response.status != http.StatusCreated || goodResult.response.headers.Get("X-User") != "a~b" {
		t.Fatalf("response = %#v", goodResult.response)
	}
	if string(goodResult.response.body) != `{"name":"Alice","role":"admin"}` {
		t.Fatalf("body = %s", goodResult.response.body)
	}
	if server.expectations[0].used != 1 {
		t.Fatalf("used = %d", server.expectations[0].used)
	}
	if _, exists := server.state["users"].(map[string]any)["a~b"]; !exists {
		t.Fatalf("state = %#v", server.state)
	}
	if err := server.verify(); err == nil || !strings.Contains(err.Error(), "authorized") || !strings.Contains(err.Error(), "supported_role") {
		t.Fatalf("verification error = %v", err)
	}
}

func TestMultipartAssertionsAndNativeJSONValues(t *testing.T) {
	directory := t.TempDir()
	writeExpectation(t, directory, "upload.yaml", `
version: 1
expectations:
  - name: upload_avatar
    when: 'request.method == "POST" && route("/users/{user_id}/avatar")'
    assertions:
      - {name: category, expr: 'form("category") == "profile"'}
      - {name: one_avatar, expr: 'len(request.files.avatar) == 1'}
      - {name: png_file, expr: 'request.files.avatar[0].content_type == "image/png"'}
      - {name: file_size, expr: 'request.files.avatar[0].size <= vars.max_avatar_bytes'}
      - {name: file_body, expr: 'request.files.avatar[0].body == "PNG"'}
    respond:
      status: 201
      json:
        filename: {expr: 'request.files.avatar[0].filename'}
        size: {expr: 'request.files.avatar[0].size'}
`)
	renderer := newTestRenderer(t, map[string]any{"vars": map[string]any{"max_avatar_bytes": 10}})
	runner := loadRunner(t, nil)
	server := loadServer(t, runner, directory, renderer)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("category", "profile"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": []string{`form-data; name="avatar"; filename="avatar.png"`},
		"Content-Type":        []string{"image/png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "PNG"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://mock/users/7/avatar", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	parsed, err := readRequest(request, defaultMaxBodyBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	result := server.transact(parsed)
	if result.response.status != http.StatusCreated || string(result.response.body) != `{"filename":"avatar.png","size":3}` {
		t.Fatalf("response = %d %s", result.response.status, result.response.body)
	}
	file := parsed.Files["avatar"][0]
	if file["body_base64"] != "UE5H" || file["body"] != "PNG" {
		t.Fatalf("file = %#v", file)
	}
}

func TestMalformedMultipartDoesNotConsumeExpectation(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://mock/upload", strings.NewReader("broken"))
	request.Header.Set("Content-Type", "multipart/form-data")
	if _, err := readRequest(request, defaultMaxBodyBytes, true); err == nil || !strings.Contains(err.Error(), "malformed multipart") {
		t.Fatalf("error = %v", err)
	}
	server := &mockServer{runner: &Runner{config: Config{MaxBodyBytes: defaultMaxBodyBytes}}}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || server.failures.Total != 1 || server.failures.Details[0].Kind != "malformed" {
		t.Fatalf("response = %d, failures = %#v", recorder.Code, server.failures)
	}
}

func TestMixedSourcesAreOrderedAndDeduplicated(t *testing.T) {
	directory := t.TempDir()
	writeExpectation(t, directory, "common/a.yaml", expectationNamed("a"))
	writeExpectation(t, directory, "users/b.yaml", expectationNamed("b"))
	writeExpectation(t, directory, "users/nested/c.yml", expectationNamed("c"))
	request := step.Request{WorkflowDir: directory}
	files, err := resolveExpectationSources(t.Context(), request, []string{"users/**/*.y*ml", "common", "users/b.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(files))
	for index, file := range files {
		got[index], _ = filepath.Rel(directory, file)
	}
	want := []string{"users/b.yaml", "users/nested/c.yml", "common/a.yaml"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", got, want)
	}
}

func TestExpectationSourcesRejectBorrowedDirectoriesTraversalAndYAMLSymlinks(t *testing.T) {
	directory := t.TempDir()
	writeExpectation(t, directory, "valid.yaml", expectationNamed("valid"))
	if _, err := resolveExpectationSources(t.Context(), step.Request{WorkflowDir: directory, WorkflowDirBorrowed: true}, []string{"valid.yaml"}); err == nil || !strings.Contains(err.Error(), "belongs to the caller") {
		t.Fatalf("borrowed directory error = %v", err)
	}
	if _, err := resolveExpectationSources(t.Context(), step.Request{WorkflowDir: directory}, []string{"../valid.yaml"}); err == nil || !strings.Contains(err.Error(), "escape") {
		t.Fatalf("traversal error = %v", err)
	}
	if err := os.Symlink(filepath.Join(directory, "valid.yaml"), filepath.Join(directory, "linked.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := resolveExpectationSources(t.Context(), step.Request{WorkflowDir: directory}, []string{"*.yaml"}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestExpectationValidationDiagnostics(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"duplicate assertion", `version: 1
expectations:
- name: x
  when: "true"
  assertions: [{name: same, expr: "true"}, {name: same, expr: "true"}]
  respond: ok
`, "assertion name \"same\" is duplicated"},
		{"missing expression", `version: 1
expectations:
- name: x
  when: "true"
  assertions: [{name: missing}]
  respond: ok
`, "name and expr are required"},
		{"not boolean", `version: 1
expectations:
- name: x
  when: "true"
  assertions: [{name: bad, expr: "42"}]
  respond: ok
`, "compiling assertion \"bad\""},
		{"bad message template", `version: 1
expectations:
- name: x
  when: "true"
  assertions: [{name: bad, expr: "false", message: "{{"}]
  respond: ok
`, "message"},
		{"nonpositive times", `version: 1
expectations:
- name: x
  times: 0
  when: "true"
  respond: ok
`, "times must be positive"},
		{"duplicate expectation", `version: 1
expectations:
- {name: x, when: "true", respond: ok}
- {name: x, when: "false", respond: ok}
`, "expectation name \"x\" is duplicated"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			writeExpectation(t, directory, "case.yaml", test.body)
			renderer := newTestRenderer(t, map[string]any{})
			runner := loadRunner(t, nil)
			_, _, err := runner.load(t.Context(), step.Request{WorkflowDir: directory}, renderer, loadRun)
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "case.yaml") {
				t.Fatalf("error = %v, want %q with filename", err, test.want)
			}
		})
	}
}

func TestLiteralStringBase64AndFileResponses(t *testing.T) {
	directory := t.TempDir()
	writeExpectation(t, directory, "body.txt", "Hello {{ .request.path }}")
	writeExpectation(t, directory, "responses.yaml", `
version: 1
expectations:
  - {name: scalar, when: 'request.path == "/scalar"', respond: 'Hello {{ .request.path }}'}
  - name: literal
    when: request.path == "/literal"
    respond: {literal_body: '{{ untouched }}'}
  - name: base64
    when: request.path == "/base64"
    respond: {body_base64: U29tZSBiaW5hcnk=}
  - name: file
    when: request.path == "/file"
    respond: {body_file: body.txt}
`)
	renderer := newTestRenderer(t, map[string]any{})
	runner := loadRunner(t, nil)
	server := loadServer(t, runner, directory, renderer)
	wants := map[string]string{"/scalar": "Hello /scalar", "/literal": "{{ untouched }}", "/base64": "Some binary", "/file": "Hello /file"}
	for path, want := range wants {
		request := httptest.NewRequest(http.MethodGet, "http://mock"+path, nil)
		parsed, err := readRequest(request, defaultMaxBodyBytes, true)
		if err != nil {
			t.Fatal(err)
		}
		result := server.transact(parsed)
		if string(result.response.body) != want {
			t.Errorf("%s body = %q, want %q", path, result.response.body, want)
		}
	}
}

func TestAssertionEvaluationErrorIsReportedAndDoesNotRenderResponse(t *testing.T) {
	directory := t.TempDir()
	writeExpectation(t, directory, "error.yaml", `version: 1
expectations:
- name: broken
  times: 1
  when: "true"
  assertions:
    - name: evaluates
      expr: 'jsonBody().missing.value == "ok"'
  respond:
    status: 299
    body: normal response must not be used
`)
	renderer := newTestRenderer(t, map[string]any{})
	runner := loadRunner(t, nil)
	server := loadServer(t, runner, directory, renderer)
	raw := httptest.NewRequest(http.MethodPost, "http://mock/error", strings.NewReader(`{}`))
	request, err := readRequest(raw, defaultMaxBodyBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	result := server.transact(request)
	if result.response.status != http.StatusUnprocessableEntity || strings.Contains(string(result.response.body), "normal response") {
		t.Fatalf("response = %d %s", result.response.status, result.response.body)
	}
	if server.expectations[0].used != 0 || !server.failures.Details[0].Assertions[0].Evaluation {
		t.Fatalf("server = %#v", server)
	}
}

func TestConcurrentTransactionsSerializeStateUpdates(t *testing.T) {
	directory := t.TempDir()
	writeExpectation(t, directory, "append.yaml", `version: 1
expectations:
- name: append
  when: 'request.method == "POST"'
  update:
    - op: append
      path: /items
      value: {expr: 'query("item")'}
  respond:
    json:
      count: {expr: 'len(state.items)'}
`)
	renderer := newTestRenderer(t, map[string]any{})
	runner := loadRunner(t, map[string]any{"items": []any{}})
	server := loadServer(t, runner, directory, renderer)
	const count = 50
	var wait sync.WaitGroup
	wait.Add(count)
	for index := range count {
		go func() {
			defer wait.Done()
			raw := httptest.NewRequest(http.MethodPost, fmt.Sprintf("http://mock/items?item=%d", index), nil)
			request, err := readRequest(raw, defaultMaxBodyBytes, true)
			if err != nil {
				t.Errorf("read request: %v", err)
				return
			}
			if result := server.transact(request); result.response.status != http.StatusOK {
				t.Errorf("status = %d: %v", result.response.status, result.err)
			}
		}()
	}
	wait.Wait()
	if got := len(server.state["items"].([]any)); got != count {
		t.Fatalf("item count = %d, want %d", got, count)
	}
}

func TestTrafficLogRedactsSecretsAndOmitsDecodedContent(t *testing.T) {
	var output bytes.Buffer
	logger, _, err := openTrafficLogger(&LogConfig{Destination: "stdout"}, &output, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := requestValue{Method: http.MethodPost, URL: "http://mock/upload?token=top-secret", Path: "/upload",
		Headers:  map[string][]string{"Authorization": {"Bearer secret"}},
		QueryAll: map[string][]string{"token": {"top-secret"}}, Body: "raw-secret", BodyBase64: "cmF3LXNlY3JldA==",
		JSON: map[string]any{"decoded": "must-not-appear"}, Form: map[string]string{"decoded": "must-not-appear"},
		Files: map[string][]map[string]any{"file": {{"body": "must-not-appear"}}}}
	if err := logger.Write(trafficRecord{Timestamp: time.Now(), RequestID: "id", Request: logger.request(request), Status: 200}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, forbidden := range []string{"Bearer secret", "top-secret", "raw-secret", "cmF3LXNlY3JldA==", "must-not-appear"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("log contains %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("log was not redacted: %s", text)
	}
}

func TestTrafficLogFileIsPrivate(t *testing.T) {
	directory := t.TempDir()
	logger, path, err := openTrafficLogger(&LogConfig{Destination: "file", Path: "traffic.jsonl"}, nil, directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("permissions = %o", permissions)
	}
}

func TestVerificationRetainsOnlyFirstTwentyRequestDetails(t *testing.T) {
	server := &mockServer{}
	for index := range 25 {
		server.recordFailure(requestFailure{Kind: "unmatched", Method: http.MethodGet,
			URL: fmt.Sprintf("http://mock/%d", index)})
	}
	err := server.verify()
	if err == nil || !strings.Contains(err.Error(), "25 request failure(s)") ||
		!strings.Contains(err.Error(), "5 additional failed request(s) omitted") || strings.Contains(err.Error(), "http://mock/20") {
		t.Fatalf("verification error = %v", err)
	}
}

func TestUnmatchedAndResponseFailureRollback(t *testing.T) {
	directory := t.TempDir()
	writeExpectation(t, directory, "failure.yaml", `version: 1
expectations:
- name: broken_response
  times: 1
  when: 'request.path == "/broken"'
  update:
    - {op: set, path: /changed, value: true}
  respond: {body_base64: definitely-not-base64}
`)
	renderer := newTestRenderer(t, map[string]any{})
	runner := loadRunner(t, map[string]any{"changed": false})
	server := loadServer(t, runner, directory, renderer)

	unmatched := httptest.NewRequest(http.MethodGet, "http://mock/missing", nil)
	parsed, err := readRequest(unmatched, defaultMaxBodyBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := server.transact(parsed); result.response.status != http.StatusNotFound {
		t.Fatalf("unmatched status = %d", result.response.status)
	}
	broken := httptest.NewRequest(http.MethodGet, "http://mock/broken", nil)
	parsed, err = readRequest(broken, defaultMaxBodyBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	result := server.transact(parsed)
	if result.response.status != http.StatusInternalServerError || server.state["changed"] != false || server.expectations[0].used != 0 {
		t.Fatalf("result = %#v, state = %#v, used = %d", result, server.state, server.expectations[0].used)
	}
}

func TestInitialStateFileIsConfinedAndRendered(t *testing.T) {
	directory := t.TempDir()
	writeExpectation(t, directory, "expectation.yaml", expectationNamed("never"))
	writeExpectation(t, directory, "state.json", `{"name":"{{ .vars.name }}","items":[]}`)
	renderer := newTestRenderer(t, map[string]any{"vars": map[string]any{"name": "Alice"}})
	runnerValue, err := New(map[string]any{"expectations": []any{"expectation.yaml"}, "initial_state_file": "state.json"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := runnerValue.(*Runner).loadInitialState(step.Request{WorkflowDir: directory}, renderer, loadRun)
	if err != nil {
		t.Fatal(err)
	}
	if state["name"] != "Alice" {
		t.Fatalf("state = %#v", state)
	}

	runnerValue, err = New(map[string]any{"expectations": []any{"expectation.yaml"}, "initial_state_file": "../outside.json"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runnerValue.(*Runner).loadInitialState(step.Request{WorkflowDir: directory}, renderer, loadRun); err == nil || !strings.Contains(err.Error(), "escape") {
		t.Fatalf("error = %v", err)
	}
}

type testServices struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan error
}

func newTestServices(t *testing.T) *testServices {
	ctx, cancel := context.WithCancel(t.Context())
	services := &testServices{ctx: ctx, cancel: cancel, done: make(chan error, 1)}
	t.Cleanup(func() {
		cancel()
		<-services.done
	})
	return services
}

func (services *testServices) StartService(_ string, _ string, _ step.ServiceOptions, run func(context.Context) error) error {
	go func() { services.done <- run(services.ctx) }()
	return nil
}

func TestHTTPAndHTTPSLifecycle(t *testing.T) {
	for _, tlsEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[tlsEnabled], func(t *testing.T) {
			directory := t.TempDir()
			writeExpectation(t, directory, "one.yaml", `version: 1
expectations:
- name: ping
  times: 1
  when: 'request.method == "GET" && request.path == "/ping"'
  respond: pong
`)
			renderer := newTestRenderer(t, map[string]any{})
			runner := loadRunner(t, nil)
			runner.config.TLS = tlsEnabled
			services := newTestServices(t)
			result, err := runner.Run(t.Context(), step.Request{StepID: "mock", WorkflowDir: directory,
				RunDir: directory, TemplateRenderer: renderer, Services: services})
			if err != nil {
				t.Fatal(err)
			}
			client := http.DefaultClient
			if tlsEnabled {
				certificate, err := os.ReadFile(result.Outputs["ca_cert"].(string))
				if err != nil {
					t.Fatal(err)
				}
				pool := x509.NewCertPool()
				if !pool.AppendCertsFromPEM(certificate) {
					t.Fatal("generated CA did not parse")
				}
				client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
			}
			response, err := client.Get(result.Outputs["url"].(string) + "/ping")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK || string(body) != "pong" {
				t.Fatalf("response = %d %q, err %v", response.StatusCode, body, err)
			}
			services.cancel()
			serviceErr := <-services.done
			services.done <- serviceErr
			if serviceErr != nil && !errorsOnlyCanceled(serviceErr) {
				t.Fatalf("service error = %v", serviceErr)
			}
			if err := runner.Cleanup(t.Context(), result); err != nil {
				t.Fatal(err)
			}
			if tlsEnabled {
				if _, err := os.Stat(result.Outputs["ca_cert"].(string)); !os.IsNotExist(err) {
					t.Fatalf("CA still exists: %v", err)
				}
			}
		})
	}
}

func errorsOnlyCanceled(err error) bool {
	if err == nil {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !errorsOnlyCanceled(child) {
				return false
			}
		}
		return true
	}
	return strings.Contains(err.Error(), "context canceled")
}

func loadRunner(t *testing.T, initialState any) *Runner {
	t.Helper()
	raw := map[string]any{"expectations": []any{"."}}
	if initialState != nil {
		raw["initial_state"] = initialState
	}
	runnerValue, err := New(raw)
	if err != nil {
		t.Fatal(err)
	}
	return runnerValue.(*Runner)
}

func loadServer(t *testing.T, runner *Runner, directory string, renderer *testRenderer) *mockServer {
	t.Helper()
	execution := step.Request{WorkflowDir: directory, Vars: valueMap(renderer.data, "vars"), TemplateRenderer: renderer}
	expectations, state, err := runner.load(t.Context(), execution, renderer, loadRun)
	if err != nil {
		t.Fatal(err)
	}
	return &mockServer{runner: runner, execution: execution, renderer: renderer, expectations: expectations, state: state}
}

func valueMap(data map[string]any, name string) map[string]any {
	value, _ := data[name].(map[string]any)
	return value
}

func writeExpectation(t *testing.T, directory, name, content string) {
	t.Helper()
	path := filepath.Join(directory, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimPrefix(content, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}

func expectationNamed(name string) string {
	return "version: 1\nexpectations:\n- name: " + name + "\n  when: 'false'\n  respond: ok\n"
}

var _ step.DataTemplateRenderer = (*testRenderer)(nil)
var _ step.ServiceLauncher = (*testServices)(nil)

func TestInlineInitialStateIsNotRenderedTwice(t *testing.T) {
	directory := t.TempDir()
	writeExpectation(t, directory, "expectation.yaml", expectationNamed("never"))
	renderer := newTestRenderer(t, map[string]any{"vars": map[string]any{"name": "Alice"}})
	// The engine renders a step's configuration before it builds the runner, so an inline
	// initial_state already holds its final text. A second pass here would expand delimiters that
	// arrived from workflow data.
	runnerValue, err := New(map[string]any{
		"expectations":  []any{"expectation.yaml"},
		"initial_state": map[string]any{"greeting": "{{ .vars.name }}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := runnerValue.(*Runner).loadInitialState(step.Request{WorkflowDir: directory}, renderer, loadRun)
	if err != nil {
		t.Fatal(err)
	}
	if state["greeting"] != "{{ .vars.name }}" {
		t.Fatalf("greeting = %#v, want the value left as the engine rendered it", state["greeting"])
	}
}

func TestValidateDefersConfigurationThatStillHoldsTemplates(t *testing.T) {
	directory := t.TempDir()
	writeExpectation(t, directory, "expectation.yaml", expectationNamed("never"))
	// Validation runs before any step produces output, so a templated path names no file yet and
	// an unresolvable reference is not a workflow error.
	renderer := newTestRenderer(t, map[string]any{})
	request := step.Request{WorkflowDir: directory, TemplateRenderer: renderer}
	for _, raw := range []map[string]any{
		{"expectations": []any{"{{ .steps.setup.dir }}/expectation.yaml"}},
		{"expectations": []any{"expectation.yaml"}, "initial_state": map[string]any{"base": "{{ .steps.setup.url }}"}},
		{"expectations": []any{"expectation.yaml"}, "initial_state_file": "{{ .steps.setup.dir }}/state.json"},
	} {
		runnerValue, err := New(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := runnerValue.(*Runner).Validate(t.Context(), request); err != nil {
			t.Fatalf("Validate(%v) = %v, want a deferred pass", raw, err)
		}
	}
}

func TestBodyBase64IsEncodedOnlyWhenRead(t *testing.T) {
	body := strings.Repeat("a", 64)
	for _, test := range []struct {
		name   string
		encode bool
		want   string
	}{
		{name: "read", encode: true, want: "YWFh"},
		{name: "unread", encode: false, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := httptest.NewRequest(http.MethodPost, "http://mock/upload", strings.NewReader(body))
			request, err := readRequest(raw, defaultMaxBodyBytes, test.encode)
			if err != nil {
				t.Fatal(err)
			}
			if request.Body != body {
				t.Fatalf("body = %q", request.Body)
			}
			if !strings.HasPrefix(request.BodyBase64, test.want) || (test.want == "" && request.BodyBase64 != "") {
				t.Fatalf("body_base64 = %q, want prefix %q", request.BodyBase64, test.want)
			}
		})
	}
}

func TestNeedsBodyBase64(t *testing.T) {
	reads := []*compiledExpectation{{readsBodyBase64: true}}
	plain := []*compiledExpectation{{}}
	if !needsBodyBase64(nil, reads) {
		t.Fatal("an expectation that mentions body_base64 must keep the encoding")
	}
	if needsBodyBase64(nil, plain) {
		t.Fatal("no consumer reads a base64 body")
	}
	if !needsBodyBase64(&LogConfig{IncludeBodies: true}, plain) {
		t.Fatal("a traffic log recording bodies reads the base64 body")
	}
	if needsBodyBase64(&LogConfig{}, plain) {
		t.Fatal("a log that omits bodies reads nothing")
	}
}

func TestLiteralEscapesDynamicExpr(t *testing.T) {
	shape := expressionShape()
	value, err := compileDynamic(map[string]any{"literal": map[string]any{"expr": "a+b"}}, shape, nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := value.resolve(map[string]any{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := resolved.(map[string]any)
	if !ok || object["expr"] != "a+b" {
		t.Fatalf("resolved = %#v, want the payload verbatim", resolved)
	}
	if _, err := compileDynamic(map[string]any{"expr": 5}, shape, nil); err == nil ||
		!strings.Contains(err.Error(), "literal") {
		t.Fatalf("error = %v, want the message to point at the escape hatch", err)
	}
}

func TestClientDisconnectIsNotAVerificationFailure(t *testing.T) {
	if !clientDisconnected(fmt.Errorf("writing response: %w", syscall.EPIPE)) {
		t.Fatal("a broken pipe is the client hanging up")
	}
	if !clientDisconnected(fmt.Errorf("writing response: %w", syscall.ECONNRESET)) {
		t.Fatal("a connection reset is the client hanging up")
	}
	if !clientDisconnected(context.Canceled) {
		t.Fatal("a canceled run is not a mock fault")
	}
	if clientDisconnected(errors.New("short write")) {
		t.Fatal("an ordinary write failure is still the mock's fault")
	}
}

func TestBodyFileMentioningBodyBase64KeepsTheEncoding(t *testing.T) {
	directory := t.TempDir()
	writeExpectation(t, directory, "echo.yaml", "version: 1\nexpectations:\n- name: echo\n"+
		"  when: 'true'\n  respond: {body_file: echo.txt}\n")
	// The YAML above never says body_base64; only the file it pulls in does. Without scanning that
	// text the server drops the encoding and this response renders empty.
	writeExpectation(t, directory, "echo.txt", "{{ .request.body_base64 }}")
	request := step.Request{WorkflowDir: directory}
	sources, err := resolveExpectationSources(t.Context(), request, []string{"echo.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := loadExpectationFiles(t.Context(), request, sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled) != 1 {
		t.Fatalf("loaded %d expectations, want 1", len(compiled))
	}
	if !compiled[0].readsBodyBase64 {
		t.Fatal("a body_file that reads body_base64 must keep the encoding")
	}
}
