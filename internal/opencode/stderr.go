package opencode

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
)

// stderrLimit bounds the agent stderr kept for diagnosis, like the native daemon's tail.
const stderrLimit = 8192

// stderrTail keeps the last stderrLimit bytes of the agent's stderr from a line start.
// It never fails its writer.
type stderrTail struct {
	mu  sync.Mutex
	buf []byte
	cut bool
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n > stderrLimit {
		p, t.cut = p[n-stderrLimit:], true
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*stderrLimit {
		t.buf, t.cut = t.buf[:copy(t.buf, t.buf[len(t.buf)-stderrLimit:])], true
	}
	return n, nil
}

// String drops a line cut by the bound; an unbroken run is dropped whole, so no
// credential fragment survives the cut.
func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, cut := t.buf, t.cut
	if len(b) > stderrLimit {
		b, cut = b[len(b)-stderrLimit:], true
	}
	if cut {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			i = bytes.IndexAny(b, " \t")
		}
		if i < 0 {
			return ""
		}
		b = b[i+1:]
	}
	return strings.ToValidUTF8(strings.TrimRight(string(b), "\r\n"), "�")
}

const redacted = "[redacted]"

// Credential shapes redacted from agent stderr beside the attempt's known secrets.
var credentialPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?i)([\w.-]*(?:token|secret|passw(?:or)?d|key|credential|authorization|cookie)[\w.-]*["']?\s*[:=]\s*["']?)(?:(?:bearer|basic)\s+)?[^\s"',;&]+`), "${1}" + redacted},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[\w.~+/=-]{8,}`), "${1} " + redacted},
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}" + redacted + "@"},
	// Controller relay grants, JWTs and common provider key formats.
	{regexp.MustCompile(`\b[a-z]+_(?:relay_)?[0-9a-f]{64}\b`), redacted},
	{regexp.MustCompile(`\beyJ[\w-]{4,}\.[\w-]{4,}\.[\w-]+`), redacted},
	{regexp.MustCompile(`\b(?:sk|pk|rk)-[\w-]{16,}|\bgh[pousr]_\w{20,}|\bglpat-[\w-]{20,}|\bxox[abpr]-[\w-]{10,}|\bAKIA[0-9A-Z]{16}\b`), redacted},
}

// redact removes the attempt's secrets, longest first, and credential-shaped text.
func redact(text string, secrets []string) string {
	secrets = slices.DeleteFunc(slices.Clone(secrets), func(s string) bool { return len(s) < 8 })
	slices.SortFunc(secrets, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, redacted)
	}
	for _, p := range credentialPatterns {
		text = p.pattern.ReplaceAllString(text, p.replacement)
	}
	return text
}

// secrets are the values the controller placed in or kept for this attempt: relay
// credentials, the task token, issued tokens and the agent's custom env.
func (r *running) secrets() []string {
	secrets := []string{r.relayToken, r.mcpToken, r.inferenceToken}
	if r.nativePassword != "" {
		secrets = append(secrets, r.nativePassword, base64.StdEncoding.EncodeToString([]byte("opencode:"+r.nativePassword)))
	}
	if token, err := r.task.TaskToken(); err == nil {
		secrets = append(secrets, token)
	}
	if r.task.Agent != nil {
		secrets = slices.AppendSeq(secrets, maps.Values(r.task.Agent.CustomEnv.Values()))
	}
	for name, value := range r.gitEnv {
		if strings.HasSuffix(name, "_TOKEN") {
			secrets = append(secrets, value)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, token := range r.tokens {
		secrets = append(secrets, token.Bearer())
	}
	for _, token := range r.httpTokens {
		secrets = append(secrets, token.Bearer())
	}
	return secrets
}

// streamAgent runs the agent, keeping its stderr tail where the workload offers it.
func (r *running) streamAgent(ctx context.Context, command []string, converse func(io.Writer, io.Reader) error) error {
	return r.stream(ctx, command, &r.stderr, converse)
}

// stream runs command, writing its stderr to tail where the workload offers it.
func (r *running) stream(ctx context.Context, command []string, tail *stderrTail, converse func(io.Writer, io.Reader) error) error {
	if w, ok := r.workload.(execution.StderrStreamer); ok {
		return w.StreamStderr(ctx, command, r.environment(), tail, converse)
	}
	return r.workload.Stream(ctx, command, r.environment(), converse)
}

// logStderr records the redacted stderr tail of a failed attempt in the controller log
// only; it never reaches the transcript or the failure message.
func (r *running) logStderr(err error) {
	if err == nil || errors.Is(err, context.Canceled) || r.adapter.Log == nil {
		return
	}
	tail := r.stderr.String()
	if tail == "" {
		return
	}
	r.adapter.Log.Warn("agent stderr", "task", r.task.ID, "attempt", r.task.AttemptKey(), "error", err.Error(), "stderr", redact(tail, r.secrets()))
}
