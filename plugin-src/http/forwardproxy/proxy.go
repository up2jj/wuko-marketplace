package forwardproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/expr-lang/expr"
	"github.com/google/uuid"
	"github.com/up2jj/wuko/step"
	glua "github.com/yuin/gopher-lua"
)

type requestValue struct {
	Method     string      `json:"method" expr:"method"`
	URL        string      `json:"url" expr:"url"`
	Scheme     string      `json:"scheme" expr:"scheme"`
	Host       string      `json:"host" expr:"host"`
	Path       string      `json:"path" expr:"path"`
	Query      url.Values  `json:"query,omitempty" expr:"query"`
	Headers    http.Header `json:"headers,omitempty" expr:"headers"`
	Body       string      `json:"body,omitempty" expr:"body"`
	BodyBase64 string      `json:"body_base64,omitempty" expr:"body_base64"`
}

type proxyServer struct {
	runner    *Runner
	execution step.Request
	authority *certificateAuthority
	transport *http.Transport
	logger    *trafficLogger
	luaProto  *glua.FunctionProto
	// address is the listener's own host:port. Forwarding a request back to it would make the
	// proxy call itself, so requests that resolve to it are refused instead of looping.
	address string
	conns   connectionSet
	// handlers counts request handlers still running. http.Server stops tracking a connection once
	// it is hijacked, so Shutdown returns while CONNECT tunnels and protocol upgrades are still
	// writing to the traffic log; shutdown waits on this before the log is closed.
	handlers sync.WaitGroup
	failure  chan error
	failOnce sync.Once
}

func (runner *Runner) Run(ctx context.Context, execution step.Request) (step.Result, error) {
	if execution.Services == nil {
		return step.Result{}, fmt.Errorf("managed service scope is unavailable")
	}
	listener, err := net.Listen("tcp", runner.config.Listen)
	if err != nil {
		return step.Result{}, fmt.Errorf("listening on %s: %w", runner.config.Listen, err)
	}
	if address, ok := listener.Addr().(*net.TCPAddr); !ok || !address.IP.IsLoopback() {
		listener.Close()
		return step.Result{}, fmt.Errorf("listen must resolve to a loopback address")
	}
	authority, err := newCertificateAuthority(time.Now())
	if err != nil {
		listener.Close()
		return step.Result{}, err
	}
	caPath, err := writeCACertificate(execution.RunDir, authority.PEM())
	if err != nil {
		listener.Close()
		return step.Result{}, err
	}
	logger, logPath, err := openTrafficLogger(runner.config.Log, execution.Stdout, execution.RunDir)
	if err != nil {
		listener.Close()
		os.Remove(caPath)
		return step.Result{}, err
	}
	luaProto, err := runner.loadLua(ctx, execution)
	if err != nil {
		listener.Close()
		logger.Close()
		os.Remove(caPath)
		return step.Result{}, err
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		listener.Close()
		logger.Close()
		os.Remove(caPath)
		return step.Result{}, fmt.Errorf("default HTTP transport has unsupported type %T", http.DefaultTransport)
	}
	server := &proxyServer{runner: runner, execution: execution, authority: authority,
		transport: defaultTransport.Clone(), logger: logger, luaProto: luaProto,
		address: listener.Addr().String(), failure: make(chan error, 1)}
	server.transport.Proxy = nil
	server.transport.DisableCompression = true
	// A forward proxy funnels concurrent traffic into a handful of upstreams. Left at zero this
	// falls back to DefaultMaxIdleConnsPerHost, so anything past the second in-flight request to a
	// host has its connection closed on completion and re-dialed on the next one.
	server.transport.MaxIdleConnsPerHost = server.transport.MaxIdleConns

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
		err := server.serve(serviceCtx, listener)
		if closeErr := logger.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("closing traffic log: %w", closeErr)
		}
		return err
	}
	if err := execution.Services.StartService(execution.StepID, "forward_proxy", step.ServiceOptions{
		KeepAlive: runner.config.KeepAlive, FailFast: true,
	}, service); err != nil {
		abandon()
		listener.Close()
		logger.Close()
		os.Remove(caPath)
		return step.Result{}, err
	}
	select {
	case <-ctx.Done():
		abandon()
		os.Remove(caPath)
		return step.Result{}, ctx.Err()
	default:
	}
	close(committed)
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return step.Result{}, fmt.Errorf("reading proxy address: %w", err)
	}
	proxyURL := "http://" + net.JoinHostPort(host, port)
	outputs := map[string]any{"ready": true, "url": proxyURL, "ca_cert": caPath, "ca_sha256": authority.SHA256()}
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
		return fmt.Errorf("removing proxy CA certificate %s: %w", path, err)
	}
	return nil
}

