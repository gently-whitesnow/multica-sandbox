package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

// mirrorLabel binds a reference mirror volume to its workspace (ADR 0035).
const mirrorLabel = "io.multica-sandbox.mirror"

// mirrorResources bound one refresh: git fetch and index-pack of a large repository.
var mirrorResources = Resources{NanoCPUs: 1e9, Memory: 1 << 30, Pids: 128, NoFile: 1024, Workspace: 64 << 20, Tmp: 64 << 20, Shm: 8 << 20}

var mirrorKey = regexp.MustCompile(`^[0-9a-f]{32}$`)

// MirrorGrants is the Git relay (repo.Relay): its grants are the only credentials a refresh holds.
type MirrorGrants interface {
	Serves(workspace, url string) bool
	Issue(attempt, workspace string, urls []string) (map[string]string, error)
	Revoke(attempt string)
}

// Mirrors keeps one bare reference mirror per (owner, workspace, repository): a volume per
// workspace holds a directory per repository, and attempts of that workspace mount it
// read-only at repo.MirrorDir. A container from the agent image fetches a mirror through
// the Git relay, one at a time per mirror. Dir records each mirror's last use and each
// volume's measured sizes; Sessions collects mirrors with the session volumes.
type Mirrors struct {
	Workloads *Projected
	Git       MirrorGrants
	Dir       string
	// Refresh is the age after which a checkout refreshes a mirror; Wait bounds a checkout's
	// wait for a mirror it never used; Timeout bounds one refresh. Mirrors unused for TTL
	// are removed.
	Refresh, Wait, Timeout, TTL time.Duration
	// Low, when set, reports the disk below the claim pause watermark: no new mirror starts.
	Low    func() bool
	Report func(error)
	// fetch replaces the refresh container in tests.
	fetch     func(ctx context.Context, workspace, volume, key, url string) error
	mu        sync.Mutex
	prepare   sync.Mutex
	flights   map[string]*flight
	attempted map[string]time.Time
	ready     map[string]bool
	// using counts the checkouts cloning from a mirror; dropping holds mirrors being removed.
	using    map[string]int
	dropping map[string]bool
}

type flight struct {
	done chan struct{}
	err  error
}

// Ensure returns the mirror of a workspace repository and starts its refresh when none ran
// for Refresh. Only a checkout without a record of the mirror waits for it, up to Wait;
// others use what the mirror holds while the refresh continues. The checkout calls release
// after its clone; until then the mirror is not collected.
func (m *Mirrors) Ensure(ctx context.Context, workspace, url string) (string, func(), bool) {
	key, ok := repo.MirrorKey(url)
	if !ok || workspace == "" || !m.Git.Serves(workspace, url) {
		return "", nil, false
	}
	volume := m.volumeName(workspace)
	id := volume + "/" + key
	m.mu.Lock()
	_, err := os.Stat(filepath.Join(m.Dir, id))
	used := err == nil
	if m.dropping[id] || !used && m.Low != nil && m.Low() {
		m.mu.Unlock()
		return "", nil, false
	}
	err = touch(filepath.Join(m.Dir, id))
	f := m.flights[id]
	if f == nil && time.Since(m.attempted[id]) >= m.Refresh {
		f = &flight{done: make(chan struct{})}
		if m.flights == nil {
			m.flights = map[string]*flight{}
		}
		m.flights[id] = f
		go m.run(f, id, workspace, volume, key, url)
	}
	if m.using == nil {
		m.using = map[string]int{}
	}
	m.using[id]++
	m.mu.Unlock()
	release := sync.OnceFunc(func() {
		m.mu.Lock()
		if m.using[id]--; m.using[id] == 0 {
			delete(m.using, id)
		}
		m.mu.Unlock()
	})
	if err != nil {
		m.report(err)
	}
	if f == nil || used {
		return key, release, true
	}
	timer := time.NewTimer(m.Wait)
	defer timer.Stop()
	select {
	case <-f.done:
		if f.err == nil {
			return key, release, true
		}
	case <-ctx.Done():
	case <-timer.C:
	}
	release()
	return "", nil, false
}

// touch records a use.
func touch(path string) error {
	err := os.MkdirAll(filepath.Dir(path), 0700)
	if err == nil {
		err = os.WriteFile(path, nil, 0600)
	}
	if now := time.Now(); err == nil {
		err = os.Chtimes(path, now, now)
	}
	return err
}

func (m *Mirrors) run(f *flight, id, workspace, volume, key, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), m.Timeout)
	defer cancel()
	fetch := m.fetch
	if fetch == nil {
		fetch = m.container
	}
	f.err = fetch(ctx, workspace, volume, key, url)
	m.mu.Lock()
	delete(m.flights, id)
	if m.attempted == nil {
		m.attempted = map[string]time.Time{}
	}
	m.attempted[id] = time.Now()
	m.mu.Unlock()
	if f.err != nil {
		m.report(fmt.Errorf("refresh mirror %s: %w", id, f.err))
	}
	close(f.done)
}

