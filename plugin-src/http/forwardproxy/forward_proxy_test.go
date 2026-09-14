package forwardproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/up2jj/wuko/step"
	httpstep "github.com/up2jj/wuko/steps/http"
)

type testServices struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	err    error
}

func newTestServices(t *testing.T) *testServices {
	ctx, cancel := context.WithCancel(t.Context())
	services := &testServices{ctx: ctx, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		services.wg.Wait()
		if services.err != nil && !strings.Contains(services.err.Error(), "context canceled") {
			t.Errorf("service error: %v", services.err)
		}
	})
	return services
}

func (services *testServices) StartService(_ string, _ string, _ step.ServiceOptions, run func(context.Context) error) error {
	services.wg.Go(func() { services.err = run(services.ctx) })
	return nil
}

func TestForwardProxyRewritesHTTPRequestAndLogsToStdout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if request.Method != http.MethodPut || request.URL.Path != "/changed" || request.URL.Query().Get("source") != "wuko" {
			t.Errorf("request = %s %s", request.Method, request.URL.String())
		}
		if request.Header.Get("X-Proxied") != "true" || string(body) != "changed" {
			t.Errorf("headers/body = %v / %q", request.Header, body)
		}
		writer.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()

	runnerValue, err := New(map[string]any{
		"rules": []any{map[string]any{
			"name": "change", "when": `original.method == "POST"`,
			"rewrite": map[string]any{"method": "PUT", "url": upstream.URL + "/changed?source=wuko",
				"headers": map[string]any{"set": map[string]any{"X-Proxied": []any{"true"}}}, "body": "changed"},
		}},
		"log": map[string]any{"destination": "stdout"},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := runnerValue.(*Runner)
	services := newTestServices(t)
	var stdout bytes.Buffer
	result, err := runner.Run(t.Context(), step.Request{StepID: "proxy", RunDir: t.TempDir(), Services: services, Stdout: &stdout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Cleanup(t.Context(), result) })

	response, err := proxyClient(t, result, nil).Post(upstream.URL+"/original", "text/plain", strings.NewReader("original"))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", response.StatusCode)
	}
	response.Body.Close()
	var record trafficRecord
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &record); err != nil {
		t.Fatalf("log = %q: %v", stdout.String(), err)
	}
	if record.Request.Method != http.MethodPut || record.Original.Method != http.MethodPost || record.Request.Body != "" {
		t.Errorf("record = %#v", record)
	}
	if len(record.MatchedRules) != 1 || record.MatchedRules[0] != "change" {
		t.Errorf("matched rules = %v", record.MatchedRules)
	}
}

func TestForwardProxyInterceptsHTTPSWithGeneratedCA(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(request.Header.Get("X-Lua")))
	}))
	defer upstream.Close()
	runnerValue, err := New(map[string]any{
		"rules": []any{map[string]any{"when": "true", "rewrite": map[string]any{"url": upstream.URL}}},
		"lua": map[string]any{"source": `function handle(original, request, matched_rules)
  request.headers["X-Lua"] = {original.host}
  return request
end`},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := runnerValue.(*Runner)
	services := newTestServices(t)
	result, err := runner.Run(t.Context(), step.Request{StepID: "proxy", WorkflowName: "test", RunDir: t.TempDir(), Services: services})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Cleanup(t.Context(), result) })

	caPEM, err := os.ReadFile(result.Outputs["ca_cert"].(string))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("generated CA certificate is invalid")
	}
	response, err := proxyClient(t, result, roots).Get("https://example.test/original")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "example.test" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
}

func TestForwardProxyRejectsOversizedBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("oversized request reached upstream")
	}))
	defer upstream.Close()
	runnerValue, err := New(map[string]any{"max_body_bytes": 4})
	if err != nil {
		t.Fatal(err)
	}
	runner := runnerValue.(*Runner)
	services := newTestServices(t)
	result, err := runner.Run(t.Context(), step.Request{StepID: "proxy", RunDir: t.TempDir(), Services: services})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Cleanup(t.Context(), result) })
	response, err := proxyClient(t, result, nil).Post(upstream.URL, "text/plain", strings.NewReader("12345"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

func TestHTTPRootCAFileUsesForwardProxyCA(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("through proxy"))
	}))
	defer upstream.Close()
	proxyRunnerValue, err := New(map[string]any{
		"rules": []any{map[string]any{"when": "true", "rewrite": map[string]any{"url": upstream.URL}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	proxyRunner := proxyRunnerValue.(*Runner)
	services := newTestServices(t)
	proxyResult, err := proxyRunner.Run(t.Context(), step.Request{StepID: "proxy", RunDir: t.TempDir(), Services: services})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxyRunner.Cleanup(t.Context(), proxyResult) })
	httpRunner, err := httpstep.New(map[string]any{
		"url":          "https://example.test/resource",
		"proxy":        map[string]any{"url": proxyResult.Outputs["url"]},
		"root_ca_file": proxyResult.Outputs["ca_cert"],
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := httpRunner.Run(t.Context(), step.Request{WorkflowDir: t.TempDir(), RunDir: t.TempDir(), Attempt: 1, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outputs["body"] != "through proxy" {
		t.Fatalf("body = %q", result.Outputs["body"])
	}
}

func TestForwardProxyConfigurationValidation(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"public listener", map[string]any{"listen": "0.0.0.0:8080"}, "loopback"},
		{"missing file path", map[string]any{"log": map[string]any{"destination": "file"}}, "path is required"},
		{"stdout path", map[string]any{"log": map[string]any{"destination": "stdout", "path": "x"}}, "only valid"},
		{"body variants", map[string]any{"rules": []any{map[string]any{"when": "true", "rewrite": map[string]any{"body": "a", "body_base64": "Yg=="}}}}, "mutually exclusive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestForwardProxyFileLogRedactsSecretsAndUsesPrivateMode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	runDir := t.TempDir()
	runnerValue, err := New(map[string]any{"log": map[string]any{
		"destination": "file", "path": "traffic/requests.jsonl", "include_bodies": true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner := runnerValue.(*Runner)
	services := newTestServices(t)
	result, err := runner.Run(t.Context(), step.Request{StepID: "proxy", RunDir: runDir, Services: services})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Cleanup(t.Context(), result) })
	request, err := http.NewRequest(http.MethodPost, upstream.URL+"?token=visible", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer visible")
	response, err := proxyClient(t, result, nil).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	path := result.Outputs["log_path"].(string)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record trafficRecord
	if err := json.Unmarshal(bytes.TrimSpace(data), &record); err != nil {
		t.Fatal(err)
	}
	if record.Original.Headers.Get("Authorization") != "[REDACTED]" || record.Original.Query.Get("token") != "[REDACTED]" {
		t.Fatalf("secrets were not redacted: %#v", record.Original)
	}
	if record.Original.Body != "body" {
		t.Fatalf("body = %q", record.Original.Body)
	}
	existingValue, err := New(map[string]any{"log": map[string]any{"destination": "file", "path": path}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := existingValue.Run(t.Context(), step.Request{StepID: "existing", RunDir: runDir, Services: services}); err == nil {
		t.Fatal("existing log file was overwritten")
	}
}

func TestRulesMatchOriginalRequestAndStopInOrder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(request.Method + ":" + request.Header.Get("X-Order")))
	}))
	defer upstream.Close()
	runnerValue, err := New(map[string]any{"rules": []any{
		map[string]any{"name": "method", "when": `original.method == "POST"`, "rewrite": map[string]any{"method": "PUT"}},
		map[string]any{"name": "still-original", "when": `original.method == "POST"`, "rewrite": map[string]any{"headers": map[string]any{"set": map[string]any{"X-Order": []any{"second"}}}}, "stop": true},
		map[string]any{"name": "stopped", "when": "true", "rewrite": map[string]any{"headers": map[string]any{"set": map[string]any{"X-Order": []any{"third"}}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner := runnerValue.(*Runner)
	services := newTestServices(t)
	result, err := runner.Run(t.Context(), step.Request{StepID: "proxy", RunDir: t.TempDir(), Services: services})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Cleanup(t.Context(), result) })
	response, err := proxyClient(t, result, nil).Post(upstream.URL, "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "PUT:second" {
		t.Fatalf("body = %q", body)
	}
}

func TestLuaErrorIsRequestLocal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	runnerValue, err := New(map[string]any{"lua": map[string]any{"source": `
function handle(original, request, matched_rules)
  error("nope")
end`}})
	if err != nil {
		t.Fatal(err)
	}
	runner := runnerValue.(*Runner)
	services := newTestServices(t)
	result, err := runner.Run(t.Context(), step.Request{StepID: "proxy", WorkflowName: "test", RunDir: t.TempDir(), Services: services})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Cleanup(t.Context(), result) })
	client := proxyClient(t, result, nil)
	for range 2 {
		response, err := client.Get(upstream.URL)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d", response.StatusCode)
		}
	}
}

func proxyClient(t *testing.T, result step.Result, roots *x509.CertPool) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(result.Outputs["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots}}}
}

func TestForwardProxyRejectsOriginFormRequests(t *testing.T) {
	runnerValue, err := New(map[string]any{"log": map[string]any{"destination": "stdout"}})
	if err != nil {
		t.Fatal(err)
	}
	runner := runnerValue.(*Runner)
	services := newTestServices(t)
	var stdout bytes.Buffer
	result, err := runner.Run(t.Context(), step.Request{StepID: "proxy", RunDir: t.TempDir(), Services: services, Stdout: &stdout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Cleanup(t.Context(), result) })
	// A request sent straight at the listener carries an origin-form URI. Forwarding it would send
	// it back to the listener's own address and loop until the process runs out of connections.
	response, err := http.Get(result.Outputs["url"].(string) + "/health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("log = %q", stdout.String())
	}
}

func TestRewritePreservesUntouchedQueryStrings(t *testing.T) {
	for _, raw := range []string{"http://h/p?b=2&a=1", "http://h/p?a=1;b=2", "http://h/p?debug", "http://h/p?q=a%20b"} {
		value, err := applyRewrite(requestValue{URL: raw}, Rewrite{Method: http.MethodGet})
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if value.URL != raw {
			t.Errorf("url = %q, want %q", value.URL, raw)
		}
	}
	value, err := applyRewrite(requestValue{URL: "http://h/p?b=2"}, Rewrite{Query: ValueChanges{Set: map[string][]string{"a": {"1"}}}})
	if err != nil || value.URL != "http://h/p?a=1&b=2" {
		t.Fatalf("url = %q, err = %v", value.URL, err)
	}
}

func TestLuaTransformPreservesAnUnchangedQuery(t *testing.T) {
	current := requestValue{Method: http.MethodGet, URL: "http://h/p?a=1;b=2", Scheme: "http", Host: "h", Path: "/p"}
	// A handler that returns the request table unmodified still carries every field back.
	fields := map[string]any{"method": http.MethodGet, "url": current.URL, "scheme": "http", "host": "h",
		"path": "/p", "query": map[string]any{}, "headers": map[string]any{}, "body": "", "body_base64": ""}
	value, err := applyLuaFields(current, fields)
	if err != nil {
		t.Fatal(err)
	}
	if value.URL != current.URL {
		t.Fatalf("url = %q, want %q", value.URL, current.URL)
	}
}

func TestTemplatedConfigurationSurvivesWorkflowValidation(t *testing.T) {
	// The engine builds a runner once against the unrendered configuration to validate the
	// workflow, so value checks must not reject text that is still a template.
	for _, raw := range []map[string]any{
		{"listen": "{{ .vars.listen }}"},
		{"max_body_bytes": "{{ .vars.limit }}"},
		{"log": map[string]any{"destination": "{{ .vars.destination }}", "path": "x"}},
	} {
		if _, err := New(raw); err != nil {
			t.Errorf("%v: %v", raw, err)
		}
	}
}

type syncBuffer struct {
	mu     sync.Mutex
	writes strings.Builder
}

func (buffer *syncBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.writes.Write(data)
}

func (buffer *syncBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.writes.String()
}

func TestForwardProxyDrainsTunnelHandlersBeforeClosingTheLog(t *testing.T) {
	// http.Server stops tracking a connection once it is hijacked, so Shutdown returns while a
	// CONNECT tunnel is still handling a request. Closing the traffic log at that point loses the
	// record and reports a write to a closed file on the step's stderr after the step has ended.
	for attempt := range 20 {
		if stderr := shutdownWithTunnelInFlight(t, attempt); strings.Contains(stderr, "forward proxy log") {
			t.Fatalf("attempt %d wrote the traffic log after it was closed: %s", attempt, stderr)
		}
	}
}

func shutdownWithTunnelInFlight(t *testing.T, attempt int) string {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-request.Context().Done():
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	runDir := t.TempDir()
	runnerValue, err := New(map[string]any{
		"rules": []any{map[string]any{"when": "true", "rewrite": map[string]any{"url": upstream.URL}}},
		"log":   map[string]any{"destination": "file", "path": fmt.Sprintf("tunnel-%d.jsonl", attempt)},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := runnerValue.(*Runner)
	services := newTestServices(t)
	stderr := &syncBuffer{}
	result, err := runner.Run(t.Context(), step.Request{StepID: "proxy", RunDir: runDir, Services: services, Stderr: stderr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Cleanup(t.Context(), result) })

	caPEM, err := os.ReadFile(result.Outputs["ca_cert"].(string))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("generated CA certificate is invalid")
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if response, err := proxyClient(t, result, roots).Get("https://example.test/slow"); err == nil {
			response.Body.Close()
		}
	}()
	<-entered
	services.cancel()
	services.wg.Wait()
	close(release)
	<-finished
	return stderr.String()
}

func TestRequestFromValueKeepsAnEmptyBodyUnchunked(t *testing.T) {
	parent := httptest.NewRequest(http.MethodPost, "http://origin.test/upload", nil)
	outbound, err := requestFromValue(parent, requestValue{Method: http.MethodPost, URL: "http://upstream.test/upload"})
	if err != nil {
		t.Fatal(err)
	}
	if outbound.Body != http.NoBody {
		t.Fatalf("body = %#v, want http.NoBody", outbound.Body)
	}
	if outbound.ContentLength != 0 {
		t.Fatalf("content length = %d, want 0", outbound.ContentLength)
	}
	var written bytes.Buffer
	if err := outbound.Write(&written); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(written.String(), "Transfer-Encoding: chunked") {
		t.Fatalf("empty body was sent chunked:\n%s", written.String())
	}
	if !strings.Contains(written.String(), "Content-Length: 0") {
		t.Fatalf("Content-Length: 0 is missing:\n%s", written.String())
	}
}

func TestRequestFromValueLetsAHostHeaderOverrideTheURLAuthority(t *testing.T) {
	parent := httptest.NewRequest(http.MethodGet, "http://origin.test/health", nil)
	value := requestValue{Method: http.MethodGet, URL: "http://staging.example.test/health",
		Headers: http.Header{"Host": {"api.example.test"}}}
	outbound, err := requestFromValue(parent, value)
	if err != nil {
		t.Fatal(err)
	}
	if outbound.Host != "api.example.test" {
		t.Fatalf("host = %q, want api.example.test", outbound.Host)
	}
	if outbound.URL.Host != "staging.example.test" {
		t.Fatalf("url host = %q, want staging.example.test", outbound.URL.Host)
	}
	var written bytes.Buffer
	if err := outbound.Write(&written); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(written.String(), "Host: api.example.test\r\n") {
		t.Fatalf("Host header override was dropped:\n%s", written.String())
	}
}

func TestLuaFileStaysInsideTheWorkflowPackage(t *testing.T) {
	workflow := t.TempDir()
	if err := os.WriteFile(workflow+"/transform.lua", []byte("function handle(request) return request end"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir() + "/secret.txt"
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workflowFile(workflow, "transform.lua"); err != nil {
		t.Fatalf("a package-relative file was rejected: %v", err)
	}
	for _, configured := range []string{outside, "../" + filepath.Base(outside), "../../etc/passwd"} {
		if _, err := workflowFile(workflow, configured); err == nil {
			t.Fatalf("%q escaped the workflow package", configured)
		}
	}
}
