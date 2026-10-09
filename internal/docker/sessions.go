package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
)

// sessionLabel binds a retained workdir volume to its workspace, agent and issue or
// chat session (ADR 0015, 0017).
const sessionLabel = "io.multica-sandbox.session"

// sessionBusyWait is upstream's wait for the previous run of an (issue, agent) to
// release its workdir before a fresh one is used instead.
var sessionBusyWait = 15 * time.Second

// Sessions retains one workdir volume per (controller, workspace, agent, conversation) with
// one writer at a time. Dir records last use; idle volumes past TTL, and the least
// recently used beyond Max, are removed. Closed, when set, reports when done or
// cancelled issues of a workspace were last updated; Sweep then also removes their
// idle volumes after Grace, as upstream GC drops their workdirs.
type Sessions struct {
	Dir        string
	TTL, Grace time.Duration
	Interval   time.Duration
	Max        int
	Closed     func(ctx context.Context, workspace string, issues []string) map[string]time.Time
	mu         sync.Mutex
	busy       map[string]bool
}

func sessionName(owner string, w execution.Workdir) (string, string) {
	label := w.Workspace + "/" + w.Agent + "/" + w.Conversation()
	sum := sha256.Sum256([]byte(owner + ":session:" + label))
	return "multica-sandbox-session-" + hex.EncodeToString(sum[:16]), label
}

// acquire marks a volume busy, waiting up to sessionBusyWait for its current holder.
func (s *Sessions) acquire(ctx context.Context, name string) bool {
	deadline := time.Now().Add(sessionBusyWait)
	for {
		s.mu.Lock()
		if s.busy == nil {
			s.busy = map[string]bool{}
		}
		if !s.busy[name] {
			s.busy[name] = true
			s.mu.Unlock()
			return true
		}
		s.mu.Unlock()
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// release records the last use and frees the volume for the next run.
func (s *Sessions) release(name string) error {
	err := s.touch(name)
	s.mu.Lock()
	delete(s.busy, name)
	s.mu.Unlock()
	return err
}

func (s *Sessions) touch(name string) error {
	path := filepath.Join(s.Dir, name)
	if err := os.WriteFile(path, nil, 0600); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(path, now, now)
}

// Collect removes idle session volumes of owner past the TTL, then the least
// recently used beyond Max. Volumes without a record count as used now.
func (s *Sessions) Collect(ctx context.Context, owner string) error {
	return s.collect(ctx, owner, false)
}

// Sweep collects, including closed issues, now and every Interval until ctx ends.
func (s *Sessions) Sweep(ctx context.Context, owner string, report func(error)) {
	sweep(ctx, s.Interval, func(ctx context.Context) error { return s.collect(ctx, owner, true) }, report)
}

func sweep(ctx context.Context, every time.Duration, collect func(context.Context) error, report func(error)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if err := collect(ctx); err != nil && ctx.Err() == nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Sessions) collect(ctx context.Context, owner string, byIssue bool) error {
	data, err := command(ctx, "volume", "ls", "--filter", "label="+ownerLabel+"="+owner, "--filter", "label="+sessionLabel, "--format", `{{.Name}} {{.Label "`+sessionLabel+`"}}`)
	if err != nil {
		return err
	}
	labels := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if name, label, _ := strings.Cut(line, " "); name != "" {
			labels[name] = label
		}
	}
	checked, closed := time.Now(), map[string]time.Time{}
	if byIssue && s.Closed != nil {
		closed = s.closed(ctx, labels)
	}
	doomed, err := s.plan(labels, closed, checked)
	for _, name := range doomed {
		_, rmErr := command(ctx, "volume", "rm", "-f", name)
		if rmErr == nil {
			rmErr = os.Remove(filepath.Join(s.Dir, name))
		}
		err = errors.Join(err, rmErr)
	}
	s.mu.Lock()
	for _, name := range doomed {
		delete(s.busy, name)
	}
	s.mu.Unlock()
	return err
}

// closed asks each workspace only about its own session issues; keys are workspace/issue.
func (s *Sessions) closed(ctx context.Context, labels map[string]string) map[string]time.Time {
	issues := map[string][]string{}
	for _, label := range labels {
		if workspace, issue, ok := sessionIssue(label); ok {
			issues[workspace] = append(issues[workspace], issue)
		}
	}
	closed := map[string]time.Time{}
	for workspace, ids := range issues {
		for issue, updated := range s.Closed(ctx, workspace, ids) {
			closed[workspace+"/"+issue] = updated
		}
	}
	return closed
}

func sessionIssue(label string) (string, string, bool) {
	parts := strings.Split(label, "/")
	return parts[0], parts[len(parts)-1], len(parts) == 3
}

// plan reserves and returns the idle volumes to remove: past the TTL, of issues
// closed Grace before checked and unused since, then the least recently used
// beyond Max. Busy volumes stay.
func (s *Sessions) plan(labels map[string]string, closed map[string]time.Time, checked time.Time) ([]string, error) {
	type idle struct {
		name string
		used time.Time
		gone bool
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy == nil {
		s.busy = map[string]bool{}
	}
	candidates, kept := []idle{}, len(labels)
	for name, label := range labels {
		if s.busy[name] {
			continue
		}
		used := now
		if info, err := os.Stat(filepath.Join(s.Dir, name)); err == nil {
			used = info.ModTime()
		} else if err := s.touch(name); err != nil {
			return nil, err
		}
		workspace, issue, _ := sessionIssue(label)
		updated, ok := closed[workspace+"/"+issue]
		gone := now.Sub(used) > s.TTL || ok && checked.Sub(updated) > s.Grace && used.Before(checked)
		if gone {
			kept--
		}
		candidates = append(candidates, idle{name, used, gone})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].used.Before(candidates[j].used) })
	doomed := []string{}
	for _, c := range candidates {
		if !c.gone && kept > s.Max {
			c.gone = true
			kept--
		}
		if c.gone {
			// Reserved while collecting, so no run starts on a volume being removed.
			s.busy[c.name] = true
			doomed = append(doomed, c.name)
		}
	}
	return doomed, nil
}

// sessionVolume returns whether the labelled session volume existed, creating and
// initializing it otherwise. A same-named volume with other labels is refused.
func (b *Projected) sessionVolume(ctx context.Context, name, label string) (bool, error) {
	data, err := command(ctx, "volume", "ls", "-q", "--filter", "name="+name)
	if err != nil {
		return false, err
	}
	if !slices.Contains(strings.Fields(string(data)), name) {
		return false, b.workdir(ctx, name, "--label", sessionLabel+"="+label)
	}
	data, err = command(ctx, "volume", "inspect", name)
	var volumes []struct{ Labels map[string]string }
	if err != nil || json.Unmarshal(data, &volumes) != nil || len(volumes) != 1 ||
		volumes[0].Labels[ownerLabel] != b.Owner || volumes[0].Labels[sessionLabel] != label {
		return false, errors.Join(err, fmt.Errorf("session volume %s is not bound to this conversation", name))
	}
	return true, nil
}