func validateListen(address string) error {
	if templated(address) {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listen must be a host:port address: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen must use a loopback address")
	}
	return nil
}

// drainTimeout bounds both the graceful shutdown of tracked connections and the wait for handlers
// the server no longer tracks.
const drainTimeout = 5 * time.Second

func (proxy *proxyServer) serve(ctx context.Context, listener net.Listener) error {
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener) }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			proxy.drainHandlers()
			return nil
		}
		return errors.Join(err, proxy.shutdown(server, context.Background()))
	case failure := <-proxy.failure:
		shutdownErr := proxy.shutdown(server, context.Background())
		serveErr := <-errCh
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(failure, shutdownErr, serveErr)
	case <-ctx.Done():
		shutdownErr := proxy.shutdown(server, context.WithoutCancel(ctx))
		serveErr := <-errCh
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		if shutdownErr != nil || serveErr != nil {
			return errors.Join(shutdownErr, serveErr)
		}
		return ctx.Err()
	}
}

func (proxy *proxyServer) shutdown(server *http.Server, base context.Context) error {
	proxy.conns.closeAll()
	proxy.transport.CloseIdleConnections()
	shutdownCtx, cancel := context.WithTimeout(base, drainTimeout)
	defer cancel()
	err := server.Shutdown(shutdownCtx)
	proxy.drainHandlers()
	return err
}

// drainHandlers waits for in-flight request handlers to return so the caller can close the traffic
// log without racing a handler that is still writing to it. Shutdown alone is not enough: a
// hijacked connection is no longer tracked by the server, so its handler outlives Shutdown.
func (proxy *proxyServer) drainHandlers() {
	drained := make(chan struct{})
	go func() { proxy.handlers.Wait(); close(drained) }()
	timer := time.NewTimer(drainTimeout)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
	}
}

func (proxy *proxyServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// A CONNECT handler owns its tunnel for as long as the decrypted server runs on it, and that
	// server only stops once the handler goroutine it spawned has closed the connection, so
	// counting this call also covers every request carried inside the tunnel.
	proxy.handlers.Add(1)
	defer proxy.handlers.Done()
	if request.Method == http.MethodConnect {
		proxy.connect(writer, request)
		return
	}
	// A forward proxy only ever receives absolute-form request URIs. An origin-form request sent
	// straight at the listener -- a browser opening the proxy URL, or a readiness probe against it
	// -- would otherwise be forwarded to the listener's own address and loop until the process runs
	// out of connections.
	if request.URL == nil || request.URL.Host == "" {
		http.Error(writer, "forward proxy requires an absolute request URI", http.StatusBadRequest)
		return
	}
	proxy.forward(writer, request)
}

