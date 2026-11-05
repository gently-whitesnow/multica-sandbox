package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

type fakeMirrorGrants struct{}

func (fakeMirrorGrants) Serves(workspace, url string) bool {
	return workspace == "w1" && strings.HasPrefix(strings.ToLower(url), "https://git.example.test/")
}
func (fakeMirrorGrants) Issue(string, string, []string) (map[string]string, error) { return nil, nil }
func (fakeMirrorGrants) Revoke(string)                                             {}

var repoMirrorKey = repo.MirrorKey

const mirrorOwner = "10000000-0000-4000-8000-000000000002"

func testMirrors(t *testing.T, fetch func(ctx context.Context, workspace, volume, key, url string) error) *Mirrors {
	return &Mirrors{Workloads: &Projected{Backend: Backend{Owner: mirrorOwner}}, Git: fakeMirrorGrants{}, Dir: t.TempDir(),
		Refresh: time.Hour, Wait: 5 * time.Second, Timeout: 5 * time.Second, TTL: time.Hour, fetch: fetch}
}

func TestMirrorVolumesAreKeyedByOwnerAndWorkspace(t *testing.T) {
	m := testMirrors(t, nil)
	other := &Mirrors{Workloads: &Projected{Backend: Backend{Owner: "10000000-0000-4000-8000-000000000003"}}}
	a, b := m.volumeName("w1"), m.volumeName("w2")
	if a == b || a == other.volumeName("w1") || !strings.HasPrefix(a, "multica-sandbox-mirror-") || len(a) != len("multica-sandbox-mirror-")+32 {
		t.Fatalf("volume names %s %s %s", a, b, other.volumeName("w1"))
	}
}

// Concurrent first checkouts of one repository share one refresh and wait for it; remote
// forms of the repository share the mirror; later checkouts within Refresh run none.
func TestMirrorEnsureSingleFlight(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	m := testMirrors(t, func(ctx context.Context, workspace, volume, key, url string) error {
		calls.Add(1)
		<-release
		return nil
	})
	urls := []string{"https://git.example.test/team/fixture.git", "https://git.example.test/team/fixture", "https://GIT.example.test/team/fixture/"}
	var wg sync.WaitGroup
	keys := make([]string, 6)
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, release, ok := m.Ensure(context.Background(), "w1", urls[i%len(urls)])
			if !ok {
				key = ""
			} else {
				release()
			}
			keys[i] = key
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 || keys[0] == "" || slices.ContainsFunc(keys, func(k string) bool { return k != keys[0] }) {
		t.Fatalf("calls=%d keys=%v", calls.Load(), keys)
	}
	if key, _, ok := m.Ensure(context.Background(), "w1", urls[0]); !ok || key != keys[0] || calls.Load() != 1 {
		t.Fatal("fresh mirror refreshed again")
	}
	if _, err := os.Stat(filepath.Join(m.Dir, m.volumeName("w1"), keys[0])); err != nil {
		t.Fatal("use not recorded")
	}
	// A used mirror past Refresh is returned at once while its refresh runs.
	m.Refresh = 0
	release = make(chan struct{})
	start := time.Now()
	if key, _, ok := m.Ensure(context.Background(), "w1", urls[0]); !ok || key != keys[0] || time.Since(start) > time.Second {
		t.Fatal("stale mirror waited for its refresh")
	}
	close(release)
}

