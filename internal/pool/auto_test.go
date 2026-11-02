package pool

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func autoPool(t *testing.T) *AutoAdmission {
	return &AutoAdmission{Pool: slice(t, "0.00", 1<<30, 0), DiskPath: "/host-docker", ReadDisk: func(string) (Disk, error) {
		return Disk{Total: 100 << 30, Available: 40 << 30, Files: 1000, FilesFree: 900}, nil
	}}
}

func TestAutoAdmissionPacesColdStartsAndPreservesHeadroom(t *testing.T) {
	a := autoPool(t)
	now := time.Now()
	if ok, _ := a.Admit(0, now); !ok {
		t.Fatal("first attempt blocked")
	}
	a.Started(now)
	for _, elapsed := range []time.Duration{0, time.Second, ColdStartInterval - time.Nanosecond} {
		if ok, _ := a.Admit(1, now.Add(elapsed)); ok {
			t.Fatal("cold burst admitted", elapsed)
		}
	}
	if ok, reason := a.Admit(1, now.Add(ColdStartInterval)); !ok {
		t.Fatal(reason)
	}
	// A compiler grows late, after warmup. Its observed working set raises the
	// estimate, rather than permitting a second equally expensive build.
	late := now.Add(2 * ColdStartInterval)
	host(t, a.Pool.Dir, mem{}, mem{anon: 5 << 30})
	a.Observe(1, late)
	if ok, _ := a.Admit(1, late); ok {
		t.Fatal("new build without headroom admitted")
	}
	host(t, a.Pool.Dir, mem{}, mem{anon: 1 << 30})
	a.Observe(1, late)
	if ok, _ := a.Admit(1, late); ok {
		t.Fatal("observed peak lost when current usage dropped")
	}
	if ok, _ := a.Admit(0, late); !ok {
		t.Fatal("history deadlocked an empty pool")
	}
	if ok, reason := a.Admit(1, late.Add(EstimateHalfLife)); !ok {
		t.Fatal("finished heavy build still limits starts:", reason)
	}
}

func TestAutoAdmissionEstimateDecays(t *testing.T) {
	a := autoPool(t)
	now := time.Now()
	for _, c := range []struct {
		name     string
		sample   int64
		age      time.Duration
		estimate int64
	}{
		{"first peak", 4 << 30, 0, 4 << 30},
		{"lower sample keeps the peak", 1 << 30, 0, 4 << 30},
		{"one half-life", 1 << 30, EstimateHalfLife, 2 << 30},
		{"two half-lives", 0, EstimateHalfLife, 1 << 30},
		{"growth replaces at once", 3 << 30, time.Minute, 3 << 30},
	} {
		now = now.Add(c.age)
		// The estimate follows the largest attempt, not the average.
		host(t, a.Pool.Dir, mem{}, mem{anon: c.sample}, mem{anon: c.sample / 4})
		a.Observe(2, now)
		if got := a.observed(now); got != c.estimate {
			t.Fatalf("%s: estimate %s, want %s", c.name, Size(got), Size(c.estimate))
		}
	}
}

func TestAutoAdmissionFloorFollowsAttemptCeiling(t *testing.T) {
	for _, c := range []struct {
		name          string
		high, ceiling int64
		floor         int64
		workingSet    int64
		admitted      bool
	}{
		{"small pool, no ceiling", 8 << 30, 0, 2 << 30, 4 << 30, true},
		{"small pool, large ceiling", 8 << 30, 6 << 30, 2 << 30, 5 << 30, false},
		{"small pool, small ceiling", 8 << 30, 1 << 30, 1 << 30, 5 << 30, true},
		{"tiny pool", 1 << 30, 0, 512 << 20, 0, true},
		{"large pool, no ceiling", 54 << 30, 0, 4 << 30, 40 << 30, true},
		{"large pool, ceiling", 54 << 30, 6 << 30, 6 << 30, 40 << 30, true},
		{"large pool, ceiling without headroom", 54 << 30, 6 << 30, 6 << 30, 42 << 30, false},
		{"large pool, minimal ceiling", 54 << 30, 64 << 20, 512 << 20, 47 << 30, true},
	} {
		a := autoPool(t)
		a.AttemptMax = c.ceiling
		write(t, a.Pool.Dir, map[string]string{"memory.high": strconv.FormatInt(c.high, 10), "memory.current": strconv.FormatInt(c.workingSet, 10)})
		if got := a.floor(c.high); got != c.floor {
			t.Fatalf("%s: floor %s, want %s", c.name, Size(got), Size(c.floor))
		}
		if ok, reason := a.Admit(1, time.Now().Add(ColdStartInterval)); ok != c.admitted {
			t.Fatalf("%s: admitted %v: %s", c.name, ok, reason)
		}
	}
}

