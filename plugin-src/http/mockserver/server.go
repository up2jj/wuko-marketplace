package mockserver

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/expr-lang/expr"
	"github.com/google/uuid"
	"github.com/up2jj/wuko-marketplace/plugin-src/http/localtls"
	"github.com/up2jj/wuko/step"
	"github.com/up2jj/wuko/workflow"
)

const (
	shutdownTimeout       = 5 * time.Second
	verificationDetailCap = 20
)

type assertionFailure struct {
	Name       string `json:"name"`
	Message    string `json:"message"`
	Evaluation bool   `json:"-"`
}

type requestFailure struct {
	Kind        string
	Method      string
	URL         string
	Expectation string
	Assertions  []assertionFailure
	Message     string
}

type verificationState struct {
	Total   int
	Details []requestFailure
}

type mockServer struct {
	runner       *Runner
	execution    step.Request
	renderer     step.DataTemplateRenderer
	expectations []*compiledExpectation
	state        map[string]any
	logger       *trafficLogger
	encodeBase64 bool

	mu       sync.Mutex
	failures verificationState
	handlers sync.WaitGroup
}

type transactionResult struct {
	response    renderedResponse
	expectation string
	assertions  []string
	err         error
}

func (runner *Runner) Run(ctx context.Context, execution step.Request) (step.Result, error) {
	if execution.Services == nil {
		return step.Result{}, fmt.Errorf("managed service scope is unavailable")
	}
	renderer, ok := execution.TemplateRenderer.(step.DataTemplateRenderer)
	if !ok || renderer == nil {
		return step.Result{}, fmt.Errorf("mock_server requires a data-aware template renderer")
	}
	renderer = renderer.Snapshot()
	execution = freezeRequest(execution)
	execution.TemplateRenderer = renderer
	expectations, state, err := runner.load(ctx, execution, renderer, loadRun)
	if err != nil {
		return step.Result{}, err
	}
	listener, err := net.Listen("tcp", runner.config.Listen)
	if err != nil {
		return step.Result{}, fmt.Errorf("listening on %s: %w", runner.config.Listen, err)
	}
	if address, ok := listener.Addr().(*net.TCPAddr); !ok || !address.IP.IsLoopback() {
		listener.Close()
		return step.Result{}, fmt.Errorf("listen must resolve to a loopback address")
	}
	// TLS is prepared before the traffic log is opened: the log file is created on disk, and a
	// failure after that point would leave it behind for a step that never started.
	var authority *localtls.Authority
	var caPath string
	serveListener := listener
	scheme := "http"
	if runner.config.TLS {
		authority, err = localtls.NewAuthority(time.Now(), "Wuko Mock Server")
		if err != nil {
			return step.Result{}, closeStartup(listener, nil, "", err)
		}
		caPath, err = localtls.WriteCertificate(execution.RunDir, ".wuko-mock-server-ca-*.pem", authority.PEM())
		if err != nil {
			return step.Result{}, closeStartup(listener, nil, "", err)
		}
		host, _, splitErr := net.SplitHostPort(listener.Addr().String())
		if splitErr != nil {
			return step.Result{}, closeStartup(listener, nil, caPath, splitErr)
		}
		certificate, certErr := authority.CertificateFor(host)
		if certErr != nil {
			return step.Result{}, closeStartup(listener, nil, caPath, certErr)
		}
		serveListener = tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{*certificate}, MinVersion: tls.VersionTLS12})
		scheme = "https"
	}
	logger, logPath, err := openTrafficLogger(runner.config.Log, execution.Stdout, execution.RunDir)
	if err != nil {
		return step.Result{}, closeStartup(listener, nil, caPath, err)
	}
	server := &mockServer{runner: runner, execution: execution, renderer: renderer,
		expectations: expectations, state: state, logger: logger,
		encodeBase64: needsBodyBase64(runner.config.Log, expectations)}

	committed := make(chan struct{})
	abandoned := make(chan struct{})
	var abandonOnce sync.Once
	abandon := func() { abandonOnce.Do(func() { close(abandoned) }) }
	service := func(serviceCtx context.Context) error {
		select {
		case <-committed:
		case <-abandoned:
			listener.Close()
			logger.Close()
			return step.ErrServiceAborted
		case <-serviceCtx.Done():
			listener.Close()
			logger.Close()
			return step.ErrServiceAborted
		}
		serveErr := server.serve(serviceCtx, serveListener)
		closeErr := logger.Close()
		verifyErr := server.verify()
		if verifyErr != nil && cancellationOnly(serveErr) {
			serveErr = nil
		}
		return errors.Join(serveErr, wrapIf(closeErr, "closing traffic log"), verifyErr)
	}
	if err := execution.Services.StartService(execution.StepID, "mock_server", step.ServiceOptions{FailFast: true}, service); err != nil {
		abandon()
		listener.Close()
		logger.Close()
		if caPath != "" {
			os.Remove(caPath)
		}
		return step.Result{}, err
	}
	select {
	case <-ctx.Done():
		abandon()
		if caPath != "" {
			os.Remove(caPath)
		}
		return step.Result{}, ctx.Err()
	default:
	}
	close(committed)
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return step.Result{}, fmt.Errorf("reading mock server address: %w", err)
	}
	outputs := map[string]any{"ready": true, "url": scheme + "://" + net.JoinHostPort(host, port)}
	if authority != nil {
		outputs["ca_cert"] = caPath
		outputs["ca_sha256"] = authority.SHA256()
	}
	if logPath != "" {
		outputs["log_path"] = logPath
	}
	return step.Result{Outputs: outputs}, nil
}

