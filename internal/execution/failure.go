package execution

import (
	"fmt"
	"time"
)

// AgentFailure exposes a numeric HTTP status or fixed adapter text, never native
// response bodies or credentials. Multica classifies the text.
type AgentFailure struct {
	Status  int
	Message string
}

func (e *AgentFailure) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Status >= 400 && e.Status <= 599 {
		return fmt.Sprintf("Agent inference request failed (HTTP %d)", e.Status)
	}
	return "Agent reported execution failure; output withheld"
}

// TimeoutError is the controller's own deadline, reported as Multica's retryable timeout.
type TimeoutError struct{ After time.Duration }

func (e *TimeoutError) Error() string { return fmt.Sprintf("timed out after %s", e.After) }

// IdleError is the upstream idle watchdog: the agent emitted nothing for the window.
type IdleError struct{ After time.Duration }

func (e *IdleError) Error() string {
	return fmt.Sprintf("agent produced no new messages for %s; force-stopped by idle watchdog", e.After)
}

// OOMError is an agent failure after the kernel killed attempt processes for memory.
// Limit is the attempt ceiling or, with Pool, the pool's memory.max (negative: unknown).
type OOMError struct {
	Killed, Peak, Limit int64
	Pool                bool
}

func (e *OOMError) Error() string {
	limit := "the attempt memory limit"
	if e.Pool {
		limit = "the shared attempt memory pool"
	}
	if e.Limit > 0 {
		limit += fmt.Sprintf(" (%s)", size(e.Limit))
	}
	peak := ""
	if e.Peak > 0 {
		peak = fmt.Sprintf(", peak usage %s", size(e.Peak))
	}
	killed := "processes"
	if e.Killed > 0 {
		killed = fmt.Sprintf("%d process(es)", e.Killed)
	}
	return fmt.Sprintf("Out of memory: the kernel killed %s in the attempt after it reached %s%s. "+
		"Not retried automatically: reduce the task's memory use or raise the limit.", killed, limit, peak)
}

func size(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%d MiB", n>>20)
}
