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
	// relayLogKeys bounds detailed keys; overflow has one fixed bucket per event class.
	relayLogKeys = 256
)

type relayEvent uint8

const (
	relayUpstreamResponse relayEvent = iota
	relayUpstreamError
	relayRequestRefused
	relayEventClasses
)

func (e relayEvent) level() slog.Level {
	if e == relayUpstreamResponse {
		return slog.LevelDebug
	}
	return slog.LevelWarn
}

func (e relayEvent) message() string {
	return [...]string{"relay upstream response", "relay upstream error", "relay request refused"}[e]
}

type refusalKey struct {
	errorType, route string
	status           int
	event            relayEvent
}

// refusalLog samples one relay's events, preserving their source and severity even
// in overflow. Paths redact secret-like segments; queries, headers and bodies are omitted.
type refusalLog struct {
	log        *slog.Logger
	relay      string
	mu         sync.Mutex
	suppressed map[refusalKey]int
	overflow   [relayEventClasses]int
}

func newRefusalLog(log *slog.Logger, relay string) *refusalLog {
	return &refusalLog{log: log, relay: relay, suppressed: map[refusalKey]int{}}
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

func (l *refusalLog) record(r *http.Request, attempt string, status int, errorType string, elapsed time.Duration) {
	event := relayUpstreamResponse
	switch {
	case errorType != "":
		event = relayRequestRefused
	case status == http.StatusTooManyRequests || status >= 500:
		event = relayUpstreamError
	}
	if !l.log.Enabled(r.Context(), event.level()) {
		return
	}
	path, route := redactPath(r.URL.Path)
	key := refusalKey{errorType, route, status, event}
	l.mu.Lock()
	n, seen := l.suppressed[key]
	switch {
	case seen:
		l.suppressed[key] = n + 1
	case len(l.suppressed) < relayLogKeys:
		l.suppressed[key] = 0
	default:
		route = "*"
		seen = l.overflow[event] > 0
		l.overflow[event]++
	}
	l.mu.Unlock()
	if seen {
		return
	}
	attrs := []any{"relay", l.relay, "method", r.Method, "path", path, "route", route}
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
	l.log.Log(r.Context(), event.level(), event.message(), attrs...)
}

// flush summarizes the window's suppressed events and opens the next window.
func (l *refusalLog) flush() {
	l.mu.Lock()
	suppressed := l.suppressed
	overflow := l.overflow
	l.suppressed = map[refusalKey]int{}
	l.overflow = [relayEventClasses]int{}
	l.mu.Unlock()
	for event, n := range overflow {
		if n > 1 {
			suppressed[refusalKey{route: "*", event: relayEvent(event)}] = n - 1
		}
	}
	for key, n := range suppressed {
		if n == 0 {
			continue
		}
		attrs := []any{"relay", l.relay, "route", key.route, "event", key.event.message()}
		if key.status != 0 {
			attrs = append(attrs, "status", key.status)
		}
		if key.errorType != "" {
			attrs = append(attrs, "error_type", key.errorType)
		}
		l.log.Log(context.Background(), key.event.level(), "relay events suppressed", append(attrs, "count", n)...)
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