func (runner *Runner) Cleanup(_ context.Context, result step.Result) error {
	path, _ := result.Outputs["ca_cert"].(string)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing mock server CA certificate %s: %w", path, err)
	}
	return nil
}

func closeStartup(listener net.Listener, logger *trafficLogger, caPath string, cause error) error {
	listener.Close()
	logger.Close()
	if caPath != "" {
		os.Remove(caPath)
	}
	return cause
}

// needsBodyBase64 reports whether any consumer of a request can observe its base64 body: an
// expectation that mentions one, or a traffic log configured to record bodies.
func needsBodyBase64(config *LogConfig, expectations []*compiledExpectation) bool {
	if config != nil && config.IncludeBodies {
		return true
	}
	return slices.ContainsFunc(expectations, func(expectation *compiledExpectation) bool {
		return expectation.readsBodyBase64
	})
}

// clientDisconnected reports a write that failed because the peer went away rather than because
// the mock produced something unwritable.
func clientDisconnected(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) ||
		errors.Is(err, http.ErrHandlerTimeout)
}

func wrapIf(err error, action string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
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
	return errors.Is(err, context.Canceled)
}

func freezeRequest(source step.Request) step.Request {
	source.Vars = workflow.CloneMap(source.Vars)
	source.PresetVars = workflow.CloneMap(source.PresetVars)
	source.Inputs = workflow.CloneMap(source.Inputs)
	source.Env = maps.Clone(source.Env)
	source.Steps = workflow.CloneMap(source.Steps)
	source.Dependencies = workflow.CloneDependencies(source.Dependencies)
	source.Bindings = workflow.CloneMap(source.Bindings)
	source.Providers = source.Providers.Clone()
	source.EnvironmentLoaders = slices.Clone(source.EnvironmentLoaders)
	return source
}

func (server *mockServer) serve(ctx context.Context, listener net.Listener) error {
	httpServer := &http.Server{Handler: server, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.Serve(listener) }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return server.drainHandlers()
		}
		return errors.Join(err, shutdownHTTP(httpServer, context.Background()), server.drainHandlers())
	case <-ctx.Done():
		shutdownErr := shutdownHTTP(httpServer, context.WithoutCancel(ctx))
		serveErr := <-errCh
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(ctx.Err(), shutdownErr, serveErr, server.drainHandlers())
	}
}

func shutdownHTTP(server *http.Server, base context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(base, shutdownTimeout)
	defer cancel()
	err := server.Shutdown(shutdownCtx)
	if err != nil {
		return errors.Join(err, server.Close())
	}
	return nil
}

func (server *mockServer) drainHandlers() error {
	drained := make(chan struct{})
	go func() {
		server.handlers.Wait()
		close(drained)
	}()
	timer := time.NewTimer(shutdownTimeout)
	defer timer.Stop()
	select {
	case <-drained:
		return nil
	case <-timer.C:
		return fmt.Errorf("timed out draining active mock request handlers")
	}
}

