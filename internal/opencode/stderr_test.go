package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
)

func TestStderrTailIsBoundedFromALineStart(t *testing.T) {
	var tail stderrTail
	for i := range 2000 {
		line := fmt.Sprintf("line %04d %s\n", i, strings.Repeat("x", i%40))
		// Writes split lines like a pipe does.
		_, _ = tail.Write([]byte(line[:len(line)/2]))
		_, _ = tail.Write([]byte(line[len(line)/2:]))
	}
	got := tail.String()
	if len(got) > stderrLimit || !strings.HasPrefix(got, "line ") || !strings.HasSuffix(got, "line 1999 "+strings.Repeat("x", 1999%40)) {
		t.Fatalf("tail %d bytes: %q...%q", len(got), got[:20], got[len(got)-20:])
	}
	var short stderrTail
	_, _ = short.Write([]byte("panic: out of memory\n"))
	if short.String() != "panic: out of memory" {
		t.Fatalf("short tail: %q", short.String())
	}
	// An unbroken run past the bound may hold a cut credential: it is dropped.
	var unbroken stderrTail
	_, _ = unbroken.Write(bytes.Repeat([]byte("a"), 3*stderrLimit))
	if unbroken.String() != "" {
		t.Fatal("cut unbroken run kept")
	}
}

func TestStderrRedactsAttemptSecretsAndCredentialShapes(t *testing.T) {
	grant := "mat_relay_" + strings.Repeat("ab", 32)
	text := strings.Join([]string{
		"custom env leaked: agent-private-value-1",
		"request failed: Authorization: Bearer opaque-token-value",
		`config {"apiKey":"sk-live-0123456789abcdef","password": "hunter2hunter2"}`,
		"GH_TOKEN=ghp_0123456789abcdefghijABCDEFGHIJ retry",
		"fetch https://user:pass-word@git.example.invalid/repo.git",
		"relay " + grant + " jwt eyJhbGciOi.eyJzdWIiOi.c2lnbmF0dXJl",
		"Error: ENOMEM at step msg_01HZY session ses_abc",
	}, "\n")
	got := redact(text, []string{"agent-private-value-1", "short"})
	for _, secret := range []string{"agent-private-value-1", "opaque-token-value", "sk-live", "hunter2", "ghp_", "pass-word", grant, "eyJ"} {
		if strings.Contains(got, secret) {
			t.Fatalf("%q survived redaction:\n%s", secret, got)
		}
	}
	if !strings.Contains(got, "Error: ENOMEM at step msg_01HZY session ses_abc") || !strings.Contains(got, "git.example.invalid/repo.git") {
		t.Fatalf("diagnosis redacted:\n%s", got)
	}
}

func TestStderrSecretsCoverControllerCredentials(t *testing.T) {
	task := relayTask(t)
	if err := json.Unmarshal([]byte(`{"PRIVATE_KEY":"agent-private-value-2"}`), &task.Agent.CustomEnv); err != nil {
		t.Fatal(err)
	}
	r := &running{task: task, relayToken: "mat_relay_one", mcpToken: "mcp_relay_two", inferenceToken: "inf_relay_three", gitEnv: map[string]string{"GH_TOKEN": "msg_four", "GH_HOST": "forge.example.invalid"},
		tokens: map[string]identity.AccessToken{"docs": identity.NewAccessToken("jwt-five", time.Now())}, httpTokens: map[string]identity.AccessToken{"api": identity.NewAccessToken("jwt-six", time.Now())}}
	token, _ := task.TaskToken()
	secrets := strings.Join(r.secrets(), " ")
	for _, want := range []string{"mat_relay_one", "mcp_relay_two", "inf_relay_three", "msg_four", "jwt-five", "jwt-six", "agent-private-value-2", token} {
		if !strings.Contains(secrets, want) {
			t.Fatalf("secret %q not redacted", want)
		}
	}
	if strings.Contains(secrets, "forge.example.invalid") {
		t.Fatal("non-secret Git setting redacted")
	}
}

func TestFailedAttemptLogsRedactedStderrOnly(t *testing.T) {
	text := []map[string]any{{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "done"}}}
	for name, c := range map[string]struct {
		agent  *fakeAgent
		failed bool
	}{
		"stream end": {&fakeAgent{version: "2.2.5", updates: text, hangup: true}, true},
		"success":    {&fakeAgent{version: "2.2.5", updates: text, stop: "end_turn"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			workload := &stubWorkload{agent: c.agent, stderr: "FATAL heap limit\nleaked agent-private-value-3 Authorization: Bearer opaque-token-value\n"}
			a := Adapter{Server: "https://multica.example.invalid", Controller: "controller", Issuer: &stubIssuer{}, MCPRelay: NewMCPRelay(), Authority: &stubAuthority{}, Workloads: workload, Status: stubStatus{}, Log: slog.New(slog.NewJSONHandler(&logs, nil))}
			task := safeTask()
			if err := json.Unmarshal([]byte(`{"PRIVATE_KEY":"agent-private-value-3"}`), &task.Agent.CustomEnv); err != nil {
				t.Fatal(err)
			}
			run, err := a.Start(context.Background(), task)
			if err != nil {
				t.Fatal(err)
			}
			defer run.Remove(context.Background())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = run.Wait(ctx)
			if c.failed != (err != nil) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("attempt: %v", err)
			}
			logged := logs.String()
			if !c.failed {
				if strings.Contains(logged, "agent stderr") {
					t.Fatalf("stderr logged for a successful attempt: %s", logged)
				}
				return
			}
			if !strings.Contains(logged, `"msg":"agent stderr"`) || !strings.Contains(logged, `"task":"`+task.ID) || !strings.Contains(logged, "FATAL heap limit") ||
				strings.Contains(logged, "agent-private-value-3") || strings.Contains(logged, "opaque-token-value") {
				t.Fatalf("stderr log: %s", logged)
			}
			if strings.Contains(err.Error(), "heap") || strings.Contains(err.Error(), "FATAL") {
				t.Fatalf("stderr reached the failure: %v", err)
			}
		})
	}
}
