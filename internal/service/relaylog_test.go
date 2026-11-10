package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

// Identical HTTP codes can be upstream answers or local errors. Summaries must
// preserve that distinction and level, while metrics count every observation.
func TestRelayEventLevels(t *testing.T) {
	for _, test := range []struct {
		name, level, message, errorType string
		status                          int
		err                             error
	}{
		{"upstream-401", "DEBUG", "relay upstream response", "", 401, nil},
		{"upstream-403", "DEBUG", "relay upstream response", "", 403, nil},
		{"upstream-404", "DEBUG", "relay upstream response", "", 404, nil},
		{"upstream-422", "DEBUG", "relay upstream response", "", 422, nil},
		{"upstream-429", "WARN", "relay upstream error", "", 429, nil},
		{"upstream-500", "WARN", "relay upstream error", "", 500, nil},
		{"upstream-503", "WARN", "relay upstream error", "", 503, nil},
		{"policy", "WARN", "relay request refused", "policy-denied", 403, relay.ErrPolicy},
		{"credential", "WARN", "relay request refused", "invalid-credential", 401, relay.ErrCredential},
		{"busy", "WARN", "relay request refused", "too-many-requests", 0, relay.ErrBusy},
		{"upstream-credential", "WARN", "relay request refused", "upstream-credential", 0, relay.ErrUpstreamCredential},
		{"transport", "WARN", "relay request refused", "request-timeout", 0, context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, log, m := observed(t, true)
			refusals := newRefusalLog(c.logger(), "forge")
			record := c.relayRecord("forge", refusals)
			req := httptest.NewRequest(http.MethodPost, "/v1/glpat-fixture?key=query-private", strings.NewReader("body-private"))
			req.Header.Set("Authorization", "Bearer header-private")
			for range 4 {
				record(req, "ws:runtime:task:dispatch", test.status, test.err, time.Millisecond)
			}
			refusals.flush()
			events := lines(t, log)
			if len(events) != 2 {
				t.Fatalf("events: %v", events)
			}
			if e := events[0]; e["level"] != test.level || e["msg"] != test.message || e["path"] != "/v1/{redacted}" || e["task"] != "task" {
				t.Fatalf("detail: %v", e)
			}
			if e := events[1]; e["level"] != test.level || e["event"] != test.message || e["count"] != 3.0 {
				t.Fatalf("summary: %v", e)
			}
			label := fmt.Sprintf(`status_code="%d"`, test.status)
			if test.errorType != "" {
				label = fmt.Sprintf(`error_type="%s"`, test.errorType)
			}
			want := `sandbox_relay_request_duration_seconds_count{relay="forge",` + label + `} 4`
			if test.errorType != "" {
				want = `sandbox_relay_request_duration_seconds_count{` + label + `,relay="forge"} 4`
			}
			if text := exposition(m); !strings.Contains(text, want) {
				t.Fatalf("missing count %s", want)
			}
			for _, secret := range []string{"glpat-fixture", "query-private", "header-private", "body-private"} {
				if strings.Contains(log.String(), secret) {
					t.Errorf("logged %s", secret)
				}
			}
		})
	}
}

