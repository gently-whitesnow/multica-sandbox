package pool

import (
	"os"
	"path/filepath"
	"strconv"
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

// slice fakes a pool with 8 GiB memory.high, 10 GiB memory.max and 8 GiB swap.
func slice(t *testing.T, pressure string, current, inactive int64) *Pool {
	dir := t.TempDir()
	write(t, dir, map[string]string{"memory.high": "8589934592\n", "memory.max": "10737418240\n", "memory.swap.max": "8589934592\n",
		"memory.pressure": "some avg10=" + pressure + " avg60=1.00 avg300=0.50 total=12345\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=10\n",
		"memory.current":  strconv.FormatInt(current, 10) + "\n", "memory.stat": "anon 100\nfile 200\ninactive_file " + strconv.FormatInt(inactive, 10) + "\nactive_file 5\n"})
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
		pressure          string
		current, inactive int64
		admit             bool
	}{
		"idle":                {"0.00", 1 << 30, 0, true},
		"psi over threshold":  {"23.51", 1 << 30, 0, false},
		"psi at threshold":    {"10.00", 1 << 30, 0, true},
		"working set high":    {"0.00", 15 << 29, 0, false},
		"page cache excluded": {"0.00", 15 << 29, 4 << 30, true},
	} {
		ok, status, err := slice(t, test.pressure, test.current, test.inactive).Admit()
		if err != nil || ok != test.admit {
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
		write(t, filepath.Join(p.Dir, dir), map[string]string{"memory.max": "6442450944\n", "memory.swap.max": "0\n"})
		if p.CheckAttempt(attemptID, 6<<30) == nil {
			t.Fatal("attempt without swap accepted")
		}
	}
}

func TestParseMemoryAsCatPrintsIt(t *testing.T) {
	m, err := ParseMemory([]byte("low 0\nhigh 0\nmax 12\noom 3\noom_kill 3\noom_group_kill 0\n1073741824\n1073741824\n"))
	if err != nil || m != (Memory{OOM: 3, OOMKill: 3, Peak: 1 << 30, Max: 1 << 30}) {
		t.Fatal(m, err)
	}
	for _, bad := range []string{"", "oom 1\noom_kill 1\n5\n", "oom 1\noom_kill x\n1\n2\n", "oom 1\n1\n2\n", "oom 1\noom_kill 1\n1\n2\n3\n", "a b c\n"} {
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
