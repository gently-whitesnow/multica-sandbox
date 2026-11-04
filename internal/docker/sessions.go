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
	"github.com/gently-whitesnow/multica-sandbox/internal/pool"
)

// sessionLabel binds a retained workdir volume to its workspace, agent and issue or
// chat session (ADR 0015, 0017).
const sessionLabel = "io.multica-sandbox.session"

// sessionBusyWait is upstream's wait for the previous run of an (issue, agent) to
// release its workdir before a fresh one is used instead.
var sessionBusyWait = 15 * time.Second

// gcTimeout bounds one helper prune or measurement of a session volume.
var gcTimeout = 10 * time.Minute

// pressureEvery is how often Sweep reads Disk.
var pressureEvery = time.Minute

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
	// Helper prunes and measures idle volumes in Sweep. ArtifactAge, when positive, prunes
	// rebuildable outputs of volumes idle that long; Budget, when positive, bounds the
	// measured bytes of all volumes by removing the least recently used idle ones.
	Helper      string
	ArtifactAge time.Duration
	Budget      int64
	// Disk, when set, is read every pressureEvery: below the claim pause watermark idle
	// volumes are pruned, then removed, least recently used first, until it recovers.
	Disk func() (pool.Disk, error)
	// Reclaimed receives each collection that pruned or removed volumes.
	Reclaimed func(Reclaim)
	mu        sync.Mutex
	busy      map[string]bool
}

// Reclaim summarizes one collection. Trigger is lifetime (TTL, closed issue, Max), age
// (ArtifactAge), budget or pressure. Artifacts is what the helper deleted; Freed is the
// gain in available disk bytes, known only with Disk and approximate under concurrent writes.
type Reclaim struct {
	Trigger          string
	Removed, Pruned  int
	Artifacts, Freed int64
}

// gcRecord is a volume's last helper result, current until the volume is used again.
type gcRecord struct {
	Pruned bool  `json:"pruned"`
	Size   int64 `json:"size"`
}

type idleVolume struct {
	name string
	used time.Time
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

// Sweep collects, including closed issues, artifacts and the budget, now and every
// Interval until ctx ends, and relieves disk pressure in between.
func (s *Sessions) Sweep(ctx context.Context, owner string, report func(error)) {
	var relieve func(context.Context) error
	if s.Disk != nil {
		relieve = func(ctx context.Context) error { return s.relieve(ctx, owner) }
	}
	sweep(ctx, s.Interval, pressureEvery, func(ctx context.Context) error { return s.collect(ctx, owner, true) }, relieve, report)
}

func sweep(ctx context.Context, every, pressure time.Duration, collect, relieve func(context.Context) error, report func(error)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	var pressured <-chan time.Time
	if relieve != nil {
		p := time.NewTicker(pressure)
		defer p.Stop()
		pressured = p.C
	}
	run := func(f func(context.Context) error) {
		if err := f(ctx); err != nil && ctx.Err() == nil {
			report(err)
		}
	}
	run(collect)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run(collect)
		case <-pressured:
			run(relieve)
		}
	}
}

func (s *Sessions) list(ctx context.Context, owner string) (map[string]string, error) {
	data, err := command(ctx, "volume", "ls", "--filter", "label="+ownerLabel+"="+owner, "--filter", "label="+sessionLabel, "--format", `{{.Name}} {{.Label "`+sessionLabel+`"}}`)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if name, label, _ := strings.Cut(line, " "); name != "" {
			labels[name] = label
		}
	}
	return labels, nil
}

func (s *Sessions) collect(ctx context.Context, owner string, byIssue bool) error {
	labels, err := s.list(ctx, owner)
	if err != nil {
		return err
	}
	checked, closed := time.Now(), map[string]time.Time{}
	if byIssue && s.Closed != nil {
		closed = s.closed(ctx, labels)
	}
	before := s.available()
	doomed, err := s.plan(labels, closed, checked)
	lifetime := Reclaim{Trigger: "lifetime"}
	for _, name := range doomed {
		rmErr := s.remove(ctx, name)
		if rmErr == nil {
			lifetime.Removed++
		}
		err = errors.Join(err, rmErr)
		delete(labels, name)
	}
	s.mu.Lock()
	for _, name := range doomed {
		delete(s.busy, name)
	}
	s.mu.Unlock()
	s.reclaimed(lifetime, before)
	if !byIssue || err != nil {
		return err
	}
	if s.ArtifactAge > 0 {
		before, age := s.available(), Reclaim{Trigger: "age"}
		idle, err := s.idle(labels)
		if err == nil {
			err = s.prune(ctx, owner, idle, time.Now().Add(-s.ArtifactAge), func() bool { return false }, &age)
		}
		s.reclaimed(age, before)
		if err != nil {
			return err
		}
	}
	if s.Budget > 0 {
		return s.budget(ctx, owner, labels)
	}
	return nil
}

