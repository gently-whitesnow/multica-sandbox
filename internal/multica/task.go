package multica

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

type Agent struct {
	Model         string          `json:"model"`
	ThinkingLevel string          `json:"thinking_level"`
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Instructions  string          `json:"instructions"`
	MCPConfig     json.RawMessage `json:"mcp_config"`
	// CustomEnv reaches attempts only for names the deployment allows (opencode.env_allow).
	CustomEnv SecretEnv `json:"custom_env"`
	// Skills are inline: the controller does not advertise skill-bundles-v1.
	Skills []Skill `json:"skills"`
}

// Skill is an agent skill with its supporting files, as upstream AgentSkillData.
type Skill struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Content     string      `json:"content"`
	Files       []SkillFile `json:"files"`
}
type SkillFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// SecretEnv holds an agent's custom_env; like Secret, its values never format or marshal.
type SecretEnv struct{ values map[string]string }

// UnmarshalJSON keeps a string map and, like the upstream server, ignores any other shape.
func (e *SecretEnv) UnmarshalJSON(data []byte) error {
	e.values = nil
	_ = json.Unmarshal(data, &e.values)
	return nil
}
func (SecretEnv) MarshalJSON() ([]byte, error) { return []byte(`null`), nil }
func (SecretEnv) String() string               { return "[redacted]" }
func (SecretEnv) GoString() string             { return "[redacted]" }
func (SecretEnv) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, "[redacted]") }

// Allowed returns the values of the allowed names the agent sets.
func (e SecretEnv) Allowed(names []string) map[string]string {
	out := map[string]string{}
	for _, name := range names {
		if value, ok := e.values[name]; ok {
			out[name] = value
		}
	}
	return out
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

// ValidID reports a canonical Multica UUID reference.
func ValidID(id string) bool { return validID(id) }
