package metrics

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// TestExportedContract pins the Prometheus names and exact label sets of ADR 0025.
func TestExportedContract(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	m.Attempt("issue", "success", "", 0, 40*time.Second)
	m.Attempt("chat", "failed", "inference", 429, time.Second)
	m.Prepare("issue", "resumed", 3*time.Second)
	m.Active(1)
	m.Event("cancelled")
	m.Event("rejected (secret native detail)")
	m.Relay("inference", 422, "", time.Second)
	m.Relay("multica", 0, "policy-denied", time.Millisecond)
	m.Client("multica", http.MethodPost, "/api/daemon/tasks/{id}/start", 200, "", 10*time.Millisecond)
	m.Client("multica", "QUERY", "/api/daemon/heartbeat", 0, "request-timeout", time.Second)
	m.Retry("multica", http.MethodPost, "/api/daemon/tasks/{id}/complete")
	m.Pool(PoolReading{Pressure: 0.42, Usage: 3 << 30, Limit: 4 << 30})
	m.Paused(true)

	families := scrape(t, m)
	want := map[string][][]string{
		"sandbox_attempt_duration_seconds":         {{"kind", "status"}, {"kind", "reason", "status", "status_code"}},
		"sandbox_attempt_prepare_duration_seconds": {{"continuity", "kind"}},
		"sandbox_attempt_active":                   {{}},
		"sandbox_attempt_events_total":             {{"event"}},
		"sandbox_relay_request_duration_seconds":   {{"relay", "status_code"}, {"error_type", "relay"}},
		"http_client_request_duration_seconds":     {{"method", "path", "status_code", "target_service_name"}, {"error_type", "method", "path", "target_service_name"}},
		"http_client_request_retries_total":        {{"method", "path", "target_service_name"}},
		"sandbox_pool_memory_pressure_ratio":       {{}},
		"sandbox_pool_memory_usage_bytes":          {{}},
		"sandbox_pool_memory_limit_bytes":          {{}},
		"sandbox_claims_paused":                    {{}},
	}
	for name, labelSets := range want {
		family, ok := families[name]
		if !ok {
			t.Errorf("missing %s", name)
			continue
		}
		var got [][]string
		for _, sample := range family.Metric {
			var names []string
			for _, label := range sample.Label {
				names = append(names, label.GetName())
			}
			slices.Sort(names)
			if !slices.ContainsFunc(got, func(g []string) bool { return slices.Equal(g, names) }) {
				got = append(got, names)
			}
		}
		for _, set := range labelSets {
			if !slices.ContainsFunc(got, func(g []string) bool { return slices.Equal(g, set) || (len(g) == 0 && len(set) == 0) }) {
				t.Errorf("%s: label set %v not in %v", name, set, got)
			}
		}
		if len(got) != len(labelSets) {
			t.Errorf("%s: label sets %v, want %v", name, got, labelSets)
		}
	}
	for name := range families {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected family %s", name)
		}
	}
	text := export(t, m)
	for _, leak := range []string{"otel_scope", "target_info", "secret", "QUERY"} {
		if strings.Contains(text, leak) {
			t.Errorf("exposition contains %q", leak)
		}
	}
	for _, line := range []string{
		`sandbox_attempt_events_total{event="other"} 1`,
		`sandbox_attempt_duration_seconds_bucket{kind="chat",reason="inference",status="failed",status_code="429",le="1"} 1`,
		`http_client_request_duration_seconds_count{error_type="request-timeout",method="other",path="/api/daemon/heartbeat",target_service_name="multica"} 1`,
		`sandbox_pool_memory_pressure_ratio 0.42`,
		`sandbox_claims_paused 1`,
	} {
		if !strings.Contains(text, line) {
			t.Errorf("exposition lacks %s", line)
		}
	}
}

func TestDefaultBuckets(t *testing.T) {
	m, _ := New()
	m.Attempt("issue", "success", "", 0, time.Second)
	m.Client("multica", http.MethodGet, "/api/daemon/workspaces", 200, "", time.Millisecond)
	families := scrape(t, m)
	for name, want := range map[string][]float64{"sandbox_attempt_duration_seconds": attemptBuckets, "http_client_request_duration_seconds": clientBuckets} {
		var got []float64
		for _, b := range families[name].Metric[0].Histogram.Bucket {
			if !math.IsInf(b.GetUpperBound(), 1) {
				got = append(got, b.GetUpperBound())
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s buckets %v, want %v", name, got, want)
		}
	}
}

func TestNilRecordsNothing(t *testing.T) {
	var m *Metrics
	m.Attempt("issue", "success", "", 0, time.Second)
	m.Prepare("issue", "fresh", time.Second)
	m.Active(1)
	m.Event("gone")
	m.Relay("git", 200, "", time.Second)
	m.Client("multica", http.MethodGet, "/", 200, "", time.Second)
	m.Retry("multica", http.MethodGet, "/")
	m.Pool(PoolReading{})
	m.Paused(true)
}

func TestServeExposesOnlyThePath(t *testing.T) {
	m, _ := New()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	if err = m.Serve(t.Context(), addr); err != nil {
		t.Fatal(err)
	}
	for path, status := range map[string]int{Path: http.StatusOK, "/metrics": http.StatusNotFound, "/": http.StatusNotFound} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != status {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, status)
		}
	}
}

func TestErrorType(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for err, want := range map[error]string{
		context.DeadlineExceeded:                             "request-timeout",
		&net.DNSError{Err: "no such host"}:                   "dns-resolve",
		&net.OpError{Op: "dial", Err: errors.New("refused")}: "connection-failed",
		errors.New("unexpected"):                             "other",
	} {
		if got := ErrorType(context.Background(), err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
	if got := ErrorType(canceled, errors.New("transport")); got != "request-canceled" {
		t.Errorf("cancelled caller: %s", got)
	}
}

func export(t *testing.T, m *Metrics) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, Path, nil))
	return w.Body.String()
}

func scrape(t *testing.T, m *Metrics) map[string]*dto.MetricFamily {
	t.Helper()
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(export(t, m)))
	if err != nil {
		t.Fatal(err)
	}
	return families
}
