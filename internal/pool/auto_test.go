package pool

import (
	"errors"
	"strconv"
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
	write(t, a.Pool.Dir, map[string]string{"memory.current": "5368709120"})
	a.Observe(1, late)
	if ok, _ := a.Admit(1, late); ok {
		t.Fatal("new build without headroom admitted")
	}
	write(t, a.Pool.Dir, map[string]string{"memory.current": "1073741824"})
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
		write(t, a.Pool.Dir, map[string]string{"memory.current": strconv.FormatInt(2*c.sample, 10)})
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
