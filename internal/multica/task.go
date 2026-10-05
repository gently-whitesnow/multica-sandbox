package multica

import (
	"encoding/json"
	"time"
)

type Agent struct {
	Model         string          `json:"model"`
	ThinkingLevel string          `json:"thinking_level"`
	ID            string          `json:"id"`
	Instructions  string          `json:"instructions"`
	MCPConfig     json.RawMessage `json:"mcp_config"`
}

func (t Task) AttemptKey() string {
	return t.WorkspaceID + ":" + t.RuntimeID + ":" + t.ID + ":" + t.DispatchedAt
}

func (t Task) ValidAgent() bool {
	return validID(t.WorkspaceID) && validID(t.AgentID) && t.Agent != nil && t.Agent.ID == t.AgentID
}

func (t Task) ValidAttempt() bool {
	_, err := time.Parse(time.RFC3339Nano, t.DispatchedAt)
	return t.ValidAgent() && validID(t.ID) && validID(t.RuntimeID) && t.StartClaimSupported && err == nil
}
