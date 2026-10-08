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

// sessionLabel binds a retained workdir volume to its workspace, agent and issue (ADR 0015).
const sessionLabel = "io.multica-sandbox.session"

// sessionBusyWait is upstream's wait for the previous run of an (issue, agent) to
// release its workdir before a fresh one is used instead.
var sessionBusyWait = 15 * time.Second

// Sessions retains one workdir volume per (controller, workspace, agent, issue) with
// one writer at a time. Dir records last use; idle volumes past TTL, and the least
// recently used beyond Max, are removed.
type Sessions struct {
	Dir  string
	TTL  time.Duration
	Max  int
	mu   sync.Mutex
	busy map[string]bool
}

func sessionName(owner string, w execution.Workdir) (string, string) {
	label := w.Workspace + "/" + w.Agent + "/" + w.Issue
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
	data, err := command(ctx, "volume", "ls", "-q", "--filter", "label="+ownerLabel+"="+owner, "--filter", "label="+sessionLabel)
	if err != nil {
		return err
	}
	names := strings.Fields(string(data))
	type idle struct {
		name string
		used time.Time
	}
	candidates := []idle{}
	now := time.Now()
	s.mu.Lock()
	if s.busy == nil {
		s.busy = map[string]bool{}
	}
	for _, name := range names {
		if s.busy[name] {
			continue
		}
		used := now
		if info, err := os.Stat(filepath.Join(s.Dir, name)); err == nil {
			used = info.ModTime()
		} else if err := s.touch(name); err != nil {
			s.mu.Unlock()
			return err
		}
		// Reserved while collecting, so no run starts on a volume being removed.
		s.busy[name] = true
		candidates = append(candidates, idle{name, used})
	}
	s.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].used.Before(candidates[j].used) })
	excess := len(names) - s.Max
	for i, c := range candidates {
		if now.Sub(c.used) > s.TTL || i < excess {
			_, rmErr := command(ctx, "volume", "rm", "-f", c.name)
			if rmErr == nil {
				rmErr = os.Remove(filepath.Join(s.Dir, c.name))
			}
			err = errors.Join(err, rmErr)
		}
	}
	s.mu.Lock()
	for _, c := range candidates {
		delete(s.busy, c.name)
	}
	s.mu.Unlock()
	return err
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
