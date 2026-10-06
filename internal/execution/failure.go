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
