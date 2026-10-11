package multica

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRuntimeScopedDiscoveryReportsUpstreamShape(t *testing.T) {
	for _, mode := range []string{"completed", "outage", "empty", "idle"} {
		t.Run(mode, func(t *testing.T) {
			calls, reports := 0, 0
			requestID := "0123456789abcdef0123456789abcdef"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/daemon/heartbeat" {
					if mode != "idle" {
						// Upstream sends only the request ID; discovery is runtime-scoped.
						fmt.Fprintf(w, `{"pending_model_list":{"id":%q}}`, requestID)
					} else {
						_, _ = w.Write([]byte(`{}`))
					}
					return
				}
				if r.URL.Path != "/api/daemon/runtimes/"+testID+"/models/"+requestID+"/result" {
					t.Error("unexpected endpoint")
				}
				var report map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&report) != nil {
					t.Fatal("bad report")
				}
				reports++
				if mode == "completed" {
					var models []map[string]any
					if string(report["status"]) != `"completed"` || string(report["supported"]) != "true" || json.Unmarshal(report["models"], &models) != nil || len(models) != 1 {
						t.Error("completed report differs from upstream shape", report)
					}
					thinking, _ := models[0]["thinking"].(map[string]any)
					if models[0]["id"] != "managed-inference/demo" || models[0]["default"] != true || thinking["default_level"] != "high" {
						t.Error("model metadata dropped", models)
					}
				} else if string(report["status"]) != `"failed"` || report["models"] != nil || report["error"] == nil {
					t.Error("failure hidden or catalog disclosed", report)
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			c, err := New(server.URL, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			err = c.HeartbeatModels(context.Background(), testID, func(context.Context) ([]ModelEntry, error) {
				calls++
				switch mode {
				case "outage":
					return nil, fmt.Errorf("private detail")
				case "empty":
					return nil, nil
				}
				return []ModelEntry{{ID: "managed-inference/demo", Provider: "managed-inference", Default: true, Thinking: &ModelThinking{DefaultLevel: "high", SupportedLevels: []ThinkingLevel{{Value: "high", Label: "High"}}}}}, nil
			})
			want := 1
			if mode == "idle" {
				want = 0
			}
			if err != nil || reports != want || calls != want {
				t.Fatal("discovery failed", err, reports, calls)
			}
		})
	}
}
func TestUnsupportedRuntimeRequestsAreAnswered(t *testing.T) {
	reports := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/heartbeat" {
			_, _ = w.Write([]byte(`{"pending_update":{"id":"u1","target_version":"v9"},"pending_model_list":{"id":"m1"},"pending_local_skills":{"id":"s1"},"pending_local_skill_import":{"id":"i1","skill_key":"k"},"pending_local_skill_imports":[{"id":"i1","skill_key":"k"},{"id":"i/2","skill_key":"k"}]}`))
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("bad report")
		}
		reports[r.URL.EscapedPath()] = body
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	c, err := New(server.URL, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	// Without an inference catalog the model list fails instead of hanging the picker.
	if err = c.Heartbeat(context.Background(), testID); err != nil {
		t.Fatal(err)
	}
	base := "/api/daemon/runtimes/" + testID
	failed := func(path, text string) {
		if body := reports[base+path]; body["status"] != "failed" || !strings.Contains(fmt.Sprint(body["error"]), text) {
			t.Errorf("%s: %v", path, body)
		}
	}
	failed("/models/m1/result", "unavailable")
	failed("/update/u1/result", "not supported by sandbox runtime")
	failed("/local-skills/import/i1/result", "not supported by sandbox runtime")
	failed("/local-skills/import/i%2F2/result", "not supported by sandbox runtime")
	if body := reports[base+"/local-skills/s1/result"]; body["status"] != "completed" || body["supported"] != false || body["mcp_supported"] != false {
		t.Errorf("local skills: %v", body)
	}
	if len(reports) != 5 {
		t.Fatalf("duplicate or missing reports: %v", reports)
	}
}

func TestFailurePayloadLeavesClassificationToMultica(t *testing.T) {
	for _, reason := range []string{"", "timeout"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["error"] != "Agent inference request failed (HTTP 403)" || body["session_id"] != "ses_fixture" {
				t.Error("unsafe or incomplete failure payload", body)
			}
			if got, ok := body["failure_reason"]; ok != (reason != "") || (ok && got != reason) {
				t.Error("failure reason not delegated", body)
			}
			_, _ = w.Write([]byte(`{}`))
		}))
		c, err := New(server.URL, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if err = c.AgentFail(context.Background(), testID, "Agent inference request failed (HTTP 403)", reason, execution.Result{SessionID: "ses_fixture"}); err != nil {
			t.Fatal(err)
		}
		server.Close()
	}
}
