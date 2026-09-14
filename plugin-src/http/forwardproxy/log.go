package forwardproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	Timestamp    time.Time    `json:"timestamp"`
	RequestID    string       `json:"request_id"`
	Original     requestValue `json:"original"`
	Request      requestValue `json:"request"`
	MatchedRules []string     `json:"matched_rules,omitempty"`
	Status       int          `json:"status"`
	DurationMS   float64      `json:"duration_ms"`
	Error        string       `json:"error,omitempty"`
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
	record.Original = logger.redact(record.Original)
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

func (logger *trafficLogger) redact(value requestValue) requestValue {
	value.Headers = cloneHeader(value.Headers)
	for name := range value.Headers {
		if _, sensitive := logger.redactHeaders[strings.ToLower(name)]; sensitive {
			value.Headers[name] = []string{"[REDACTED]"}
		}
	}
	value.Query = cloneValues(value.Query)
	for name := range value.Query {
		switch strings.ToLower(name) {
		case "token", "access_token", "api_key", "apikey", "password", "secret":
			value.Query[name] = []string{"[REDACTED]"}
		}
	}
	if !logger.includeBodies {
		value.Body = ""
		value.BodyBase64 = ""
	}
	return value
}

func cloneHeader(source http.Header) http.Header {
	if source == nil {
		return nil
	}
	return source.Clone()
}

func cloneValues(source url.Values) url.Values {
	if source == nil {
		return nil
	}
	result := make(url.Values, len(source))
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}
