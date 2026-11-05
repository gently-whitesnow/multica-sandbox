package service

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// relayLogWindow samples relay refusal logs: per relay, outcome and route the first
	// event of a window is logged and later ones are counted into one summary when it
	// closes, so an agent retrying in a loop cannot flood the log. Metrics count them all.
	relayLogWindow = time.Minute
	// relayLogKeys bounds the routes sampled per window; further ones share one key.
	relayLogKeys = 256
)

type refusalKey struct {
	relay, errorType, route string
	status                  int
}

// refusalLog logs relay refusals and upstream errors at warning level: method, a path
// with secret-like segments redacted and the attempt's task and workspace, never the
// query, headers or bodies.
type refusalLog struct {
	log        *slog.Logger
	mu         sync.Mutex
	suppressed map[refusalKey]int
}

func newRefusalLog(log *slog.Logger) *refusalLog {
	return &refusalLog{log: log, suppressed: map[refusalKey]int{}}
}

// run closes a window every interval until ctx ends, then closes the last one.
func (l *refusalLog) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.flush()
			return
		case <-ticker.C:
			l.flush()
		}
	}
}

func (l *refusalLog) record(relay string, r *http.Request, attempt string, status int, errorType string, elapsed time.Duration) {
	path, route := redactPath(r.URL.Path)
	key := refusalKey{relay, errorType, route, status}
	l.mu.Lock()
	if _, seen := l.suppressed[key]; !seen && len(l.suppressed) >= relayLogKeys {
		key = refusalKey{relay: relay, route: "*"}
	}
	n, seen := l.suppressed[key]
	if seen {
		l.suppressed[key] = n + 1
	} else {
		l.suppressed[key] = 0
	}
	l.mu.Unlock()
	if seen {
		return
	}
	attrs := []any{"relay", relay, "method", r.Method, "path", path, "route", route}
	if status != 0 {
		attrs = append(attrs, "status", status)
	}
	if errorType != "" {
		attrs = append(attrs, "error_type", errorType)
	}
	// Attempt keys are workspace:runtime:task:dispatched_at.
	if parts := strings.SplitN(attempt, ":", 4); len(parts) == 4 {
		attrs = append(attrs, "workspace", parts[0], "task", parts[2])
	}
	attrs = append(attrs, "seconds", elapsed.Seconds())
	msg := "relay upstream error"
	if errorType != "" {
		msg = "relay request refused"
	}
	l.log.Warn(msg, attrs...)
}

// flush summarizes the window's suppressed events and opens the next window.
func (l *refusalLog) flush() {
	l.mu.Lock()
	suppressed := l.suppressed
	l.suppressed = map[refusalKey]int{}
	l.mu.Unlock()
	for key, n := range suppressed {
		if n == 0 {
			continue
		}
		attrs := []any{"relay", key.relay, "route", key.route}
		if key.status != 0 {
			attrs = append(attrs, "status", key.status)
		}
		if key.errorType != "" {
			attrs = append(attrs, "error_type", key.errorType)
		}
		l.log.Warn("relay events suppressed", append(attrs, "count", n)...)
	}
}

var (
	// credentialSegment matches relay and forge credential prefixes and JWTs.
	credentialSegment = regexp.MustCompile(`^(?:mat_relay_|inf_relay_|mcp_relay_|msg_|glpat-|gldt-|glrt-|gh[opsur]_|github_pat_|sk-|xox[a-z]-|eyJ)`)
	// opaqueSegment matches long token alphabets; secret() also requires letters and digits.
	opaqueSegment = regexp.MustCompile(`^[A-Za-z0-9_~+=.-]{32,}$`)
	uuidSegment   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// idSegment matches numbers, hashes and issue keys such as ABC-12.
	idSegment = regexp.MustCompile(`^(?:[0-9]+|[0-9a-f]{4,}|[A-Za-z][A-Za-z0-9]*-[0-9]+)$`)
)

// redactPath returns the path with secret-like segments replaced and its route, the
// path with identifiers replaced too, which keys log sampling.
func redactPath(p string) (string, string) {
	segments := strings.Split(p, "/")
	route := make([]string, len(segments))
	for i, segment := range segments {
		route[i] = segment
		switch {
		case uuidSegment.MatchString(segment):
			route[i] = "{id}"
		case secret(segment):
			segments[i], route[i] = "{redacted}", "{redacted}"
		case idSegment.MatchString(segment) && strings.ContainsAny(segment, "0123456789"):
			route[i] = "{id}"
		}
	}
	return strings.Join(segments, "/"), strings.Join(route, "/")
}

func secret(segment string) bool {
	return credentialSegment.MatchString(segment) || opaqueSegment.MatchString(segment) &&
		strings.ContainsAny(segment, "0123456789") && strings.ContainsFunc(segment, func(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' })
}