func (server *mockServer) ServeHTTP(writer http.ResponseWriter, raw *http.Request) {
	server.handlers.Add(1)
	defer server.handlers.Done()
	started := time.Now()
	requestID := uuid.NewString()
	request, err := readRequest(raw, server.runner.config.MaxBodyBytes, server.encodeBase64)
	if err != nil {
		server.mu.Lock()
		server.recordFailure(requestFailure{Kind: "malformed", Method: raw.Method, URL: requestURL(raw), Message: err.Error()})
		server.mu.Unlock()
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "malformed mock request", "message": err.Error()})
		server.log(trafficRecord{Timestamp: started.UTC(), RequestID: requestID,
			Request: logRequest{Method: raw.Method, URL: requestURL(raw), Path: requestPath(raw)},
			Status:  http.StatusBadRequest, DurationMS: elapsedMS(started), Error: err.Error()})
		return
	}
	result := server.transact(request)
	for name, values := range result.response.headers {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(result.response.status)
	var writeErr error
	if _, err := writer.Write(result.response.body); err != nil {
		writeErr = err
		// A client that hangs up mid-response is the client's own business: an http step whose
		// timeout fired, or a canceled run. The mock served the request correctly, so the loss is
		// logged but never promoted to a verification failure that fails the enclosing scope.
		if !clientDisconnected(err) {
			server.mu.Lock()
			server.recordFailure(requestFailure{Kind: "write", Method: request.Method, URL: request.URL,
				Expectation: result.expectation, Message: err.Error()})
			server.mu.Unlock()
		}
	}
	record := trafficRecord{Timestamp: started.UTC(), RequestID: requestID, Request: server.logger.request(request),
		Expectation: result.expectation, Assertions: result.assertions, Status: result.response.status, DurationMS: elapsedMS(started)}
	switch {
	case result.err != nil:
		record.Error = result.err.Error()
	case writeErr != nil:
		record.Error = writeErr.Error()
	}
	server.log(record)
}

func (server *mockServer) transact(request requestValue) transactionResult {
	server.mu.Lock()
	defer server.mu.Unlock()
	// One environment serves every candidate: it closes over &request and the pre-request state,
	// both of which are stable across the scan. Rebuilding it per expectation would re-clone the
	// workflow roots and re-bind every plugin helper closure once per expectation per request.
	environment := requestEnvironment(server.execution, &request, server.state)
	for _, expectation := range server.expectations {
		if expectation.config.Times != nil && expectation.used >= *expectation.config.Times {
			continue
		}
		request.Params = map[string]string{}
		matched, err := expr.Run(expectation.program, environment)
		if err != nil {
			return server.processingFailure(request, expectation.config.Name, fmt.Errorf("evaluating when: %w", err))
		}
		if !matched.(bool) {
			continue
		}
		failures := server.evaluateAssertions(expectation, request, environment)
		if len(failures) > 0 {
			server.recordFailure(requestFailure{Kind: "assertion", Method: request.Method, URL: request.URL,
				Expectation: expectation.config.Name, Assertions: failures})
			body := map[string]any{"error": "mock request assertion failed", "expectation": expectation.config.Name,
				"assertions": failures}
			response, _ := fixedJSON(http.StatusUnprocessableEntity, body)
			names := make([]string, len(failures))
			for index, failure := range failures {
				names[index] = failure.Name
			}
			return transactionResult{response: response, expectation: expectation.config.Name, assertions: names,
				err: fmt.Errorf("request assertions failed: %s", strings.Join(names, ", "))}
		}
		candidate, err := applyPatches(expectation, server.execution, request, server.state, server.renderer)
		if err != nil {
			return server.processingFailure(request, expectation.config.Name, err)
		}
		responseEnvironment := requestEnvironment(server.execution, &request, candidate)
		response, err := expectation.response.render(responseEnvironment, server.renderer, request, candidate)
		if err != nil {
			return server.processingFailure(request, expectation.config.Name, err)
		}
		server.state = candidate
		expectation.used++
		return transactionResult{response: response, expectation: expectation.config.Name}
	}
	message := "no mock expectation matched request"
	server.recordFailure(requestFailure{Kind: "unmatched", Method: request.Method, URL: request.URL, Message: message})
	response, _ := fixedJSON(http.StatusNotFound, map[string]any{"error": message})
	return transactionResult{response: response, err: errors.New(message)}
}

