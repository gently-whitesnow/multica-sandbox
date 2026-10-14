package service

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/controller"
)

// finished is an attempt's summary line: identity, outcome, timing and resources, with
// only the safe failure text Multica also receives. Prompts and output stay out.
func finished(f controller.Finish) string {
	kind := "issue"
	switch {
	case f.Task.ChatSessionID != "":
		kind = "chat"
	case f.Task.IssueID == "":
		kind = "other"
	}
	fields := []string{"finished task=" + f.Task.ID, "workspace=" + f.Task.WorkspaceID, "agent=" + f.Task.AgentID, "kind=" + kind}
	message, reason := "", ""
	outcome := "completed"
	if f.Failure != nil {
		outcome = "failed"
		message, reason = failure(f.Failure)
		if reason == "" {
			reason = "agent"
		}
	}
	fields = append(fields, "outcome="+outcome)
	if reason != "" {
		fields = append(fields, "reason="+reason)
	}
	if !f.Started.IsZero() {
		fields = append(fields, "prepare="+seconds(f.Started.Sub(f.Claimed)), "run="+seconds(f.Ended.Sub(f.Started)))
	}
	fields = append(fields, "total="+seconds(f.Ended.Sub(f.Claimed)))
	t := f.Result.Telemetry
	if t.Model != "" {
		fields = append(fields, "model="+t.Model)
	}
	if t.Effort != "" {
		fields = append(fields, "effort="+t.Effort)
	}
	if t.InputTokens > 0 || t.OutputTokens > 0 {
		fields = append(fields, "tokens_in="+strconv.FormatInt(t.InputTokens, 10), "tokens_out="+strconv.FormatInt(t.OutputTokens, 10), "tokens_cached="+strconv.FormatInt(t.CachedTokens, 10))
	}
	if t.PeakMemory > 0 {
		fields = append(fields, fmt.Sprintf("peak_memory=%dMiB", t.PeakMemory>>20))
	}
	if f.Result.SessionID != "" {
		fields = append(fields, "session="+f.Result.SessionID)
	}
	if f.Deferred {
		fields = append(fields, "report=deferred")
	}
	if message != "" {
		fields = append(fields, "error="+strconv.Quote(message))
	}
	return strings.Join(fields, " ")
}

func seconds(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s" }
