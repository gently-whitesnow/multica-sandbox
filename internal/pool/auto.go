package pool

import (
	"fmt"
	"math"
	"syscall"
	"time"
)

// ColdStartInterval lets compilers and nested engines establish their working set
// before another claim. The controller still owns no queue: work waits in Multica.
const ColdStartInterval = 30 * time.Second

// Disk describes the host filesystem backing Docker and attempt volumes.
type Disk struct{ Total, Available, Files, FilesFree uint64 }

// ReadDisk uses available blocks (not root's reserved blocks). Mount the actual
// Docker backing directory into the controller, not a tmpfs or its image rootfs.
func ReadDisk(path string) (Disk, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return Disk{}, err
	}
	return Disk{uint64(s.Blocks) * uint64(s.Bsize), uint64(s.Bavail) * uint64(s.Bsize), uint64(s.Files), uint64(s.Ffree)}, nil
}

// AutoAdmission adds conservative cold-start pacing, observed memory headroom and
// hysteretic disk admission to the pool's existing PSI gate. It never kills work.
// It is used by the single controller claim loop, and is not concurrency-safe.
type AutoAdmission struct {
	Pool           *Pool
	DiskPath       string
	ReadDisk       func(string) (Disk, error)
	lastStart      time.Time
	peakPerAttempt int64
	diskPaused     bool
}

// Observe samples even while all slots are occupied, so busy periods contribute
// to the estimate. Aggregate working set per active attempt is an estimate, not
// a promise about the next build. Pool MemoryMax remains the final memory bound.
func (a *AutoAdmission) Observe(active int) {
	if active == 0 {
		return
	}
	if _, s, err := a.Pool.Admit(); err == nil {
		a.peakPerAttempt = max(a.peakPerAttempt, s.WorkingSet/int64(active))
	}
}

// Started records one newly claimed attempt. Only one can pass each warmup window.
func (a *AutoAdmission) Started(now time.Time) { a.lastStart = now }

func (a *AutoAdmission) Admit(active int, now time.Time) (bool, string) {
	read := a.ReadDisk
	if read == nil {
		read = ReadDisk
	}
	d, err := read(a.DiskPath)
	if err != nil || d.Total == 0 {
		return false, "host Docker filesystem unreadable"
	}
	low, high := max(uint64(2<<30), d.Total/10), max(uint64(3<<30), d.Total*15/100)
	lowInodes := d.Files > 0 && d.FilesFree < d.Files/20
	recoveredInodes := d.Files == 0 || d.FilesFree >= d.Files/10
	if d.Available < low || lowInodes {
		a.diskPaused = true
	}
	if a.diskPaused && d.Available >= high && recoveredInodes {
		a.diskPaused = false
	}
	if a.diskPaused {
		return false, fmt.Sprintf("host Docker filesystem low: free=%d bytes, free_inodes=%d", d.Available, d.FilesFree)
	}
	if active == 0 {
		return true, "auto admission: first attempt"
	}
	if now.Sub(a.lastStart) < ColdStartInterval {
		return false, "auto admission: cold-start warmup"
	}
	_, s, err := a.Pool.Admit()
	if err != nil {
		return false, "auto admission: pool unreadable"
	}
	// Begin conservatively at a quarter of High, then use the largest observed
	// average with 25% extra. This prevents empty cgroups admitting a cold burst.
	estimate := max(int64(512<<20), s.High/4, a.peakPerAttempt)
	estimate = int64(math.Ceil(float64(estimate) * 1.25))
	if float64(s.High)*a.Pool.MaxWorkingSet-float64(s.WorkingSet) < float64(estimate) {
		return false, fmt.Sprintf("auto admission: build headroom below estimate %s", Size(estimate))
	}
	return true, "auto admission: warm pool with build headroom"
}