func TestAutoAdmissionDiskHysteresisAndReadErrors(t *testing.T) {
	a := autoPool(t)
	d := Disk{Total: 100 << 30, Available: 9 << 30, Files: 1000, FilesFree: 900}
	a.ReadDisk = func(string) (Disk, error) { return d, nil }
	now := time.Now()
	if ok, _ := a.Admit(0, now); ok {
		t.Fatal("low disk admitted")
	}
	d.Available = 12 << 30
	if ok, _ := a.Admit(0, now); ok {
		t.Fatal("disk gate flapped below recovery watermark")
	}
	d.Available = 16 << 30
	if ok, reason := a.Admit(0, now); !ok {
		t.Fatal(reason)
	}
	d.FilesFree = 40
	if ok, _ := a.Admit(0, now); ok {
		t.Fatal("inode exhaustion admitted")
	}
	d.FilesFree = 70
	if ok, _ := a.Admit(0, now); ok {
		t.Fatal("inode gate flapped")
	}
	d.FilesFree = 100
	if ok, reason := a.Admit(0, now); !ok {
		t.Fatal(reason)
	}
	a.ReadDisk = func(string) (Disk, error) { return Disk{}, errors.New("missing mount") }
	if ok, _ := a.Admit(0, now); ok {
		t.Fatal("missing disk mount admitted")
	}
}

func TestReadDisk(t *testing.T) {
	if d, err := ReadDisk(t.TempDir()); err != nil || d.Total == 0 || d.Available > d.Total {
		t.Fatal(d, err)
	}
	if _, err := ReadDisk(t.TempDir() + "/missing"); err == nil {
		t.Fatal("missing path accepted")
	}
}

// mem is one cgroup's charged memory. cache is file cache without shmem, inactive
// its inactive part; slabReclaimable is part of kernel. memory.stat counts shmem in
// file, and memory.current is anon + file + kernel.
type mem struct{ anon, shmem, cache, inactive, kernel, slabReclaimable int64 }

func (m mem) write(t *testing.T, dir string) {
	t.Helper()
	write(t, dir, map[string]string{"memory.current": strconv.FormatInt(m.anon+m.shmem+m.cache+m.kernel, 10) + "\n",
		"memory.stat": "anon " + strconv.FormatInt(m.anon, 10) + "\nfile " + strconv.FormatInt(m.cache+m.shmem, 10) +
			"\nkernel " + strconv.FormatInt(m.kernel, 10) + "\nshmem " + strconv.FormatInt(m.shmem, 10) +
			"\ninactive_file " + strconv.FormatInt(m.inactive, 10) + "\nactive_file " + strconv.FormatInt(m.cache-m.inactive, 10) +
			"\nslab_reclaimable " + strconv.FormatInt(m.slabReclaimable, 10) + "\n"})
}

