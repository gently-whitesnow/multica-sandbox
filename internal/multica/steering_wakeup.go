package multica

import "strings"

// SupplementUpdates supplies coalesced hints only for this negotiated attempt.
// Callers must keep a bounded polling fallback: hints carry no authority or work.
func (c *Client) SupplementUpdates(t Task) <-chan struct{} {
	s := c.supplements
	if s == nil || !t.ValidAttempt() || !validID(t.IssueID) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := t.AttemptKey()
	if s.negotiated[key] != t.ID {
		return nil
	}
	if s.wake[key] == nil {
		s.wake[key] = make(chan struct{}, 1)
	}
	return s.wake[key]
}

// WakeSupplement accepts only the owning runtime/exact task's advisory signal.
// Retired channels are detached, not closed; worker contexts own their lifetime.
func (c *Client) WakeSupplement(runtime, task string) {
	s := c.supplements
	if s == nil || !validID(runtime) || !validID(task) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, id := range s.negotiated {
		parts := strings.SplitN(key, ":", 4)
		if id != task || len(parts) != 4 || parts[1] != runtime || parts[2] != task {
			continue
		}
		select {
		case s.wake[key] <- struct{}{}:
		default:
		}
	}
}