// Checkouts fall back to a plain clone when the first refresh fails or outlasts Wait, for
// repositories the workspace does not bind, while the mirror is removed and, for a new
// mirror, while the disk is low.
func TestMirrorEnsureFallsBack(t *testing.T) {
	failed := errors.New("relay unavailable")
	var reports atomic.Int32
	m := testMirrors(t, func(context.Context, string, string, string, string) error { return failed })
	m.Report = func(err error) {
		if errors.Is(err, failed) {
			reports.Add(1)
		}
	}
	if _, _, ok := m.Ensure(context.Background(), "w1", "https://git.example.test/team/a.git"); ok || reports.Load() != 1 {
		t.Fatal("failed refresh offered a mirror")
	}
	// The record stays: the next checkout no longer waits, and passes a mirror path the
	// script may not find.
	if _, _, ok := m.Ensure(context.Background(), "w1", "https://git.example.test/team/a.git"); !ok {
		t.Fatal("recorded mirror not offered")
	}
	slow := make(chan struct{})
	defer close(slow)
	m = testMirrors(t, func(ctx context.Context, _, _, _, _ string) error {
		select {
		case <-slow:
		case <-ctx.Done():
		}
		return ctx.Err()
	})
	m.Wait = 50 * time.Millisecond
	if _, _, ok := m.Ensure(context.Background(), "w1", "https://git.example.test/team/b.git"); ok {
		t.Fatal("unfinished refresh offered a mirror")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Wait = time.Minute
	if _, _, ok := m.Ensure(ctx, "w1", "https://git.example.test/team/c.git"); ok {
		t.Fatal("cancelled checkout offered a mirror")
	}
	for _, c := range [][2]string{{"w2", "https://git.example.test/team/a.git"}, {"w1", "https://other.example.test/team/a.git"}, {"w1", "ext::sh"}, {"", "https://git.example.test/team/a.git"}} {
		if _, _, ok := m.Ensure(context.Background(), c[0], c[1]); ok {
			t.Fatalf("%v offered a mirror", c)
		}
	}
	key, _ := repoMirrorKey("https://git.example.test/team/a.git")
	m.dropping = map[string]bool{m.volumeName("w1") + "/" + key: true}
	if _, _, ok := m.Ensure(context.Background(), "w1", "https://git.example.test/team/a.git"); ok {
		t.Fatal("mirror offered while removed")
	}
	if _, _, ok := m.Ensure(context.Background(), "w1", "https://git.example.test/team/b.git"); !ok {
		t.Fatal("removal held another mirror of the volume")
	}
	m.Low = func() bool { return true }
	if _, _, ok := m.Ensure(context.Background(), "w1", "https://git.example.test/team/new.git"); ok {
		t.Fatal("new mirror started on a low disk")
	}
	if _, _, ok := m.Ensure(context.Background(), "w1", "https://git.example.test/team/b.git"); !ok {
		t.Fatal("low disk held a recorded mirror")
	}
	if _, err := os.Stat(filepath.Join(m.Dir, m.volumeName("w1"), mirrorKeyOf(t, "https://git.example.test/team/new.git"))); !os.IsNotExist(err) {
		t.Fatal("refused mirror recorded")
	}
}

func mirrorKeyOf(t *testing.T, url string) string {
	t.Helper()
	key, ok := repoMirrorKey(url)
	if !ok {
		t.Fatal(url)
	}
	return key
}

// recordMirror records a mirror's last use ago.
func recordMirror(t *testing.T, m *Mirrors, volume, key string, ago time.Duration) {
	t.Helper()
	path, at := filepath.Join(m.Dir, volume, key), time.Now().Add(-ago)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil || os.WriteFile(path, nil, 0600) != nil || os.Chtimes(path, at, at) != nil {
		t.Fatal(err)
	}
}

// A checkout holds its mirror until release; held and refreshing mirrors are not idle.
func TestMirrorLeaseHoldsCollection(t *testing.T) {
	m := testMirrors(t, func(context.Context, string, string, string, string) error { return nil })
	url := "https://git.example.test/team/fixture.git"
	key, release, ok := m.Ensure(context.Background(), "w1", url)
	if !ok {
		t.Fatal("no mirror")
	}
	volume := m.volumeName("w1")
	if idle, _ := m.idle([]string{volume}); len(idle) != 0 {
		t.Fatalf("held mirror idle: %v", idle)
	}
	release()
	release()
	if idle, _ := m.idle([]string{volume}); len(idle) != 1 || idle[0].key != key || m.using[volume+"/"+key] != 0 {
		t.Fatalf("released mirror not idle: %v %v", idle, m.using)
	}
	m.flights = map[string]*flight{volume + "/" + key: {}}
	if idle, _ := m.idle([]string{volume}); len(idle) != 0 {
		t.Fatal("refreshing mirror idle")
	}
}

// The sweep removes mirrors unused for TTL with the helper, records the sizes of the rest
// and backfills records of mirrors the volume holds without one, as upstream does.
func TestSweepExpiresMirrors(t *testing.T) {
	const mib = 1 << 20
	e := &fakeEngine{base: map[string]int64{}, artifacts: map[string]int64{}}
	s := fakeSessions(t, e, nil)
	m := testMirrors(t, nil)
	m.Workloads.Helper = "helper"
	s.Mirrors = m
	volume := m.volumeName("w1")
	old, recent, held, unknown := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32), strings.Repeat("d", 32)
	e.mirrors = map[string]map[string]int64{volume: {old: 300 * mib, recent: 200 * mib, held: 100 * mib, unknown: 50 * mib}}
	recordMirror(t, m, volume, old, 2*time.Hour)
	recordMirror(t, m, volume, recent, time.Minute)
	recordMirror(t, m, volume, held, 2*time.Hour)
	m.using = map[string]int{volume + "/" + held: 1}
	var reclaims []Reclaim
	s.Reclaimed = func(r Reclaim) { reclaims = append(reclaims, r) }
	if err := s.collect(context.Background(), "owner", true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.helper, []string{"drop " + volume + " " + old}) || len(e.unexpected) != 0 {
		t.Fatalf("helper %v other %v", e.helper, e.unexpected)
	}
	if want := []Reclaim{{Trigger: "lifetime", Mirrors: 1, MirrorBytes: 300 * mib}}; !slices.Equal(reclaims, want) {
		t.Fatalf("reclaims %+v", reclaims)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, volume, old)); !os.IsNotExist(err) {
		t.Fatal("record of a removed mirror kept")
	}
	if info, err := os.Stat(filepath.Join(m.Dir, volume, unknown)); err != nil || time.Since(info.ModTime()) > time.Minute {
		t.Fatal("unrecorded mirror not backfilled as used now")
	}
	m.using = nil
	idle, total := m.idle([]string{volume})
	if len(idle) != 3 || idle[0].key != held || idle[0].size != 100*mib || total != 350*mib {
		t.Fatalf("idle %+v total %d", idle, total)
	}
	// A failed helper keeps the records and frees the mirrors for the next sweep.
	e.helper = nil
	commandWithin = func(context.Context, time.Duration, ...string) ([]byte, error) {
		return nil, errors.New("engine unavailable")
	}
	m.TTL = time.Minute
	if err := m.expire(context.Background(), &Reclaim{}); err == nil {
		t.Fatal("failure not reported")
	}
	if _, err := os.Stat(filepath.Join(m.Dir, volume, held)); err != nil || len(m.dropping) != 0 {
		t.Fatal("failed removal lost the record or held the mirror")
	}
}

