package mockserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type trafficLogger struct {
	mu            sync.Mutex
	writer        io.Writer
	closer        io.Closer
	includeBodies bool
	redactHeaders map[string]struct{}
}

type trafficRecord struct {
	Timestamp   time.Time  `json:"timestamp"`
	RequestID   string     `json:"request_id"`
	Request     logRequest `json:"request"`
	Expectation string     `json:"expectation,omitempty"`
	Assertions  []string   `json:"assertions,omitempty"`
	Status      int        `json:"status"`
	DurationMS  float64    `json:"duration_ms"`
	Error       string     `json:"error,omitempty"`
}

type logRequest struct {
	Method     string              `json:"method"`
	URL        string              `json:"url"`
	Path       string              `json:"path"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Query      map[string][]string `json:"query,omitempty"`
	Body       string              `json:"body,omitempty"`
	BodyBase64 string              `json:"body_base64,omitempty"`
}

func openTrafficLogger(config *LogConfig, requestWriter io.Writer, runDir string) (*trafficLogger, string, error) {
	if config == nil {
		return nil, "", nil
	}
	logger := &trafficLogger{includeBodies: config.IncludeBodies, redactHeaders: map[string]struct{}{
		"authorization": {}, "proxy-authorization": {}, "cookie": {}, "set-cookie": {},
	}}
	for _, name := range config.RedactHeaders {
		logger.redactHeaders[strings.ToLower(name)] = struct{}{}
	}
	if config.Destination == "stdout" {
		if requestWriter == nil {
			requestWriter = io.Discard
		}
		logger.writer = requestWriter
		return logger, "", nil
	}
	path := config.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(runDir, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolving log path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, "", fmt.Errorf("creating log directory: %w", err)
	}
	flags := os.O_CREATE | os.O_WRONLY
	if config.Overwrite {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("opening log file %s: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, "", fmt.Errorf("setting log file permissions: %w", err)
	}
	logger.writer, logger.closer = file, file
	return logger, filepath.Clean(path), nil
}

func (logger *trafficLogger) Close() error {
	if logger == nil || logger.closer == nil {
		return nil
	}
	return logger.closer.Close()
}

func (logger *trafficLogger) Write(record trafficRecord) error {
	if logger == nil {
		return nil
	}
	record.Request = logger.redact(record.Request)
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encoding traffic log: %w", err)
	}
	data = append(data, '\n')
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if _, err := logger.writer.Write(data); err != nil {
		return fmt.Errorf("writing traffic log: %w", err)
	}
	return nil
}

func (logger *trafficLogger) request(value requestValue) logRequest {
	return logRequest{Method: value.Method, URL: value.URL, Path: value.Path,
		Headers: cloneStringLists(value.Headers), Query: cloneStringLists(value.QueryAll),
		Body: value.Body, BodyBase64: value.BodyBase64}
}

func (logger *trafficLogger) redact(value logRequest) logRequest {
	value.Headers = cloneStringLists(value.Headers)
	for name := range value.Headers {
		if _, sensitive := logger.redactHeaders[strings.ToLower(name)]; sensitive {
			value.Headers[name] = []string{"[REDACTED]"}
		}
	}
	value.Query = cloneStringLists(value.Query)
	for name := range value.Query {
		if sensitiveQuery(name) {
			value.Query[name] = []string{"[REDACTED]"}
		}
	}
	if parsed, err := url.Parse(value.URL); err == nil {
		query := parsed.Query()
		for name := range query {
			if sensitiveQuery(name) {
				query[name] = []string{"[REDACTED]"}
			}
		}
		parsed.RawQuery = query.Encode()
		value.URL = parsed.String()
	}
	if !logger.includeBodies {
		value.Body = ""
		value.BodyBase64 = ""
	}
	return value
}

func sensitiveQuery(name string) bool {
	switch strings.ToLower(name) {
	case "token", "access_token", "api_key", "apikey", "password", "secret":
		return true
	default:
		return false
	}
}
