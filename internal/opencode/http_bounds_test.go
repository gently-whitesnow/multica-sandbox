package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"io"
	"strings"
	"testing"
	"time"
)

func TestHTTPAPIStreamsLargeValidBodiesAndResponses(t *testing.T) {
	r, _, w := nativeHTTPFixture()
	input := strings.Repeat("\x01", 65536)
	response := strings.Repeat("x", 256<<10)
	w.captureHTTP = func(_ context.Context, args []string) ([]byte, error) {
		if len(args[7]) <= 128<<10 {
			t.Fatal("fixture did not exceed Linux per-argument bound")
		}
		var body struct{ Text string }
		if json.Unmarshal([]byte(args[7]), &body) != nil || body.Text != input {
			t.Fatal("body did not survive stdin streaming")
		}
		return json.Marshal(map[string]any{"data": map[string]string{"text": response}})
	}
	var got struct{ Data struct{ Text string } }
	if err := r.httpAPI(context.Background(), "POST", "/api/session/ses_root/prompt", map[string]string{"text": input}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Data.Text != response {
		t.Fatal("native response was silently truncated")
	}
	for _, args := range w.commands {
		for _, arg := range args {
			if len(arg) > 8192 || strings.Contains(arg, input) || strings.Contains(arg, r.nativePassword) {
				t.Fatal("payload or credential entered exec argv")
			}
		}
	}
}

func TestHTTPConfigPreservesNativeHeaderMCPWithoutOAuth(t *testing.T) {
	r, _, _ := nativeHTTPFixture()
	r.adapter.Steering = &multica.Client{}
	r.adapter.MCPRelayURL = "http://mcp-relay:8095"
	r.mcpToken = "mcp_relay_fixture"
	r.connections = map[string]Remote{"fixture": {Type: "remote", URL: "https://unused.example.invalid/mcp"}}
	configured, err := r.configureNativeHTTP([]byte(`{"permission":{"*":"deny","fixture_*":"allow"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCP struct {
			Servers map[string]struct {
				URL     string
				OAuth   *bool
				Headers map[string]string
			}
		}
	}
	if json.Unmarshal(configured, &got) != nil {
		t.Fatal("invalid config")
	}
	server := got.MCP.Servers["fixture"]
	if server.OAuth == nil || *server.OAuth || server.URL != "http://mcp-relay:8095/fixture" || server.Headers["Authorization"] != "Bearer mcp_relay_fixture" {
		t.Fatalf("config=%s", configured)
	}
}

func TestHTTPReadyReadIsContextBounded(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := nativeHTTPReady(ctx, reader)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("readiness was not bounded: %v", err)
	}
}

func TestHTTPReadyRejectsOversizedAnnouncement(t *testing.T) {
	announcement := `{"url":"http://127.0.0.1:4096","extra":"` + strings.Repeat("x", 128<<10) + `"}`
	if _, err := nativeHTTPReady(context.Background(), strings.NewReader(announcement)); err == nil {
		t.Fatal("unbounded readiness announcement accepted")
	}
}

func TestHTTPEventUnknownSchemaAndAggregateFrameBound(t *testing.T) {
	r, h, _ := nativeHTTPFixture()
	input := "data: {\"type\":\"inventory.changed\",\"data\":{\"id\":{\"provider\":\"fixture\"}}}\n\n" +
		"data: {\"type\":\"session.instructions.updated\",\"data\":{\"sessionID\":\"ses_root\",\"delta\":{\"added\":[]}}}\n\n" +
		"data: {\"id\":\"evt_text\",\"type\":\"session.text.delta\",\"data\":{\"sessionID\":\"ses_root\",\"delta\":\"output\"}}\n\n"
	if err := readHTTPEvents(context.Background(), strings.NewReader(input), h.consume); !errors.Is(err, streamCut) {
		t.Fatal(err)
	}
	output, _, _ := r.events.result()
	if output != "output" {
		t.Fatal("unhandled inventory payload poisoned the native stream")
	}
	oversized := strings.Repeat("data: "+strings.Repeat("x", 512<<10)+"\n", 10)
	if err := readHTTPEvents(context.Background(), strings.NewReader(oversized), func(httpEvent) {}); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("aggregate multiline frame was not bounded: %v", err)
	}
}

func TestHTTPQuiescenceIsConservativeAndLateEventsInvalidateIt(t *testing.T) {
	r, h, w := nativeHTTPFixture()
	h.busy, h.terminal = false, true
	w.captureHTTP = func(_ context.Context, args []string) ([]byte, error) {
		if strings.HasSuffix(args[5], "/active") {
			return []byte(`{"data":{}}`), nil
		}
		return []byte(`{"data":[]}`), nil
	}
	h.markClean(context.Background())
	if !r.nativeClean {
		t.Fatal("quiescent owner was not reusable")
	}
	h.consume(nativeEvent(t, "foreign-start", "session.execution.started", "ses_foreign", nil))
	if r.nativeClean {
		t.Fatal("foreign execution did not invalidate reuse")
	}
	h.markClean(context.Background())
	if r.nativeClean {
		t.Fatal("snapshot inspection erased evidence of an unknown execution")
	}
}

func TestHTTPQuiescenceNeverTrustsMissingInspection(t *testing.T) {
	r, h, w := nativeHTTPFixture()
	h.busy, h.terminal = false, true
	w.captureHTTP = func(context.Context, []string) ([]byte, error) { return []byte(`{}`), nil }
	h.markClean(context.Background())
	if r.nativeClean {
		t.Fatal("missing inspection was treated as empty")
	}
}