// targetsProxy reports that a transformed request would be sent back to this proxy. The listener
// is loopback-only, so any loopback host on the listening port is the proxy itself.
func (proxy *proxyServer) targetsProxy(target *url.URL) bool {
	_, listenPort, err := net.SplitHostPort(proxy.address)
	if err != nil {
		return false
	}
	port := target.Port()
	if port == "" {
		port = "80"
		if target.Scheme == "https" {
			port = "443"
		}
	}
	if port != listenPort {
		return false
	}
	host := target.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (proxy *proxyServer) connect(writer http.ResponseWriter, request *http.Request) {
	host, _, err := net.SplitHostPort(request.Host)
	if err != nil {
		host = request.Host
	}
	if strings.TrimSpace(host) == "" {
		http.Error(writer, "CONNECT target is required", http.StatusBadRequest)
		return
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		http.Error(writer, "connection hijacking is unavailable", http.StatusInternalServerError)
		return
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	proxy.conns.add(connection)
	defer proxy.conns.remove(connection)
	defer connection.Close()
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	// Hijack hands back whatever the server already read past the CONNECT request. A client that
	// pipelines its ClientHello behind CONNECT leaves those bytes here, so they must be replayed
	// ahead of the socket or the handshake stalls waiting for data that was already delivered.
	client := net.Conn(connection)
	if pending := buffered.Reader.Buffered(); pending > 0 {
		prefix, err := buffered.Reader.Peek(pending)
		if err != nil {
			return
		}
		client = &prefixedConn{Conn: connection, prefix: bytes.NewReader(bytes.Clone(prefix))}
	}
	tlsConnection := tls.Server(client, &tls.Config{GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		name := hello.ServerName
		if name == "" {
			name = host
		}
		return proxy.authority.CertificateFor(name)
	}, MinVersion: tls.VersionTLS12})
	// The request context is not canceled while this handler runs, so an idle client would hold the
	// goroutine for the life of the step without a deadline of its own.
	handshakeCtx, cancelHandshake := context.WithTimeout(request.Context(), 10*time.Second)
	err = tlsConnection.HandshakeContext(handshakeCtx)
	cancelHandshake()
	if err != nil {
		return
	}
	listener := newSingleConnListener(tlsConnection)
	decrypted := &http.Server{ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Scheme == "" {
			r.URL.Scheme = "https"
		}
		if r.URL.Host == "" {
			r.URL.Host = r.Host
			if r.URL.Host == "" {
				r.URL.Host = request.Host
			}
		}
		proxy.forward(w, r)
	})}
	if err := decrypted.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		return
	}
}

func (proxy *proxyServer) forward(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	recorder := &responseRecorder{ResponseWriter: writer, status: http.StatusOK, conns: &proxy.conns}
	requestID := uuid.NewString()
	body, tooLarge, err := readBody(request.Body, int64(proxy.runner.config.MaxBodyBytes))
	if err != nil {
		http.Error(recorder, "reading request body", http.StatusBadRequest)
		proxy.writeLog(trafficRecord{Timestamp: started.UTC(), RequestID: requestID, Status: recorder.status,
			DurationMS: milliseconds(time.Since(started)), Error: err.Error()})
		return
	}
	original := requestSnapshot(request, body)
	if tooLarge {
		http.Error(recorder, "request body exceeds max_body_bytes", http.StatusRequestEntityTooLarge)
		proxy.writeLog(trafficRecord{Timestamp: started.UTC(), RequestID: requestID, Original: original, Request: original,
			Status: recorder.status, DurationMS: milliseconds(time.Since(started)), Error: "request body exceeds max_body_bytes"})
		return
	}
	final := original
	matched := make([]string, 0, len(proxy.runner.rules))
	// Every rule sees the same original request, so the environment is built once per request
	// rather than once per rule: it copies the provider roots and rebinds every plugin helper.
	var environment map[string]any
	if len(proxy.runner.rules) > 0 {
		environment = proxy.execution.ExpressionEnvironment(map[string]any{"original": original})
	}
	for index, rule := range proxy.runner.rules {
		value, evalErr := expr.Run(rule.program, environment)
		if evalErr != nil {
			proxy.localError(recorder, started, requestID, original, final, matched, fmt.Errorf("evaluating rule %q: %w", ruleName(rule.rule, index), evalErr))
			return
		}
		matches, _ := value.(bool)
		if !matches {
			continue
		}
		matched = append(matched, ruleName(rule.rule, index))
		final, err = applyRewrite(final, rule.rule.Rewrite)
		if err != nil {
			proxy.localError(recorder, started, requestID, original, final, matched, fmt.Errorf("applying rule %q: %w", ruleName(rule.rule, index), err))
			return
		}
		if rule.rule.Stop {
			break
		}
	}
	if proxy.luaProto != nil {
		final, err = proxy.applyLua(request.Context(), original, final, matched)
		if err != nil {
			proxy.localError(recorder, started, requestID, original, final, matched, err)
			return
		}
	}
	outbound, err := requestFromValue(request, final)
	if err != nil {
		proxy.localError(recorder, started, requestID, original, final, matched, err)
		return
	}
	if proxy.targetsProxy(outbound.URL) {
		proxy.localError(recorder, started, requestID, original, final, matched,
			fmt.Errorf("transformed request targets the proxy itself"))
		return
	}
	var proxyErr error
	reverse := &httputil.ReverseProxy{Transport: proxy.transport, BufferPool: responseBuffers{}, Rewrite: func(proxyRequest *httputil.ProxyRequest) {
		proxyRequest.Out.URL = outbound.URL
		proxyRequest.Out.Host = outbound.Host
		proxyRequest.Out.Header = outbound.Header
		proxyRequest.Out.Body = outbound.Body
		proxyRequest.Out.ContentLength = outbound.ContentLength
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, roundTripErr error) {
		proxyErr = roundTripErr
		http.Error(w, "proxy upstream request failed", http.StatusBadGateway)
	}}
	reverse.ServeHTTP(recorder, outbound)
	record := trafficRecord{Timestamp: started.UTC(), RequestID: requestID, Original: original, Request: final,
		MatchedRules: matched, Status: recorder.status, DurationMS: milliseconds(time.Since(started))}
	if proxyErr != nil {
		record.Error = proxyErr.Error()
	}
	proxy.writeLog(record)
}

