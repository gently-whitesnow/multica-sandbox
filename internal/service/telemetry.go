package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// attemptEvent logs a lifecycle event; a rejection carries its safe cause in parentheses.
func (c Config) attemptEvent(event, id string) {
	name, cause, _ := strings.Cut(event, " (")
	attrs := []any{"task", id}
	if cause != "" {
		attrs = append(attrs, "error", strings.TrimSuffix(cause, ")"))
	}
	c.logger().Info("attempt "+name, attrs...)
	if name != "claimed" && name != "started" && name != "reported" && name != "rejected" {
		c.metrics.Event(name)
	}
}

// finished logs and records an attempt's summary: identity, outcome, timing and
// resources, with only the safe failure text Multica also receives. Prompts and
// output stay out.
func (c Config) finished(f controller.Finish) {
	kind := attemptKind(f.Task)
	status, reason, code := outcome(f.Failure)
	attrs := []slog.Attr{slog.String("task", f.Task.ID), slog.String("workspace", f.Task.WorkspaceID), slog.String("agent", f.Task.AgentID),
		slog.String("kind", kind), slog.String("status", status)}
	if reason != "" {
		attrs = append(attrs, slog.String("reason", reason))
	}
	if code != 0 {
		attrs = append(attrs, slog.Int("status_code", code))
	}
	message := ""
	if f.Failure != nil {
		var failureReason string
		message, failureReason = failure(f.Failure)
		if failureReason != "" {
			attrs = append(attrs, slog.String("failure_reason", failureReason))
		}
	}
	t := f.Result.Telemetry
	if !f.Started.IsZero() {
		prepare := f.Started.Sub(f.Claimed)
		attrs = append(attrs, slog.Float64("prepare_seconds", prepare.Seconds()), slog.Float64("run_seconds", f.Ended.Sub(f.Started).Seconds()))
		if t.Continuity != "" {
			attrs = append(attrs, slog.String("continuity", t.Continuity))
			c.metrics.Prepare(kind, t.Continuity, prepare)
		}
	}
	total := f.Ended.Sub(f.Claimed)
	attrs = append(attrs, slog.Float64("total_seconds", total.Seconds()))
	c.metrics.Attempt(kind, status, reason, code, total)
	if t.Model != "" {
		attrs = append(attrs, slog.String("model", t.Model))
	}
	if t.Effort != "" {
		attrs = append(attrs, slog.String("effort", t.Effort))
	}
	if t.InputTokens > 0 || t.OutputTokens > 0 {
		attrs = append(attrs, slog.Int64("tokens_in", t.InputTokens), slog.Int64("tokens_out", t.OutputTokens), slog.Int64("tokens_cached", t.CachedTokens))
	}
	if t.PeakMemory > 0 {
		attrs = append(attrs, slog.Int64("peak_memory_bytes", t.PeakMemory))
	}
	if f.Result.SessionID != "" {
		attrs = append(attrs, slog.String("session", f.Result.SessionID))
	}
	if f.Deferred {
		attrs = append(attrs, slog.Bool("report_deferred", true))
	}
	if message != "" {
		attrs = append(attrs, slog.String("error", message))
	}
	level := slog.LevelInfo
	if f.Failure != nil {
		level = slog.LevelWarn
	}
	c.logger().LogAttrs(context.Background(), level, "attempt finished", attrs...)
}

func attemptKind(t multica.Task) string {
	switch {
	case t.ChatSessionID != "":
		return "chat"
	case t.IssueID == "":
		return "other"
	}
	return "issue"
}

// outcome classifies an attempt for metrics: status success, failed or timeout, the
// reason of a failure and the inference HTTP status of an inference failure.
func outcome(cause error) (status, reason string, code int) {
	var timeout *execution.TimeoutError
	var idle *execution.IdleError
	var rejected *execution.RejectedError
	var oom *execution.OOMError
	var agent *execution.AgentFailure
	switch {
	case cause == nil:
		return "success", "", 0
	case errors.As(cause, &timeout):
		return "timeout", "", 0
	case errors.As(cause, &idle):
		return "failed", "idle", 0
	case errors.As(cause, &rejected):
		return "failed", "rejected", 0
	case errors.As(cause, &oom):
		return "failed", "oom", 0
	case errors.As(cause, &agent) && agent.Status >= 400 && agent.Status <= 599:
		return "failed", "inference", agent.Status
	case errors.As(cause, &agent):
		return "failed", "agent", 0
	}
	return "failed", "internal", 0
}
