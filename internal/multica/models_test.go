package multica

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscoveryRequiresAgentScope(t *testing.T) {
	for _, agent := range []string{"", testID, "invalid"} {
		t.Run("agent="+agent, func(t *testing.T) {
			calls, reports := 0, 0
			requestID := "0123456789abcdef0123456789abcdef"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/daemon/heartbeat" {
					fmt.Fprintf(w, `{"pending_model_list":{"id":%q,"agent_id":%q}}`, requestID, agent)
					return
				}
				if r.URL.Path != "/api/daemon/runtimes/"+testID+"/models/"+requestID+"/result" {
					t.Error("unexpected endpoint")
				}
				var report struct {
					Status  string       `json:"status"`
					AgentID string       `json:"agent_id"`
					Models  []ModelEntry `json:"models"`
				}
				if json.NewDecoder(r.Body).Decode(&report) != nil {
					t.Fatal("bad report")
				}
				reports++
				if agent == testID {
					if report.Status != "completed" || report.AgentID != agent || len(report.Models) != 1 || report.Models[0].Thinking.DefaultLevel != "high" || report.Models[0].Context != 64000 {
						t.Error("catalog metadata dropped")
					}
				} else if report.Status != "failed" || len(report.Models) != 0 {
					t.Error("unscoped catalog disclosed")
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			c, err := New(server.URL, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			err = c.HeartbeatModels(context.Background(), testID, func(_ context.Context, id string) ([]ModelEntry, error) {
				calls++
				if id != testID {
					t.Error("wrong agent")
				}
				return []ModelEntry{{ID: "managed-inference/demo", Provider: "managed-inference", Context: 64000, Output: 4096, Thinking: &ModelThinking{DefaultLevel: "high", SupportedLevels: []ThinkingLevel{{Value: "high", Label: "High"}}}}}, nil
			})
			if err != nil || reports != 1 || (calls == 1) != (agent == testID) {
				t.Fatal("discovery scope failed", err)
			}
		})
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
