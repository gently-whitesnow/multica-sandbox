package multica

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/metrics"
)

func TestRouteKeepsLabelsBounded(t *testing.T) {
	for path, want := range map[string]string{
		"/api/daemon/tasks/" + testID + "/start":                           "/api/daemon/tasks/{id}/start",
		"/api/daemon/runtimes/" + testID + "/local-skills/import/x/result": "/api/daemon/runtimes/{id}/local-skills/import/{id}/result",
		"/api/workspaces/" + testID + "/env?secret=1":                      "/api/workspaces/{id}/env",
		"/api/daemon/heartbeat":                                            "/api/daemon/heartbeat",
	} {
		if got := Route(path); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
}

// Requests are recorded by route and status; resends after the first are retries.
func TestInstrumentRecordsRequestsAndRetries(t *testing.T) {
	sleep := retrySleep
	retrySleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { retrySleep = sleep })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)
	c, err := New(server.URL, "controller-secret")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := metrics.New()
	c.Instrument(m, nil)
	if err = c.CancelAck(t.Context(), testID); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, metrics.Path, nil))
	text := rec.Body.String()
	labels := `method="POST",path="/api/daemon/tasks/{id}/cancel-ack"`
	for _, line := range []string{
		`http_client_request_duration_seconds_count{` + labels + `,status_code="503",target_service_name="multica"} 1`,
		`http_client_request_duration_seconds_count{` + labels + `,status_code="200",target_service_name="multica"} 1`,
		`http_client_request_retries_total{` + labels + `,target_service_name="multica"} 1`,
	} {
		if !strings.Contains(text, line) {
			t.Errorf("exposition lacks %s:\n%s", line, text)
		}
	}
	if strings.Contains(text, testID) || strings.Contains(text, "controller-secret") {
		t.Fatal("identifier or credential in metrics")
	}
}