func TestRelayDebugDisabledKeepsMetricsAndWarnings(t *testing.T) {
	c, log, m := observed(t, false)
	refusals := newRefusalLog(c.logger(), "mcp")
	record := c.relayRecord("mcp", refusals)
	for i := range relayLogKeys + 10 {
		req := httptest.NewRequest(http.MethodGet, "/v1/route"+strconv.Itoa(i)+"x", nil)
		for range 2 {
			record(req, "", 401, nil, time.Millisecond)
			record(req, "", 403, nil, time.Millisecond)
		}
	}
	if len(refusals.suppressed) != 0 || refusals.overflow != [relayEventClasses]int{} {
		t.Fatal("debug-disabled traffic consumed sampling state")
	}
	refusals.flush()
	if log.Len() != 0 || len(refusals.suppressed) != 0 {
		t.Fatalf("debug-disabled traffic logged or retained: %s", log)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/route0x", nil)
	for range 2 {
		record(req, "", 0, relay.ErrPolicy, time.Millisecond)
	}
	refusals.flush()
	events := lines(t, log)
	if len(events) != 2 {
		t.Fatalf("warning events: %v", events)
	}
	for _, e := range events {
		if e["level"] != "WARN" || e["error_type"] != "policy-denied" {
			t.Fatalf("warning: %v", e)
		}
	}
	for _, code := range []int{401, 403} {
		want := fmt.Sprintf(`sandbox_relay_request_duration_seconds_count{relay="mcp",status_code="%d"} %d`, code, 2*(relayLogKeys+10))
		if !strings.Contains(exposition(m), want) {
			t.Errorf("missing count %s", want)
		}
	}
}

// Overflow keeps fixed buckets for ordinary responses, degraded upstreams and
// local errors. A debug flood cannot absorb the first warning or promote its count.
func TestRelayOverflowSeparatesEventClasses(t *testing.T) {
	c, log, _ := observed(t, true)
	refusals := newRefusalLog(c.logger(), "forge")
	record := c.relayRecord("forge", refusals)
	for i := range relayLogKeys {
		record(httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/route%dx", i), nil), "", 403, nil, 0)
	}
	log.Reset()
	req := httptest.NewRequest(http.MethodGet, "/v1/overflow", nil)
	for range 5 {
		record(req, "", 401, nil, 0)
		record(req, "", 403, nil, 0)
		record(req, "", 429, nil, 0)
		record(req, "", 503, nil, 0)
		record(req, "", 0, relay.ErrPolicy, 0)
		record(req, "", 0, relay.ErrCredential, 0)
	}
	if len(refusals.suppressed) != relayLogKeys {
		t.Fatalf("detailed keys exceeded bound: %d", len(refusals.suppressed))
	}
	refusals.flush()
	refusals.flush()
	events := lines(t, log)
	if len(events) != 6 {
		t.Fatalf("overflow events: %v", events)
	}
	want := map[string]string{"relay upstream response": "DEBUG", "relay upstream error": "WARN", "relay request refused": "WARN"}
	for _, e := range events {
		event := e["msg"].(string)
		if event == "relay events suppressed" {
			event = e["event"].(string)
			if e["count"] != 9.0 || e["status"] != nil || e["error_type"] != nil {
				t.Errorf("mixed overflow summary: %v", e)
			}
		}
		if e["level"] != want[event] || e["route"] != "*" || e["relay"] != "forge" {
			t.Errorf("overflow classification: %v", e)
		}
	}
	log.Reset()
	record(req, "", 403, nil, 0)
	if e := lines(t, log); len(e) != 1 || e[0]["route"] != "/v1/overflow" {
		t.Fatalf("next window: %v", e)
	}
}

func TestRelaySamplingConcurrentFlush(t *testing.T) {
	c, log, m := observed(t, true)
	refusals := newRefusalLog(c.logger(), "forge")
	record := c.relayRecord("forge", refusals)
	var workers sync.WaitGroup
	for worker := range 6 {
		workers.Go(func() {
			for i := range 100 {
				req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/route%dx", i%3), nil)
				record(req, "", 403+100*(worker%2), nil, 0)
				if i%7 == 0 {
					refusals.flush()
				}
			}
		})
	}
	workers.Wait()
	refusals.flush()
	counts := map[string]int{}
	for _, e := range lines(t, log) {
		n := 1
		if e["msg"] == "relay events suppressed" {
			n = int(e["count"].(float64))
		}
		counts[e["level"].(string)] += n
	}
	if counts["DEBUG"] != 300 || counts["WARN"] != 300 {
		t.Fatalf("lost or promoted events: %v", counts)
	}
	for _, status := range []int{403, 503} {
		want := fmt.Sprintf(`sandbox_relay_request_duration_seconds_count{relay="forge",status_code="%d"} 300`, status)
		if !strings.Contains(exposition(m), want) {
			t.Errorf("missing count %s", want)
		}
	}
}

// Exercise the real relay callback, including body withholding, so logging cannot
// silently change protocol answers or turn upstream 401/403 into local refusals.
func TestRelayHTTPObservation(t *testing.T) {
	for _, debug := range []bool{false, true} {
		for _, withhold := range []bool{false, true} {
			t.Run(fmt.Sprintf("debug=%t/withhold=%t", debug, withhold), func(t *testing.T) {
				c, log, m := observed(t, debug)
				refusals := newRefusalLog(c.logger(), "forge")
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					code, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/v1/status/"))
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(code)
					_, _ = w.Write([]byte("upstream-private"))
				}))
				t.Cleanup(upstream.Close)
				proxy, token := relayLogFixture(t, c, refusals, upstream.URL, withhold)
				for _, status := range []int{200, 401, 403, 404, 422, 429, 503} {
					for range 3 {
						req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/status/%d?key=query-private", status), nil)
						req.Header.Set("Authorization", "Bearer "+token)
						w := httptest.NewRecorder()
						proxy.ServeHTTP(w, req)
						body := "upstream-private"
						if withhold && status >= 400 {
							body = fmt.Sprintf(`{"error":{"message":"upstream refused the request","type":"upstream_error","code":"%d"}}`, status)
						} else if w.Header().Get("Retry-After") != "7" {
							t.Fatal("upstream header changed")
						}
						if w.Code != status || w.Body.String() != body {
							t.Fatalf("HTTP %d: %d %s", status, w.Code, w.Body)
						}
					}
					want := fmt.Sprintf(`sandbox_relay_request_duration_seconds_count{relay="forge",status_code="%d"} 3`, status)
					if !strings.Contains(exposition(m), want) {
						t.Errorf("missing count %s", want)
					}
				}
				refusals.flush()
				for _, e := range lines(t, log) {
					if e["error_type"] != nil || (!debug && e["level"] != "WARN") {
						t.Errorf("upstream classified as local refusal: %v", e)
					}
				}
				if strings.Contains(log.String(), "private") || strings.Contains(log.String(), token) {
					t.Fatal("upstream body, query or credential logged")
				}
			})
		}
	}
}

