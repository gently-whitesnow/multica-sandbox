package execution

import (
	"fmt"
	"time"
)

// AgentFailure exposes only a numeric HTTP status, never native response bodies or credentials.
type AgentFailure struct{ Status int }

func (e *AgentFailure) Error() string {
	if e.Status >= 400 && e.Status <= 599 {
		return fmt.Sprintf("Agent inference request failed (HTTP %d)", e.Status)
	}
	return "Agent reported execution failure; output withheld"
}

// TimeoutError is the controller's own deadline, reported as Multica's retryable timeout.
type TimeoutError struct{ After time.Duration }

func (e *TimeoutError) Error() string { return fmt.Sprintf("timed out after %s", e.After) }