func (m *Mirrors) report(err error) {
	if m.Report != nil {
		m.Report(err)
	}
}

func (m *Mirrors) volumeName(workspace string) string {
	sum := sha256.Sum256([]byte(m.Workloads.Owner + ":mirror:" + workspace))
	return "multica-sandbox-mirror-" + hex.EncodeToString(sum[:16])
}

// volume returns the workspace's mirror volume, created like a workdir volume for the
// attempt user. A same-named volume with other labels is refused.
func (m *Mirrors) volume(ctx context.Context, workspace string) (string, error) {
	name := m.volumeName(workspace)
	m.prepare.Lock()
	defer m.prepare.Unlock()
	if m.ready[name] {
		return name, nil
	}
	b := m.Workloads
	data, err := command(ctx, "volume", "ls", "-q", "--filter", "name="+name)
	switch {
	case err != nil:
	case !slices.Contains(strings.Fields(string(data)), name):
		err = b.workdir(ctx, name, "--label", mirrorLabel+"="+workspace)
	default:
		data, err = command(ctx, "volume", "inspect", name)
		var volumes []struct{ Labels map[string]string }
		if err != nil || json.Unmarshal(data, &volumes) != nil || len(volumes) != 1 ||
			volumes[0].Labels[ownerLabel] != b.Owner || volumes[0].Labels[mirrorLabel] != workspace {
			err = errors.Join(err, fmt.Errorf("mirror volume %s is not bound to this workspace", name))
		}
	}
	if err != nil {
		return "", err
	}
	if m.ready == nil {
		m.ready = map[string]bool{}
	}
	m.ready[name] = true
	return name, nil
}

// container fetches a mirror in a container from the agent image on its own attempt-style
// network. It holds a Git relay grant for the workspace and runs only repo.MirrorScript.
func (m *Mirrors) container(ctx context.Context, workspace, volume, key, url string) (err error) {
	b := m.Workloads
	if _, err = m.volume(ctx, workspace); err != nil {
		return err
	}
	grant := "mirror:" + volume + "/" + key
	env, err := m.Git.Issue(grant, workspace, []string{url})
	if err != nil {
		return err
	}
	defer m.Git.Revoke(grant)
	probe := Projected{Backend: Backend{Image: b.Image, Owner: b.Owner, Command: []string{"/bin/sh", "-c", repo.MirrorScript, "mirror", "/mirror/" + key, url}}, Bundles: b.Bundles}
	bundles, _, err := probe.prepare(ctx)
	if err != nil {
		return err
	}
	r := &run{name: b.name(grant)}
	network, err := b.isolate(ctx, r.name+"-net")
	if err != nil {
		return err
	}
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err = errors.Join(err, r.Remove(cleanCtx), removeNetwork(cleanCtx, network, b.Peers))
	}()
	extra := append(bundles, "--mount=type=volume,source="+volume+",target=/mirror,volume-nocopy", "--env=GIT_CONFIG_NOSYSTEM=1", "--env=GIT_ALLOW_PROTOCOL=http")
	environ := os.Environ()
	for name, value := range env {
		if strings.HasPrefix(name, "GIT_") && envPattern.MatchString(name) && !strings.ContainsRune(value, 0) {
			extra = append(extra, "--env", name)
			environ = append(environ, name+"="+value)
		}
	}
	create := exec.CommandContext(ctx, "docker", attemptNetwork(probe.createArgs(r.name, mirrorResources, extra...), network)...)
	create.Env = environ
	if err = create.Run(); err != nil {
		return fmt.Errorf("create mirror refresh: %w", err)
	}
	_, err = commandWithin(ctx, m.Timeout, "start", "--attach", r.name)
	return err
}

// idleMirror is a recorded mirror with its last use and last measured size.
type idleMirror struct {
	volume, key string
	used        time.Time
	size        int64
}

// volumes lists the owned mirror volumes.
func (m *Mirrors) volumes(ctx context.Context) ([]string, error) {
	data, err := command(ctx, "volume", "ls", "--filter", "label="+ownerLabel+"="+m.Workloads.Owner, "--filter", "label="+mirrorLabel, "--format", "{{.Name}}")
	return strings.Fields(string(data)), err
}

