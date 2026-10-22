package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/up2jj/wuko-marketplace/plugin-src/kube/execution"
)

const validReport = `{"schema_version":2,"invocation_id":"inv-1","status":"succeeded","dry_run":false,"duration_ms":100,"stats":{"run_duration_ms":100,"attempts":1,"retries":0,"retry_wait_ms":0,"polls":0,"poll_wait_ms":0,"steps":{"total":1,"succeeded":1,"failed":0,"skipped":0,"canceled":0,"timed_out":0}},"outputs":{"result":"complete"}}`

func TestReportStates(t *testing.T) {
	cases := []struct{ body, state string }{{validReport, "available"}, {`{}`, "invalid"}, {`{"schema_version":1}`, "invalid"}, {strings.Repeat("x", execution.ReportLimit+1), "oversized"}}
	for _, tt := range cases {
		state, data := ClassifyReport([]byte(tt.body))
		if state != tt.state {
			t.Fatalf("%s want %s", state, tt.state)
		}
		if state == "available" && string(data) != tt.body {
			t.Fatal("report modified")
		}
		if state != "available" && data != nil {
			t.Fatal("partial report exposed")
		}
	}
	if state, _ := ReadReport(filepath.Join(t.TempDir(), "missing")); state != "missing" {
		t.Fatal(state)
	}
	p := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(p, []byte(strings.Repeat("x", execution.ReportLimit+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if state, _ := ReadReport(p); state != "oversized" {
		t.Fatal(state)
	}
}