func (proxy *proxyServer) localError(writer http.ResponseWriter, started time.Time, id string, original, final requestValue, matched []string, err error) {
	http.Error(writer, "proxy request transform failed", http.StatusBadGateway)
	proxy.writeLog(trafficRecord{Timestamp: started.UTC(), RequestID: id, Original: original, Request: final,
		MatchedRules: matched, Status: http.StatusBadGateway, DurationMS: milliseconds(time.Since(started)), Error: err.Error()})
}

func (proxy *proxyServer) writeLog(record trafficRecord) {
	if err := proxy.logger.Write(record); err != nil && proxy.execution.Stderr != nil {
		fmt.Fprintf(proxy.execution.Stderr, "forward proxy log: %v\n", err)
		proxy.failOnce.Do(func() { proxy.failure <- err })
	} else if err != nil {
		proxy.failOnce.Do(func() { proxy.failure <- err })
	}
}

func readBody(body io.ReadCloser, limit int64) ([]byte, bool, error) {
	if body == nil {
		return nil, false, nil
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, false, err
	}
	return data, int64(len(data)) > limit, nil
}

func requestSnapshot(request *http.Request, body []byte) requestValue {
	requestURL := cloneURL(request.URL)
	if requestURL.Scheme == "" {
		if request.TLS != nil {
			requestURL.Scheme = "https"
		} else {
			requestURL.Scheme = "http"
		}
	}
	if requestURL.Host == "" {
		requestURL.Host = request.Host
	}
	return requestValue{Method: request.Method, URL: requestURL.String(), Scheme: requestURL.Scheme,
		Host: requestURL.Host, Path: requestURL.Path, Query: cloneValues(requestURL.Query()),
		Headers: cloneHeader(request.Header), Body: string(body), BodyBase64: base64.StdEncoding.EncodeToString(body)}
}