// relieve, below the claim pause watermark, prunes and then removes idle volumes, least
// recently used first, until the disk reaches the resume watermark. Busy volumes stay.
func (s *Sessions) relieve(ctx context.Context, owner string) error {
	d, err := s.disk()
	if err != nil || !pool.DiskLow(d) {
		return err
	}
	labels, err := s.list(ctx, owner)
	if err != nil {
		return err
	}
	idle, err := s.idle(labels)
	if err != nil {
		return err
	}
	recovered := func() bool {
		d, err := s.disk()
		return ctx.Err() != nil || err == nil && pool.DiskRecovered(d)
	}
	r := Reclaim{Trigger: "pressure"}
	err = s.prune(ctx, owner, idle, time.Now(), recovered, &r)
	for _, v := range idle {
		if recovered() {
			break
		}
		err = errors.Join(err, s.evict(ctx, v, &r))
	}
	s.reclaimed(r, d.Available)
	return err
}

// budget measures idle volumes used since their last measurement, then removes the least
// recently used idle ones while the total exceeds Budget. Busy volumes count with their
// last measured size.
func (s *Sessions) budget(ctx context.Context, owner string, labels map[string]string) error {
	before := s.available()
	idle, err := s.idle(labels)
	if err != nil {
		return err
	}
	sizes := map[string]int64{}
	for name := range labels {
		record, _ := s.record(name)
		sizes[name] = record.Size
	}
	for _, v := range idle {
		if _, current := s.record(v.name); current || !s.reserve(v) {
			continue
		}
		u, ok, gcErr := s.gc(ctx, owner, v.name, false)
		if ok {
			sizes[v.name] = u.Size
		}
		err = errors.Join(err, gcErr)
	}
	var total int64
	for _, size := range sizes {
		total += size
	}
	r := Reclaim{Trigger: "budget"}
	for _, v := range idle {
		if total <= s.Budget || ctx.Err() != nil {
			break
		}
		removed := r.Removed
		err = errors.Join(err, s.evict(ctx, v, &r))
		if r.Removed > removed {
			total -= sizes[v.name]
		}
	}
	s.reclaimed(r, before)
	return err
}

// prune deletes rebuildable outputs of idle volumes last used before cutoff and not pruned
// since, least recently used first, until enough.
func (s *Sessions) prune(ctx context.Context, owner string, idle []idleVolume, cutoff time.Time, enough func() bool, r *Reclaim) error {
	var err error
	for _, v := range idle {
		if ctx.Err() != nil || !v.used.Before(cutoff) || enough() {
			break
		}
		if record, current := s.record(v.name); current && record.Pruned || !s.reserve(v) {
			continue
		}
		u, ok, gcErr := s.gc(ctx, owner, v.name, true)
		if ok {
			r.Pruned++
			r.Artifacts += u.Freed
		}
		err = errors.Join(err, gcErr)
	}
	return err
}

// evict removes one idle volume unless an attempt took it meanwhile.
func (s *Sessions) evict(ctx context.Context, v idleVolume, r *Reclaim) error {
	if ctx.Err() != nil || !s.reserve(v) {
		return nil
	}
	err := s.remove(ctx, v.name)
	if err == nil {
		r.Removed++
	}
	s.unreserve(v.name)
	return err
}

func (s *Sessions) remove(ctx context.Context, name string) error {
	if _, err := commandWithin(ctx, RemoveTimeout, "volume", "rm", "-f", name); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.Dir, name+".gc")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(filepath.Join(s.Dir, name))
}

