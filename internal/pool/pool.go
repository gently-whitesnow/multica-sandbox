// Package pool reads the host cgroup slice that attempts share (ADR 0020): its memory
// limits, its pressure for claim admission and the memory events of attempt cgroups.
package pool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Unlimited is a cgroup "max" value.
const Unlimited = -1

// Default admission thresholds: PSI "some" avg10 in percent and the working set as a
// fraction of memory.high.
const (
	DefaultMaxPressure   = 10
	DefaultMaxWorkingSet = 0.9
)

// Pool is the slice attempts run in. Dir is the slice directory as the controller
// sees it, normally a read-only bind mount of the host cgroup.
type Pool struct {
	Parent, Dir                string
	MaxPressure, MaxWorkingSet float64
}

var slicePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}\.slice$`)
var pathPattern = regexp.MustCompile(`^(/[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}){1,8}$`)

// ValidParent accepts a systemd slice name for the systemd cgroup driver and an
// absolute cgroup path for cgroupfs, as Docker's --cgroup-parent does.
func ValidParent(parent, driver string) error {
	if (driver == "systemd" && slicePattern.MatchString(parent)) || (driver == "cgroupfs" && pathPattern.MatchString(parent)) {
		return nil
	}
	return fmt.Errorf("pool.cgroup_parent %.64q does not fit the %.16s cgroup driver", parent, driver)
}

// Limits are the slice's memory.high, memory.max and memory.swap.max.
type Limits struct{ High, Max, SwapMax int64 }

// Limits requires a bounded pool: memory.high and memory.max set, high within max.
func (p *Pool) Limits() (Limits, error) {
	var l Limits
	var err error
	for _, f := range []struct {
		name   string
		target *int64
	}{{"memory.high", &l.High}, {"memory.max", &l.Max}, {"memory.swap.max", &l.SwapMax}} {
		if *f.target, err = readValue(filepath.Join(p.Dir, f.name)); err != nil {
			return l, fmt.Errorf("attempt pool %s: %w", f.name, err)
		}
	}
	if l.High == Unlimited || l.Max == Unlimited || l.High > l.Max {
		return l, fmt.Errorf("attempt pool needs MemoryHigh and MemoryMax with MemoryHigh <= MemoryMax")
	}
	return l, nil
}

// Status is the pool state admission decides on.
type Status struct {
	SomeAvg10            float64
	WorkingSet, High     int64
	Pressured, Saturated bool
}

func (s Status) String() string {
	return fmt.Sprintf("memory pressure some avg10=%.2f%%, working set %s of memory.high %s", s.SomeAvg10, Size(s.WorkingSet), Size(s.High))
}

// Admit reports whether a new attempt may start: PSI and the working set (memory.current
// without inactive file cache) stay under the thresholds. Unreadable files refuse.
func (p *Pool) Admit() (bool, Status, error) {
	var s Status
	data, err := os.ReadFile(filepath.Join(p.Dir, "memory.pressure"))
	if err != nil {
		return false, s, err
	}
	if s.SomeAvg10, err = someAvg10(string(data)); err != nil {
		return false, s, err
	}
	current, err := readValue(filepath.Join(p.Dir, "memory.current"))
	if err != nil {
		return false, s, err
	}
	stat, err := os.ReadFile(filepath.Join(p.Dir, "memory.stat"))
	if err != nil {
		return false, s, err
	}
	if s.High, err = readValue(filepath.Join(p.Dir, "memory.high")); err != nil {
		return false, s, err
	}
	s.WorkingSet = max(current-keyed(string(stat))["inactive_file"], 0)
	s.Pressured = s.SomeAvg10 > p.MaxPressure
	s.Saturated = s.High != Unlimited && float64(s.WorkingSet) > p.MaxWorkingSet*float64(s.High)
	return !s.Pressured && !s.Saturated, s, nil
}

// Attempt returns the cgroup directory of a container in the pool: docker-<id>.scope
// under the systemd driver, <id> under cgroupfs.
func (p *Pool) Attempt(id string) (string, error) {
	for _, name := range []string{"docker-" + id + ".scope", id} {
		dir := filepath.Join(p.Dir, name)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, nil
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("attempt cgroup is not in the pool")
}

// CheckAttempt verifies that a started container is in the pool with the expected
// memory.max (ceiling, 0 for none) and unlimited swap within the pool.
func (p *Pool) CheckAttempt(id string, ceiling int64) error {
	dir, err := p.Attempt(id)
	if err != nil {
		return err
	}
	limit, err := readValue(filepath.Join(dir, "memory.max"))
	if err != nil {
		return err
	}
	swap, err := readValue(filepath.Join(dir, "memory.swap.max"))
	if err != nil {
		return err
	}
	if ceiling == 0 {
		ceiling = Unlimited
	}
	if limit != ceiling || swap != Unlimited {
		return fmt.Errorf("attempt cgroup limits differ from the pool configuration")
	}
	return nil
}

// AttemptMemory reads the memory state of a container in the pool.
func (p *Pool) AttemptMemory(id string) (Memory, error) {
	dir, err := p.Attempt(id)
	if err != nil {
		return Memory{}, err
	}
	return ReadMemory(dir)
}

// Memory is the state of an attempt cgroup: memory.events oom (limit reached and
// allocation failed) and oom_kill (processes killed), memory.max and memory.peak (-1
// before Linux 5.19, which added it).
type Memory struct {
	OOM, OOMKill, Peak, Max int64
}

// ReadMemory reads an attempt cgroup directory.
func ReadMemory(dir string) (Memory, error) {
	var data []byte
	for _, name := range []string{"memory.events", "memory.max", "memory.peak"} {
		part, err := os.ReadFile(filepath.Join(dir, name))
		if name == "memory.peak" && errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return Memory{}, err
		}
		data = append(data, part...)
	}
	return ParseMemory(data)
}

// ParseMemory reads memory.events, memory.max and the optional memory.peak
// concatenated, as `cat` prints them inside the attempt.
func ParseMemory(data []byte) (Memory, error) {
	m := Memory{Peak: -1, Max: -1}
	values := []*int64{&m.Max, &m.Peak}
	events := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 2:
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return m, fmt.Errorf("invalid memory.events")
			}
			switch fields[0] {
			case "oom":
				m.OOM, events = n, events+1
			case "oom_kill":
				m.OOMKill, events = n, events+1
			}
		case len(fields) == 1 && len(values) > 0:
			n, err := parseValue(fields[0])
			if err != nil {
				return m, err
			}
			*values[0], values = n, values[1:]
		default:
			return m, fmt.Errorf("invalid attempt memory state")
		}
	}
	if events != 2 || len(values) == 2 {
		return m, fmt.Errorf("incomplete attempt memory state")
	}
	return m, nil
}

func readValue(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return parseValue(strings.TrimSpace(string(data)))
}

func parseValue(value string) (int64, error) {
	if value == "max" {
		return Unlimited, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid cgroup value")
	}
	return n, nil
}

func someAvg10(pressure string) (float64, error) {
	for _, line := range strings.Split(pressure, "\n") {
		if rest, ok := strings.CutPrefix(line, "some "); ok {
			for _, field := range strings.Fields(rest) {
				if value, ok := strings.CutPrefix(field, "avg10="); ok {
					return strconv.ParseFloat(value, 64)
				}
			}
		}
	}
	return 0, fmt.Errorf("invalid memory.pressure")
}

func keyed(stat string) map[string]int64 {
	values := map[string]int64{}
	for _, line := range strings.Split(stat, "\n") {
		if key, value, ok := strings.Cut(line, " "); ok {
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				values[key] = n
			}
		}
	}
	return values
}

// Size formats bytes in binary units for operator and task messages.
func Size(n int64) string {
	switch {
	case n == Unlimited:
		return "unlimited"
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	default:
		return fmt.Sprintf("%d MiB", n>>20)
	}
}