func applyRewrite(value requestValue, rewrite Rewrite) (requestValue, error) {
	parsed, err := url.Parse(value.URL)
	if err != nil {
		return value, err
	}
	if rewrite.URL != "" {
		parsed, err = url.Parse(rewrite.URL)
		if err != nil {
			return value, fmt.Errorf("parsing url: %w", err)
		}
	} else {
		if rewrite.Scheme != "" {
			parsed.Scheme = rewrite.Scheme
		}
		if rewrite.Host != "" {
			parsed.Host = rewrite.Host
		}
		if rewrite.Path != "" {
			parsed.Path = rewrite.Path
			parsed.RawPath = ""
		}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return value, fmt.Errorf("url must use http or https")
	}
	if parsed.Host == "" {
		return value, fmt.Errorf("url must include a host")
	}
	if rewrite.Method != "" {
		value.Method = rewrite.Method
	}
	value.Headers = applyHeaderChanges(value.Headers, rewrite.Headers)
	// Re-encoding a query that no rule touched is not lossless: url.Values sorts keys, turns %20
	// into +, adds "=" to bare keys, and drops semicolon-separated pairs entirely. Leave the raw
	// query exactly as the client sent it unless this rule actually changes it.
	query := parsed.Query()
	if len(rewrite.Query.Set)+len(rewrite.Query.Add)+len(rewrite.Query.Remove) > 0 {
		query = applyValueChanges(query, rewrite.Query)
		parsed.RawQuery = query.Encode()
	}
	if rewrite.Body != nil {
		value.Body = *rewrite.Body
		value.BodyBase64 = base64.StdEncoding.EncodeToString([]byte(value.Body))
	}
	if rewrite.BodyBase64 != nil {
		body, err := base64.StdEncoding.DecodeString(*rewrite.BodyBase64)
		if err != nil {
			return value, fmt.Errorf("decoding body_base64: %w", err)
		}
		value.Body, value.BodyBase64 = string(body), *rewrite.BodyBase64
	}
	value.URL, value.Scheme, value.Host, value.Path, value.Query = parsed.String(), parsed.Scheme, parsed.Host, parsed.Path, query
	return value, nil
}

func applyHeaderChanges(header http.Header, changes ValueChanges) http.Header {
	result := cloneHeader(header)
	if result == nil {
		result = make(http.Header)
	}
	for _, name := range changes.Remove {
		result.Del(name)
	}
	for name, values := range changes.Set {
		result.Del(name)
		for _, value := range values {
			result.Add(name, value)
		}
	}
	for name, values := range changes.Add {
		for _, value := range values {
			result.Add(name, value)
		}
	}
	return result
}

func applyValueChanges(values url.Values, changes ValueChanges) url.Values {
	result := cloneValues(values)
	if result == nil {
		result = make(url.Values)
	}
	for _, name := range changes.Remove {
		result.Del(name)
	}
	for name, replacements := range changes.Set {
		result[name] = append([]string(nil), replacements...)
	}
	for name, additions := range changes.Add {
		result[name] = append(result[name], additions...)
	}
	return result
}