func relayLogFixture(t *testing.T, c Config, refusals *refusalLog, origin string, withhold bool) (http.Handler, string) {
	t.Helper()
	grants := relay.NewGrants("fixture_")
	token, err := grants.Issue("ws:runtime:task:dispatch", relay.Upstream{Origin: origin, Credential: "fixture-upstream"})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := relay.New(grants, relay.Policy{Allow: func(path string) bool { return strings.HasPrefix(path, "/v1/") }, Limit: 1024,
		Withhold: withhold, Record: c.relayRecord("forge", refusals)})
	if err != nil {
		t.Fatal(err)
	}
	return proxy, token
}

func TestRelayLocalRefusalAndTransportObservation(t *testing.T) {
	c, log, m := observed(t, false)
	refusals := newRefusalLog(c.logger(), "forge")
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("refused request reached upstream")
	}))
	t.Cleanup(upstream.Close)
	proxy, token := relayLogFixture(t, c, refusals, upstream.URL, false)
	upstream.Close()
	for _, test := range []struct {
		path, token, errorType string
		status                 int
	}{
		{"/blocked", token, "policy-denied", 403},
		{"/v1/test", "fixture_unknown", "invalid-credential", 401},
		{"/v1/test", token, "connection-failed", 502},
	} {
		req := httptest.NewRequest(http.MethodGet, test.path, nil)
		req.Header.Set("Authorization", "Bearer "+test.token)
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)
		if w.Code != test.status {
			t.Errorf("%s: HTTP %d, want %d", test.errorType, w.Code, test.status)
		}
		want := fmt.Sprintf(`sandbox_relay_request_duration_seconds_count{error_type="%s",relay="forge"} 1`, test.errorType)
		if !strings.Contains(exposition(m), want) {
			t.Errorf("missing count %s", want)
		}
	}
	events := lines(t, log)
	if len(events) != 3 {
		t.Fatalf("events: %v", events)
	}
	for _, e := range events {
		if e["level"] != "WARN" || e["msg"] != "relay request refused" || e["status"] != nil {
			t.Errorf("local/transport event: %v", e)
		}
	}
}