// host writes the vps-worker slice (ECO-149): what finished attempts left charged to
// it plus each running attempt, whose cgroups sit below it as Docker creates them.
func host(t *testing.T, dir string, left mem, attempts ...mem) {
	t.Helper()
	children, _ := filepath.Glob(filepath.Join(dir, "docker-*.scope"))
	for _, child := range children {
		if err := os.RemoveAll(child); err != nil {
			t.Fatal(err)
		}
	}
	total := left
	for i, m := range attempts {
		m.write(t, filepath.Join(dir, "docker-"+strings.Repeat(strconv.Itoa(i), 64)+".scope"))
		total = mem{total.anon + m.anon, total.shmem + m.shmem, total.cache + m.cache, total.inactive + m.inactive,
			total.kernel + m.kernel, total.slabReclaimable + m.slabReclaimable}
	}
	total.write(t, dir)
}

// idle is the vps-worker slice with no attempt: 2.5 GiB of page cache and kernel
// memory from earlier attempts and their nested engines, no anon or shmem.
var idle = mem{cache: 2661728256, inactive: 1910177792, kernel: 198586368}

func hostAdmission(t *testing.T) *AutoAdmission {
	a := autoPool(t)
	write(t, a.Pool.Dir, map[string]string{"memory.high": "5252317184\n", "memory.max": "6179258368\n"})
	host(t, a.Pool.Dir, idle)
	return a
}

// A coding attempt: under 1 GiB of process memory, more in its own page cache
// (repository, dependencies, nested engine images).
var coding = mem{anon: 900 << 20, cache: 1200 << 20, inactive: 200 << 20, kernel: 120 << 20}

func TestAutoAdmissionIgnoresReclaimablePageCache(t *testing.T) {
	a := hostAdmission(t)
	start := time.Now()
	if ok, reason := a.Admit(0, start); !ok {
		t.Fatal(reason)
	}
	a.Started(start)
	host(t, a.Pool.Dir, idle, coding)
	if ok, reason := a.Admit(1, start.Add(ColdStartInterval-time.Second)); ok || reason != Warmup {
		t.Fatal("second attempt skipped warmup:", reason)
	}
	second := start.Add(ColdStartInterval)
	a.Observe(1, second)
	if ok, reason := a.Admit(1, second); !ok {
		t.Fatal("second attempt refused with real headroom:", reason)
	}
	a.Started(second)
	// Reaching memory.high the kernel reclaims the stale cache first.
	host(t, a.Pool.Dir, mem{cache: 300 << 20, inactive: 200 << 20, kernel: 185 << 20}, coding, coding)
	third := second.Add(ColdStartInterval)
	a.Observe(2, third)
	if ok, _ := a.Admit(2, third.Add(-time.Second)); ok {
		t.Fatal("third attempt skipped warmup")
	}
	if ok, reason := a.Admit(2, third); !ok {
		t.Fatal("third attempt refused with real headroom:", reason)
	}
	a.ReadDisk = func(string) (Disk, error) {
		return Disk{Total: 100 << 30, Available: 5 << 30, Files: 1000, FilesFree: 900}, nil
	}
	if ok, _ := a.Admit(2, third); ok {
		t.Fatal("low disk admitted")
	}
}

func TestAutoAdmissionRefusesUnreclaimableShortage(t *testing.T) {
	for name, attempts := range map[string][]mem{
		"large build":     {{anon: 2600 << 20, cache: 300 << 20, kernel: 150 << 20}},
		"tmpfs in shmem":  {{anon: 400 << 20, shmem: 2200 << 20, cache: 300 << 20, kernel: 100 << 20}},
		"two builds":      {coding, {anon: 1700 << 20, cache: 200 << 20, kernel: 120 << 20}},
		"kernel not slab": {{anon: 900 << 20, kernel: 2000 << 20, slabReclaimable: 100 << 20}},
	} {
		a := hostAdmission(t)
		host(t, a.Pool.Dir, mem{cache: 200 << 20, kernel: 150 << 20}, attempts...)
		now := time.Now().Add(ColdStartInterval)
		a.Observe(len(attempts), now)
		if ok, reason := a.Admit(len(attempts), now); ok || !strings.Contains(reason, "build headroom below estimate") {
			t.Errorf("%s: admitted %v: %s", name, ok, reason)
		}
	}
}