// gc runs the helper on a reserved volume, records its usage and releases the volume
// unless a cut helper container may still run.
func (s *Sessions) gc(ctx context.Context, owner, name string, prune bool) (helperUsage, bool, error) {
	mode, container := "du", name+"-gc"
	if prune {
		mode = "prune"
	}
	var u helperUsage
	out, err := commandWithin(ctx, gcTimeout, "run", "--rm", "--name", container, "--label", ownerLabel+"="+owner, "--pull=never", "--runtime=runc",
		"--network=none", "--read-only", "--user="+AttemptUser, "--cap-drop=ALL", "--security-opt=no-new-privileges=true",
		"--memory=64m", "--pids-limit=16", "--log-driver=none", "--mount=type=volume,source="+name+",target=/work,volume-nocopy",
		"--entrypoint", "/sandbox-helper", s.Helper, mode, "/work")
	if err == nil && json.Unmarshal(out, &u) != nil {
		err = fmt.Errorf("helper %s of %s: invalid output", mode, name)
	}
	if err != nil {
		if _, rmErr := command(context.WithoutCancel(ctx), "rm", "-f", container); rmErr != nil {
			return u, false, errors.Join(err, rmErr)
		}
		s.unreserve(name)
		return u, false, err
	}
	data, _ := json.Marshal(gcRecord{Pruned: prune, Size: u.Size})
	err = os.WriteFile(filepath.Join(s.Dir, name+".gc"), data, 0600)
	s.unreserve(name)
	if err == nil && u.Failed > 0 {
		err = fmt.Errorf("helper %s of %s could not remove %d entries", mode, name, u.Failed)
	}
	return u, true, err
}

// helperUsage is the output of sandbox-helper prune and du.
type helperUsage struct {
	Freed  int64 `json:"freed"`
	Size   int64 `json:"size"`
	Failed int   `json:"failed"`
}

// record returns the last helper result and whether it is newer than the last use.
func (s *Sessions) record(name string) (gcRecord, bool) {
	var r gcRecord
	path := filepath.Join(s.Dir, name)
	data, err := os.ReadFile(path + ".gc")
	info, infoErr := os.Stat(path + ".gc")
	used, usedErr := os.Stat(path)
	if err != nil || infoErr != nil || json.Unmarshal(data, &r) != nil {
		return gcRecord{}, false
	}
	return r, usedErr == nil && !info.ModTime().Before(used.ModTime())
}

// idle lists volumes that are not busy, least recently used first.
func (s *Sessions) idle(labels map[string]string) ([]idleVolume, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idle := []idleVolume{}
	for name := range labels {
		if s.busy[name] {
			continue
		}
		used, err := s.used(name)
		if err != nil {
			return nil, err
		}
		idle = append(idle, idleVolume{name, used})
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].used.Before(idle[j].used) })
	return idle, nil
}

// used returns the last use; volumes without a record count as used now.
func (s *Sessions) used(name string) (time.Time, error) {
	if info, err := os.Stat(filepath.Join(s.Dir, name)); err == nil {
		return info.ModTime(), nil
	}
	return time.Now(), s.touch(name)
}

// reserve marks an idle volume busy unless an attempt holds it or used it since listed.
func (s *Sessions) reserve(v idleVolume) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy == nil {
		s.busy = map[string]bool{}
	}
	if info, err := os.Stat(filepath.Join(s.Dir, v.name)); s.busy[v.name] || err != nil || info.ModTime().After(v.used) {
		return false
	}
	s.busy[v.name] = true
	return true
}

// unreserve frees a volume without recording a use.
func (s *Sessions) unreserve(name string) {
	s.mu.Lock()
	delete(s.busy, name)
	s.mu.Unlock()
}

func (s *Sessions) disk() (pool.Disk, error) {
	d, err := s.Disk()
	if err == nil && d.Total == 0 {
		err = errors.New("host Docker filesystem unreadable")
	}
	return d, err
}

func (s *Sessions) available() uint64 {
	if s.Disk == nil {
		return 0
	}
	d, _ := s.disk()
	return d.Available
}

func (s *Sessions) reclaimed(r Reclaim, before uint64) {
	if r.Removed+r.Pruned == 0 || s.Reclaimed == nil {
		return
	}
	if after := s.available(); before > 0 && after > before {
		r.Freed = int64(after - before)
	}
	s.Reclaimed(r)
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

// sessionIssue returns the workspace and issue of an issue session; chat sessions
// (`chat:<id>`) have none, and asking Multica about them fails the whole batch.
func sessionIssue(label string) (string, string, bool) {
	parts := strings.Split(label, "/")
	return parts[0], parts[len(parts)-1], len(parts) == 3 && !strings.HasPrefix(parts[2], "chat:")
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
		used, err := s.used(name)
		if err != nil {
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
