package multica

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Secret holds a claim credential. It never formats, marshals or logs its value.
type Secret struct{ value string }

func (s *Secret) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &s.value); err != nil {
		return errors.New("invalid claim credential")
	}
	return nil
}
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`null`), nil }
func (Secret) String() string               { return "[redacted]" }
func (Secret) GoString() string             { return "[redacted]" }
func (Secret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, "[redacted]") }

// TaskToken mirrors upstream taskScopedAuthToken: only a task-scoped mat_ token, no fallback.
func (t Task) TaskToken() (string, error) {
	token := strings.TrimSpace(t.AuthToken.value)
	if !strings.HasPrefix(token, "mat_") || len(token) > 256 || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("claim lacks a task-scoped mat_ token")
	}
	return token, nil
}
