package multica

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const supplementJournalLimit = 1 << 20

type supplementStore struct {
	dir        string
	mu         sync.Mutex
	negotiated map[string]string
	flight     map[string]bool
}

// supplementRecord contains guidance and references, never claim/service credentials.
type supplementRecord struct {
	Version    int             `json:"version"`
	Server     string          `json:"server"`
	Workspace  string          `json:"workspace_id"`
	Runtime    string          `json:"runtime_id"`
	Task       string          `json:"task_id"`
	Issue      string          `json:"issue_id"`
	Dispatched string          `json:"dispatched_at"`
	Session    string          `json:"session_id"`
	Phase      string          `json:"phase"`
	Current    *taskSupplement `json:"current,omitempty"`
	Done       []string        `json:"done,omitempty"`
}

// UseSupplements must run before the client is used, only for a conformant adapter.
// It enables an offer, not support: /start must explicitly negotiate the capability.
func (c *Client) UseSupplements(dir string) error {
	if c.supplements != nil {
		return fmt.Errorf("supplements already configured")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	err = errors.Join(file.Sync(), file.Close(), os.Remove(file.Name()), syncSupplementDir(dir))
	if err != nil {
		return err
	}
	c.supplements = &supplementStore{dir: dir, negotiated: map[string]string{}, flight: map[string]bool{}}
	return nil
}

func (s *supplementStore) negotiate(t Task, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.negotiated, t.AttemptKey())
	if enabled {
		s.negotiated[t.AttemptKey()] = t.ID
	}
}

func (c *Client) SupplementsNegotiated(t Task) bool {
	s := c.supplements
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.negotiated[t.AttemptKey()] == t.ID && t.ValidAttempt() && validID(t.IssueID)
}

func (s *supplementStore) begin(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flight[key] {
		return false
	}
	s.flight[key] = true
	return true
}

func (s *supplementStore) end(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.flight, key)
}

func (r supplementRecord) key() string {
	return r.Workspace + ":" + r.Runtime + ":" + r.Task + ":" + r.Dispatched
}

func (s *supplementStore) load(c *Client, t Task, session string) (supplementRecord, error) {
	record := supplementRecord{Version: 1, Server: c.base, Workspace: t.WorkspaceID, Runtime: t.RuntimeID, Task: t.ID, Issue: t.IssueID, Dispatched: t.DispatchedAt, Session: session, Phase: "idle"}
	data, err := os.ReadFile(s.path(record.key()))
	if os.IsNotExist(err) {
		return record, nil
	}
	if err != nil {
		return record, err
	}
	var saved supplementRecord
	if len(data) > supplementJournalLimit || json.Unmarshal(data, &saved) != nil || !saved.valid(c.base) || saved.key() != record.key() || saved.Issue != t.IssueID || saved.Session != session {
		return record, fmt.Errorf("invalid or mismatched supplement journal")
	}
	return saved, nil
}

func (r supplementRecord) valid(server string) bool {
	_, err := time.Parse(time.RFC3339Nano, r.Dispatched)
	if r.Version != 1 || r.Server != server || !validID(r.Workspace) || !validID(r.Runtime) || !validID(r.Task) || !validID(r.Issue) || err != nil || !supplementSession.MatchString(r.Session) || len(r.Done) > 4096 {
		return false
	}
	for _, id := range r.Done {
		if !validID(id) {
			return false
		}
	}
	switch r.Phase {
	case "idle", "claiming":
		return r.Current == nil
	case "claimed", "consumed":
		return r.Current != nil && r.Current.valid()
	}
	return false
}

func (s *supplementStore) save(r supplementRecord) error {
	data, err := json.Marshal(r)
	if err != nil || len(data) > supplementJournalLimit {
		return fmt.Errorf("supplement journal exceeds its bound")
	}
	file, err := os.CreateTemp(s.dir, ".supplement-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), s.path(r.key())); err != nil {
		return err
	}
	return syncSupplementDir(s.dir)
}

func (s *supplementStore) path(key string) string { return filepath.Join(s.dir, fileName(key)) }

func (s *supplementStore) remove(key string) error {
	if err := os.Remove(s.path(key)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncSupplementDir(s.dir)
}

func syncSupplementDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
