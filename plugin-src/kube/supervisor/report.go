package supervisor

import (
	"encoding/json"
	"io"
	"os"
	"syscall"

	"github.com/up2jj/wuko-marketplace/plugin-src/kube/execution"
)

// ReadReport reads at most limit+1; neither malformed reports nor outputs are
// truncated or regenerated. The complete schema-v2 document is retained.
func ReadReport(path string) (string, json.RawMessage) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing", nil
		}
		return "invalid", nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "invalid", nil
	}
	data, err := io.ReadAll(io.LimitReader(f, execution.ReportLimit+1))
	if err != nil {
		return "invalid", nil
	}
	if len(data) > execution.ReportLimit {
		return "oversized", nil
	}
	return ClassifyReport(data)
}
func ClassifyReport(data []byte) (string, json.RawMessage) {
	if len(data) > execution.ReportLimit {
		return "oversized", nil
	}
	var r struct {
		SchemaVersion int    `json:"schema_version"`
		InvocationID  string `json:"invocation_id"`
		Status        string `json:"status"`
		DurationMS    *int64 `json:"duration_ms"`
		DryRun        *bool  `json:"dry_run"`
		Stats         *struct {
			Steps *map[string]int `json:"steps"`
		} `json:"stats"`
	}
	if json.Unmarshal(data, &r) != nil || r.SchemaVersion != 2 || r.InvocationID == "" || r.DurationMS == nil || *r.DurationMS < 0 || r.DryRun == nil || *r.DryRun || r.Stats == nil || r.Stats.Steps == nil {
		return "invalid", nil
	}
	var full map[string]json.RawMessage
	if json.Unmarshal(data, &full) != nil {
		return "invalid", nil
	}
	var stats map[string]json.RawMessage
	if json.Unmarshal(full["stats"], &stats) != nil {
		return "invalid", nil
	}
	for _, key := range []string{"run_duration_ms", "attempts", "retries", "retry_wait_ms", "polls", "poll_wait_ms"} {
		var n int64
		if json.Unmarshal(stats[key], &n) != nil || n < 0 {
			return "invalid", nil
		}
	}
	for _, key := range []string{"total", "succeeded", "failed", "skipped", "canceled", "timed_out"} {
		n, ok := (*r.Stats.Steps)[key]
		if !ok || n < 0 {
			return "invalid", nil
		}
	}
	switch r.Status {
	case "succeeded", "failed", "canceled", "timed_out":
	default:
		return "invalid", nil
	}
	if outputs, ok := full["outputs"]; ok {
		var m map[string]any
		if json.Unmarshal(outputs, &m) != nil || m == nil {
			return "invalid", nil
		}
	}
	return "available", json.RawMessage(data)
}