func requestFromValue(parent *http.Request, value requestValue) (*http.Request, error) {
	parsed, err := url.Parse(value.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("transformed request has an invalid HTTP URL")
	}
	body, err := base64.StdEncoding.DecodeString(value.BodyBase64)
	if err != nil {
		return nil, fmt.Errorf("transformed request body_base64 is invalid: %w", err)
	}
	request := parent.Clone(parent.Context())
	request.Method, request.URL, request.Host = value.Method, parsed, parsed.Host
	request.RequestURI = ""
	request.Header = cloneHeader(value.Headers)
	// net/http writes the Host line from Request.Host and never from the header map, so a rule that
	// sets a Host header would otherwise be dropped without a word. Let an explicit one win over the
	// URL authority, which is the whole point of setting it.
	if host := request.Header.Get("Host"); host != "" {
		request.Host = host
		request.Header.Del("Host")
	}
	removeHopHeaders(request.Header)
	// An empty body has to stay NoBody. ReverseProxy nils out a zero-length body itself, but the
	// Rewrite hook below puts ours back, and a non-nil body with ContentLength 0 makes outgoingLength
	// report -1, so net/http sends the request chunked and drops Content-Length.
	if len(body) == 0 {
		request.Body = http.NoBody
	} else {
		request.Body = io.NopCloser(bytes.NewReader(body))
	}
	request.ContentLength = int64(len(body))
	request.GetBody = func() (io.ReadCloser, error) {
		if len(body) == 0 {
			return http.NoBody, nil
		}
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return request, nil
}

func removeHopHeaders(header http.Header) {
	upgrade := header.Get("Upgrade")
	keepUpgrade := false
	for _, connection := range header.Values("Connection") {
		for name := range strings.SplitSeq(connection, ",") {
			if strings.EqualFold(strings.TrimSpace(name), "upgrade") && upgrade != "" {
				keepUpgrade = true
			}
		}
	}
	for _, connection := range header.Values("Connection") {
		for name := range strings.SplitSeq(connection, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
	if keepUpgrade {
		header.Set("Connection", "Upgrade")
		header.Set("Upgrade", upgrade)
	}
}

func cloneURL(source *url.URL) *url.URL {
	if source == nil {
		return &url.URL{}
	}
	copy := *source
	return &copy
}

// responseBuffers hands ReverseProxy the copy buffer it would otherwise allocate anew, at 32 KiB,
// for every proxied response.
type responseBuffers struct{}

var responseBufferPool = sync.Pool{New: func() any { buffer := make([]byte, 32*1024); return &buffer }}

func (responseBuffers) Get() []byte { return *responseBufferPool.Get().(*[]byte) }

func (responseBuffers) Put(buffer []byte) { responseBufferPool.Put(&buffer) }

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	conns  *connectionSet
}

func (writer *responseRecorder) WriteHeader(status int) {
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *responseRecorder) Flush() { http.NewResponseController(writer.ResponseWriter).Flush() }

func (writer *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := writer.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	connection, buffered, err := hijacker.Hijack()
	if err == nil {
		managed := &managedConn{Conn: connection, set: writer.conns}
		writer.conns.add(managed)
		connection = managed
	}
	return connection, buffered, err
}

func (writer *responseRecorder) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

type connectionSet struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

type managedConn struct {
	net.Conn
	once sync.Once
	set  *connectionSet
}

func (connection *managedConn) Close() error {
	err := connection.Conn.Close()
	connection.once.Do(func() { connection.set.remove(connection) })
	return err
}

func (set *connectionSet) add(connection net.Conn) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.conns == nil {
		set.conns = make(map[net.Conn]struct{})
	}
	set.conns[connection] = struct{}{}
}

func (set *connectionSet) remove(connection net.Conn) {
	set.mu.Lock()
	delete(set.conns, connection)
	set.mu.Unlock()
}

func (set *connectionSet) closeAll() {
	set.mu.Lock()
	connections := make([]net.Conn, 0, len(set.conns))
	for connection := range set.conns {
		connections = append(connections, connection)
	}
	set.mu.Unlock()
	for _, connection := range connections {
		connection.Close()
	}
}

// prefixedConn replays bytes the HTTP server had already buffered before the connection was
// hijacked, then reads straight from the socket.
type prefixedConn struct {
	net.Conn
	prefix *bytes.Reader
}

func (connection *prefixedConn) Read(buffer []byte) (int, error) {
	if connection.prefix != nil {
		if read, err := connection.prefix.Read(buffer); read > 0 {
			return read, nil
		} else if err == nil {
			return 0, nil
		}
		connection.prefix = nil
	}
	return connection.Conn.Read(buffer)
}

type singleConnListener struct {
	connection net.Conn
	once       sync.Once
	closed     chan struct{}
}

func newSingleConnListener(connection net.Conn) *singleConnListener {
	return &singleConnListener{connection: connection, closed: make(chan struct{})}
}

func (listener *singleConnListener) Accept() (net.Conn, error) {
	var connection net.Conn
	listener.once.Do(func() { connection = &closingConn{Conn: listener.connection, closed: listener.closed} })
	if connection != nil {
		return connection, nil
	}
	<-listener.closed
	return nil, net.ErrClosed
}

func (listener *singleConnListener) Close() error {
	return listener.connection.Close()
}

func (listener *singleConnListener) Addr() net.Addr { return listener.connection.LocalAddr() }

type closingConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (connection *closingConn) Close() error {
	err := connection.Conn.Close()
	connection.once.Do(func() { close(connection.closed) })
	return err
}