func (server *mockServer) evaluateAssertions(expectation *compiledExpectation, request requestValue, environment map[string]any) []assertionFailure {
	var failures []assertionFailure
	extra := map[string]any{"request": request.templateValue(), "state": server.state}
	for _, assertion := range expectation.assertions {
		value, err := expr.Run(assertion.program, environment)
		if err == nil && value.(bool) {
			continue
		}
		failure := assertionFailure{Name: assertion.config.Name, Evaluation: err != nil}
		switch {
		case assertion.config.Message != "":
			message, renderErr := server.renderer.RenderContentWith(assertion.config.Message, extra)
			if renderErr != nil {
				failure.Message = fmt.Sprintf("assertion message template failed: %v", renderErr)
				failure.Evaluation = true
			} else {
				failure.Message = message
			}
		case err != nil:
			failure.Message = fmt.Sprintf("assertion evaluation failed: %v", err)
		default:
			failure.Message = "assertion expression evaluated to false"
		}
		failures = append(failures, failure)
	}
	return failures
}

func (server *mockServer) processingFailure(request requestValue, expectation string, cause error) transactionResult {
	server.recordFailure(requestFailure{Kind: "processing", Method: request.Method, URL: request.URL,
		Expectation: expectation, Message: cause.Error()})
	response, _ := fixedJSON(http.StatusInternalServerError, map[string]any{"error": "mock request processing failed"})
	return transactionResult{response: response, expectation: expectation, err: cause}
}

func (server *mockServer) recordFailure(failure requestFailure) {
	server.failures.Total++
	if len(server.failures.Details) < verificationDetailCap {
		server.failures.Details = append(server.failures.Details, failure)
	}
}

func (server *mockServer) verify() error {
	server.mu.Lock()
	defer server.mu.Unlock()
	var lines []string
	if server.failures.Total > 0 {
		lines = append(lines, fmt.Sprintf("%d request failure(s)", server.failures.Total))
		for _, failure := range server.failures.Details {
			line := fmt.Sprintf("%s %s: %s", failure.Method, failure.URL, failure.Kind)
			if failure.Expectation != "" {
				line += " expectation " + failure.Expectation
			}
			if len(failure.Assertions) > 0 {
				names := make([]string, len(failure.Assertions))
				for index, assertion := range failure.Assertions {
					kind := "failed"
					if assertion.Evaluation {
						kind = "evaluation error"
					}
					names[index] = assertion.Name + " (" + kind + "): " + assertion.Message
				}
				line += ": " + strings.Join(names, "; ")
			} else if failure.Message != "" {
				line += ": " + failure.Message
			}
			lines = append(lines, line)
		}
		if server.failures.Total > len(server.failures.Details) {
			lines = append(lines, fmt.Sprintf("... %d additional failed request(s) omitted", server.failures.Total-len(server.failures.Details)))
		}
	}
	for _, expectation := range server.expectations {
		if expectation.config.Times != nil && expectation.used != *expectation.config.Times {
			lines = append(lines, fmt.Sprintf("expectation %q matched %d time(s), expected %d", expectation.config.Name, expectation.used, *expectation.config.Times))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("mock server verification failed:\n- %s", strings.Join(lines, "\n- "))
}

func (server *mockServer) log(record trafficRecord) {
	if err := server.logger.Write(record); err != nil {
		server.mu.Lock()
		server.recordFailure(requestFailure{Kind: "logging", Method: record.Request.Method,
			URL: record.Request.URL, Expectation: record.Expectation, Message: err.Error()})
		server.mu.Unlock()
		if server.execution.Stderr != nil {
			fmt.Fprintf(server.execution.Stderr, "mock_server: %v\n", err)
		}
	}
}

func fixedJSON(status int, value any) (renderedResponse, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return renderedResponse{}, err
	}
	return renderedResponse{status: status, headers: http.Header{"Content-Type": []string{"application/json"}}, body: body}, nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	response, _ := fixedJSON(status, value)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(response.status)
	_, _ = writer.Write(response.body)
}

func requestURL(request *http.Request) string {
	if request.URL == nil {
		return ""
	}
	return request.URL.String()
}

func requestPath(request *http.Request) string {
	if request.URL == nil {
		return ""
	}
	return request.URL.Path
}

func elapsedMS(started time.Time) float64 {
	return float64(time.Since(started)) / float64(time.Millisecond)
}
