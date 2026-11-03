package pool

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const attemptID = "8570c5417a70e31995b7fc2d2574f47eff622228da8207c68a7e23c9021b8b84"

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// slice fakes a pool with 8 GiB memory.high, 10 GiB memory.max and 8 GiB swap; cache
// of current is file cache, half of it active.
func slice(t *testing.T, pressure string, current, cache int64) *Pool {
	dir := t.TempDir()
	write(t, dir, map[string]string{"memory.high": "8589934592\n", "memory.max": "10737418240\n", "memory.swap.max": "8589934592\n",
		"memory.pressure": "some avg10=" + pressure + " avg60=1.00 avg300=0.50 total=12345\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=10\n",
		"memory.current":  strconv.FormatInt(current, 10) + "\n", "memory.stat": "anon 100\nfile " + strconv.FormatInt(cache, 10) + "\ninactive_file " + strconv.FormatInt(cache/2, 10) + "\nactive_file " + strconv.FormatInt(cache-cache/2, 10) + "\n"})
	return &Pool{Parent: "multica_attempts.slice", Dir: dir, MaxPressure: DefaultMaxPressure, MaxWorkingSet: DefaultMaxWorkingSet}
}

func TestLimitsRequireBoundedPool(t *testing.T) {
	p := slice(t, "0.00", 0, 0)
	if l, err := p.Limits(); err != nil || l.High != 8<<30 || l.Max != 10<<30 || l.SwapMax != 8<<30 {
		t.Fatal(l, err)
	}
	for name, files := range map[string]map[string]string{
		"no high":       {"memory.high": "max\n"},
		"no max":        {"memory.max": "max\n"},
		"high over max": {"memory.high": "21474836480\n"},
		"garbage":       {"memory.swap.max": "lots\n"},
	} {
		p := slice(t, "0.00", 0, 0)
		write(t, p.Dir, files)
		if _, err := p.Limits(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := (&Pool{Dir: filepath.Join(t.TempDir(), "missing")}).Limits(); err == nil {
		t.Error("missing slice accepted")
	}
}

func TestAdmitOnPressureAndWorkingSet(t *testing.T) {
	for name, test := range map[string]struct {
		pressure       string
		current, cache int64
		admit          bool
	}{
		"idle":                {"0.00", 1 << 30, 0, true},
		"psi over threshold":  {"23.51", 1 << 30, 0, false},
		"psi at threshold":    {"10.00", 1 << 30, 0, true},
		"working set high":    {"0.00", 15 << 29, 0, false},
		"page cache excluded": {"0.00", 15 << 29, 4 << 30, true},
		// Excluding cache leaves its reclaim cost to PSI.
		"cache at memory.high":          {"0.00", 8 << 30, 7 << 30, true},
		"cache at memory.high reclaims": {"23.51", 8 << 30, 7 << 30, false},
	} {
		ok, status, err := slice(t, test.pressure, test.current, test.cache).Admit()
		if err != nil || ok != test.admit || (status.Refusal() == "") != test.admit {
			t.Errorf("%s: admit=%t %v %v", name, ok, status, err)
		}
	}
	p := slice(t, "0.00", 0, 0)
	write(t, p.Dir, map[string]string{"memory.pressure": "garbage\n"})
	if ok, _, err := p.Admit(); ok || err == nil {
		t.Error("unreadable pressure admitted")
	}
	if _, status, _ := slice(t, "12.5", 3<<30, 1<<30).Admit(); status.String() != "memory pressure some avg10=12.50%, working set 2.0 GiB of memory.high 8.0 GiB" {
		t.Error(status.String())
	}
}

// Only memory the kernel cannot reclaim counts: page cache, active or not, and
// reclaimable slab are left out, shmem and other kernel memory stay.
func TestWorkingSetCountsUnreclaimableMemory(t *testing.T) {
	for name, c := range map[string]struct {
		stat string
		want int64
	}{
		"page cache":       {"anon 1000\nfile 6000\ninactive_file 4000\nactive_file 2000\n", 4000},
		"shmem":            {"anon 1000\nfile 6000\nshmem 2500\n", 6500},
		"reclaimable slab": {"anon 1000\nfile 6000\nkernel 2000\nslab_reclaimable 1500\n", 2500},
		"no stat keys":     {"", 10000},
		"cache over usage": {"file 20000\n", 0},
	} {
		dir := t.TempDir()
		write(t, dir, map[string]string{"memory.current": "10000\n", "memory.stat": c.stat})
		if got, err := workingSet(dir); err != nil || got != c.want {
			t.Errorf("%s: %d %v, want %d", name, got, err, c.want)
		}
	}
	if _, err := workingSet(t.TempDir()); err == nil {
		t.Error("unreadable cgroup counted")
	}
}

func TestLargestAttemptReadsAttemptCgroups(t *testing.T) {
	p := slice(t, "0.00", 0, 0)
	if n, err := p.largestAttempt(); err != nil || n != 0 {
		t.Fatal("empty pool:", n, err)
	}
	for dir, current := range map[string]string{"docker-" + attemptID + ".scope": "3000", strings.Repeat("a", 64): "5000", "init.scope": "9000", "docker-x.scope": "9000"} {
		write(t, filepath.Join(p.Dir, dir), map[string]string{"memory.current": current + "\n", "memory.stat": "file 1000\n"})
	}
	// A removed attempt leaves an empty directory behind while the kernel frees it.
	write(t, filepath.Join(p.Dir, strings.Repeat("b", 64)), nil)
	if n, err := p.largestAttempt(); err != nil || n != 4000 {
		t.Fatal(n, err)
	}
	if _, err := (&Pool{Dir: filepath.Join(t.TempDir(), "missing")}).largestAttempt(); err == nil {
		t.Fatal("missing pool read")
	}
}

func TestAttemptPlacementAndMemory(t *testing.T) {
	p := slice(t, "0.00", 0, 0)
	if _, err := p.Attempt(attemptID); err == nil {
		t.Fatal("absent attempt found")
	}
	for _, dir := range []string{"docker-" + attemptID + ".scope", attemptID} {
		p := slice(t, "0.00", 0, 0)
		write(t, filepath.Join(p.Dir, dir), map[string]string{"memory.max": "max\n", "memory.swap.max": "max\n",
			"memory.events": "low 0\nhigh 4\nmax 9\noom 0\noom_kill 2\noom_group_kill 0\n", "memory.peak": "3221225472\n"})
		if err := p.CheckAttempt(attemptID, 0); err != nil {
			t.Fatal(dir, err)
		}
		if p.CheckAttempt(attemptID, 6<<30) == nil {
			t.Fatal("missing ceiling accepted")
		}
		if m, err := p.AttemptMemory(attemptID); err != nil || m != (Memory{OOM: 0, OOMKill: 2, Peak: 3 << 30, Max: Unlimited}) {
			t.Fatal(m, err)
		}
		if err := os.Remove(filepath.Join(p.Dir, dir, "memory.peak")); err != nil {
			t.Fatal(err)
		}
		if m, err := p.AttemptMemory(attemptID); err != nil || m != (Memory{OOMKill: 2, Peak: -1, Max: Unlimited}) {
			t.Fatal("without memory.peak:", m, err)
		}
		write(t, filepath.Join(p.Dir, dir), map[string]string{"memory.max": "6442450944\n", "memory.swap.max": "0\n"})
		if p.CheckAttempt(attemptID, 6<<30) == nil {
			t.Fatal("attempt without swap accepted")
		}
	}
}

func TestParseMemoryAsCatPrintsIt(t *testing.T) {
	m, err := ParseMemory([]byte("low 0\nhigh 0\nmax 12\noom 3\noom_kill 3\noom_group_kill 0\n1073741824\n805306368\n"))
	if err != nil || m != (Memory{OOM: 3, OOMKill: 3, Peak: 768 << 20, Max: 1 << 30}) {
		t.Fatal(m, err)
	}
	// Before Linux 5.19 there is no memory.peak.
	if m, err := ParseMemory([]byte("oom 0\noom_kill 1\nmax\n")); err != nil || m != (Memory{OOMKill: 1, Peak: -1, Max: Unlimited}) {
		t.Fatal(m, err)
	}
	for _, bad := range []string{"", "oom 1\noom_kill 1\n", "oom 1\noom_kill x\n1\n2\n", "oom 1\n1\n2\n", "oom 1\noom_kill 1\n1\n2\n3\n", "a b c\n"} {
		if _, err := ParseMemory([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestValidParentFollowsCgroupDriver(t *testing.T) {
	for parent, driver := range map[string]string{"multica_attempts.slice": "systemd", "/multica/attempts": "cgroupfs"} {
		if err := ValidParent(parent, driver); err != nil {
			t.Error(parent, err)
		}
	}
	for parent, driver := range map[string]string{"multica_attempts.slice": "cgroupfs", "/multica": "systemd", "../x.slice": "systemd", "/a/../b": "cgroupfs", "": "systemd"} {
		if ValidParent(parent, driver) == nil {
			t.Error("accepted", parent, driver)
		}
	}
}