// Under pressure and the budget, mirrors and session volumes go in one least recently used
// order; busy volumes and mirrors in use stay, and mirror bytes count toward the budget.
func TestPressureAndBudgetReclaimMirrorsWithVolumes(t *testing.T) {
	const gib = 1 << 30
	e := &fakeEngine{available: 5 * gib,
		base:      map[string]int64{"old": 2 * gib, "new": 2 * gib},
		artifacts: map[string]int64{"old": 0, "new": 0}}
	s := fakeSessions(t, e, map[string]time.Duration{"old": 5 * time.Hour, "new": time.Minute})
	m := testMirrors(t, nil)
	m.Workloads.Helper = "helper"
	s.Mirrors, s.Disk = m, e.disk
	volume := m.volumeName("w1")
	older, mid, held := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	e.mirrors = map[string]map[string]int64{volume: {older: 3 * gib, mid: 5 * gib, held: 8 * gib}}
	recordMirror(t, m, volume, older, 6*time.Hour)
	recordMirror(t, m, volume, mid, 3*time.Hour)
	recordMirror(t, m, volume, held, 7*time.Hour)
	m.using = map[string]int{volume + "/" + held: 1}
	var reclaims []Reclaim
	s.Reclaimed = func(r Reclaim) { reclaims = append(reclaims, r) }
	if err := s.relieve(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	// From 5 GiB free the older mirror (8), the old volume (10) and the mid mirror go until
	// 15 GiB (15%) are free; the newer volume and the mirror in use stay.
	if e.labels["old"] != "" || e.labels["new"] == "" || len(e.mirrors[volume]) != 1 || e.mirrors[volume][held] == 0 {
		t.Fatalf("volumes %v mirrors %v", e.labels, e.mirrors)
	}
	want := Reclaim{Trigger: "pressure", Removed: 1, Pruned: 2, Mirrors: 2, MirrorBytes: 8 * gib, Freed: 10 * gib}
	if len(reclaims) != 1 || reclaims[0] != want {
		t.Fatalf("reclaims %+v", reclaims)
	}
	// The budget counts the held mirror's measured bytes: 8+1+2 GiB exceed 10 GiB, and the
	// mirror used before the newer volume goes.
	reclaims, e.available = nil, 50*gib
	if err := os.WriteFile(filepath.Join(s.Dir, "new.gc"), []byte(`{"size":2147483648}`), 0600); err != nil {
		t.Fatal(err)
	}
	recordMirror(t, m, volume, mid, 2*time.Hour)
	if err := os.WriteFile(filepath.Join(m.Dir, volume+".gc"), []byte(`{"`+held+`":8589934592,"`+mid+`":1073741824}`), 0600); err != nil {
		t.Fatal(err)
	}
	e.mirrors[volume][mid] = gib
	s.Budget = 10 * gib
	if err := s.budget(context.Background(), "owner", map[string]string{"new": "w1/agent/i-new"}); err != nil {
		t.Fatal(err)
	}
	if want := []Reclaim{{Trigger: "budget", Mirrors: 1, MirrorBytes: gib, Freed: gib}}; !slices.Equal(reclaims, want) || e.labels["new"] == "" {
		t.Fatalf("budget reclaims %+v volumes %v", reclaims, e.labels)
	}
}