// idle lists the recorded mirrors of volumes that no checkout, refresh or removal holds,
// least recently used first, and the measured bytes of all of them.
func (m *Mirrors) idle(volumes []string) ([]idleMirror, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	idle, total := []idleMirror{}, int64(0)
	for _, volume := range volumes {
		sizes := m.sizes(volume)
		entries, _ := os.ReadDir(filepath.Join(m.Dir, volume))
		for _, entry := range entries {
			key, id := entry.Name(), volume+"/"+entry.Name()
			info, err := entry.Info()
			if !mirrorKey.MatchString(key) || err != nil {
				continue
			}
			total += sizes[key]
			if m.using[id] == 0 && m.flights[id] == nil && !m.dropping[id] {
				idle = append(idle, idleMirror{volume, key, info.ModTime(), sizes[key]})
			}
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].used.Before(idle[j].used) })
	return idle, total
}

// sizes reads a volume's last measurement.
func (m *Mirrors) sizes(volume string) map[string]int64 {
	sizes := map[string]int64{}
	data, _ := os.ReadFile(filepath.Join(m.Dir, volume+".gc"))
	_ = json.Unmarshal(data, &sizes)
	return sizes
}

// expire removes mirrors unused for TTL and measures every volume.
func (m *Mirrors) expire(ctx context.Context, r *Reclaim) error {
	volumes, err := m.volumes(ctx)
	if err != nil {
		return err
	}
	for _, volume := range volumes {
		idle, _ := m.idle([]string{volume})
		doomed := []idleMirror{}
		for _, v := range idle {
			if time.Since(v.used) > m.TTL {
				doomed = append(doomed, v)
			}
		}
		err = errors.Join(err, m.drop(ctx, volume, doomed, r))
	}
	return err
}

// evict removes one idle mirror unless a checkout used it since it was listed.
func (m *Mirrors) evict(ctx context.Context, v idleMirror, r *Reclaim) error {
	if ctx.Err() != nil {
		return nil
	}
	return m.drop(ctx, v.volume, []idleMirror{v}, r)
}

// drop removes the mirrors still idle from a volume with the helper, as the attempt user
// without network or capabilities, and records the sizes of what remains. A mirror the
// volume holds without a record counts as used now, as upstream's missing stamp does.
func (m *Mirrors) drop(ctx context.Context, volume string, mirrors []idleMirror, r *Reclaim) error {
	m.mu.Lock()
	if m.dropping == nil {
		m.dropping = map[string]bool{}
	}
	keys := []string{}
	for _, v := range mirrors {
		id := volume + "/" + v.key
		info, err := os.Stat(filepath.Join(m.Dir, id))
		if m.using[id] == 0 && m.flights[id] == nil && !m.dropping[id] && err == nil && !info.ModTime().After(v.used) {
			m.dropping[id] = true
			keys = append(keys, v.key)
		}
	}
	m.mu.Unlock()
	container := volume + "-gc"
	out, err := commandWithin(ctx, gcTimeout, append([]string{"run", "--rm", "--name", container, "--label", ownerLabel + "=" + m.Workloads.Owner, "--pull=never", "--runtime=runc",
		"--network=none", "--read-only", "--user=" + AttemptUser, "--cap-drop=ALL", "--security-opt=no-new-privileges=true",
		"--memory=64m", "--pids-limit=16", "--log-driver=none", "--mount=type=volume,source=" + volume + ",target=/mirror,volume-nocopy",
		"--entrypoint", "/sandbox-helper", m.Workloads.Helper, "drop", "/mirror"}, keys...)...)
	var u struct {
		helperUsage
		Mirrors map[string]int64 `json:"mirrors"`
	}
	if err == nil && json.Unmarshal(out, &u) != nil {
		err = fmt.Errorf("helper drop of %s: invalid output", volume)
	}
	if err != nil {
		if _, rmErr := command(context.WithoutCancel(ctx), "rm", "-f", container); rmErr != nil {
			// A cut helper may still run: the mirrors stay held.
			return errors.Join(err, rmErr)
		}
	} else {
		r.Mirrors += len(keys)
		r.MirrorBytes += u.Freed
		if u.Failed > 0 {
			err = fmt.Errorf("helper drop of %s could not remove %d entries", volume, u.Failed)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range keys {
		id := volume + "/" + key
		delete(m.dropping, id)
		if u.Mirrors != nil {
			delete(m.attempted, id)
			if rmErr := os.Remove(filepath.Join(m.Dir, id)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				err = errors.Join(err, rmErr)
			}
		}
	}
	if u.Mirrors == nil {
		return err
	}
	for key := range u.Mirrors {
		if path := filepath.Join(m.Dir, volume, key); mirrorKey.MatchString(key) {
			if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
				err = errors.Join(err, touch(path))
			}
		}
	}
	data, _ := json.Marshal(u.Mirrors)
	return errors.Join(err, os.WriteFile(filepath.Join(m.Dir, volume+".gc"), data, 0600))
}
