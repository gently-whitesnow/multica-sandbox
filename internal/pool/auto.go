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

// Warmup is the reason Admit gives while a cold start establishes its working set.
// It paces starts and is not pool pressure.
const Warmup = "auto admission: cold-start warmup"

// EstimateHalfLife halves an observed per-attempt peak that is no longer seen, so one
// heavy build slows later starts for tens of minutes rather than until restart.
const EstimateHalfLife = 10 * time.Minute

// DefaultAttemptEstimate is the cold build estimate without attempt_memory_max: the
// legacy per-attempt memory limit.
const DefaultAttemptEstimate = 4 << 30

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

// DiskLow reports the claim pause watermark: free space below max(2 GiB, 10%) or
// free inodes below 5%. Session collection also starts there (ADR 0034).
func DiskLow(d Disk) bool {
	return d.Available < max(uint64(2<<30), d.Total/10) || d.Files > 0 && d.FilesFree < d.Files/20
}

// DiskRecovered reports the resume watermark: max(3 GiB, 15%) free space and 10% free inodes.
func DiskRecovered(d Disk) bool {
	return d.Available >= max(uint64(3<<30), d.Total*15/100) && (d.Files == 0 || d.FilesFree >= d.Files/10)
}

// AutoAdmission adds conservative cold-start pacing, observed memory headroom and
// hysteretic disk admission to the pool's existing PSI gate. It never kills work.
// It is used by the single controller claim loop, and is not concurrency-safe.
type AutoAdmission struct {
	Pool     *Pool
	DiskPath string
	ReadDisk func(string) (Disk, error)
	// AttemptMax is the per-attempt memory ceiling (attempt_memory_max), 0 for none.
	AttemptMax        int64
	lastStart, peakAt time.Time
	peak              int64
	diskPaused        bool
}

// Observe samples on every claim poll, also while admission refuses, so busy periods
// contribute to the estimate. The largest attempt cgroup's working set is an estimate,
// not a promise about the next build. Pool MemoryMax remains the final memory bound.
func (a *AutoAdmission) Observe(active int, now time.Time) {
	if active == 0 {
		return
	}
	if sample, err := a.Pool.largestAttempt(); err == nil && sample >= a.observed(now) {
		a.peak, a.peakAt = sample, now
	}
}

// observed is the largest per-attempt sample, halved every EstimateHalfLife since it
// was seen: it follows growth at once and forgets a finished heavy build gradually.
func (a *AutoAdmission) observed(now time.Time) int64 {
	age := max(now.Sub(a.peakAt), 0)
	return int64(float64(a.peak) * math.Exp2(-age.Seconds()/EstimateHalfLife.Seconds()))
}

// floor is the cold estimate of a new attempt: its ceiling, else the legacy default,
// within a quarter of MemoryHigh so small pools still run several attempts.
func (a *AutoAdmission) floor(high int64) int64 {
	estimate := a.AttemptMax
	if estimate <= 0 {
		estimate = DefaultAttemptEstimate
	}
	return max(512<<20, min(estimate, high/4))
}

// Started records one newly claimed attempt. Only one can pass each warmup window.
func (a *AutoAdmission) Started(now time.Time) { a.lastStart = now }

// Admit reports whether a new attempt may start, the bounded refusal reason (empty when
// admitted) and a detail for logs.
func (a *AutoAdmission) Admit(active int, now time.Time) (bool, string, string) {
	read := a.ReadDisk
	if read == nil {
		read = ReadDisk
	}
	d, err := read(a.DiskPath)
	if err != nil || d.Total == 0 {
		return false, RefusalUnreadable, "host Docker filesystem unreadable"
	}
	if DiskLow(d) {
		a.diskPaused = true
	}
	if a.diskPaused && DiskRecovered(d) {
		a.diskPaused = false
	}
	if a.diskPaused {
		return false, RefusalDisk, fmt.Sprintf("host Docker filesystem low: free=%d bytes, free_inodes=%d", d.Available, d.FilesFree)
	}
	if active == 0 {
		return true, "", "auto admission: first attempt"
	}
	if now.Sub(a.lastStart) < ColdStartInterval {
		return false, RefusalWarmup, Warmup
	}
	_, s, err := a.Pool.Admit()
	if err != nil {
		return false, RefusalUnreadable, "auto admission: pool unreadable"
	}
	// Begin at the cold floor, then use the decaying observed peak, with 25% extra.
	// This prevents empty cgroups admitting a cold burst.
	estimate := max(a.floor(s.High), a.observed(now))
	estimate = int64(math.Ceil(float64(estimate) * 1.25))
	if float64(s.High)*a.Pool.MaxWorkingSet-float64(s.WorkingSet) < float64(estimate) {
		return false, RefusalHeadroom, fmt.Sprintf("auto admission: build headroom below estimate %s", Size(estimate))
	}
	return true, "", "auto admission: warm pool with build headroom"
}
