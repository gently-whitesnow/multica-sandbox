package execution

import "fmt"

// AgentFailure exposes only a numeric HTTP status, never native response bodies or credentials.
type AgentFailure struct{ Status int }

func (e *AgentFailure) Error() string {
	if e.Status >= 400 && e.Status <= 599 {
		return fmt.Sprintf("Agent inference request failed (HTTP %d)", e.Status)
	}
	return "Agent reported execution failure; output withheld"
}
