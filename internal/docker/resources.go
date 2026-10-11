package docker

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Resources bound one attempt container; policy compares the created container with them.
type Resources struct {
	NanoCPUs                    int64
	Memory, Workspace, Tmp, Shm int64
	Pids, NoFile                int
}

// DefaultResources fit real builds (compilers, test runners); the upstream daemon sets none.
var DefaultResources = Resources{NanoCPUs: 2e9, Memory: 4 << 30, Pids: 1024, NoFile: 4096, Workspace: 1 << 30, Tmp: 256 << 20, Shm: 64 << 20}

// probeResources bound the lifecycle probe's test executions.
var probeResources = Resources{NanoCPUs: 5e8, Memory: 128 << 20, Pids: 64, NoFile: 256, Workspace: 64 << 20, Tmp: 16 << 20, Shm: 8 << 20}

// TmpDir is the attempt TMPDIR: executable, unlike /tmp, and inside the bounded /workspace.
const TmpDir = "/workspace/tmp"

// ResourceSpec is the configured form; empty fields keep the defaults.
type ResourceSpec struct {
	CPUs          float64 `json:"cpus,omitempty"`
	Memory        string  `json:"memory,omitempty"`
	Pids          int     `json:"pids,omitempty"`
	NoFile        int     `json:"nofile,omitempty"`
	WorkspaceSize string  `json:"workspace_size,omitempty"`
	TmpSize       string  `json:"tmp_size,omitempty"`
	ShmSize       string  `json:"shm_size,omitempty"`
}

// Resolve applies a spec over DefaultResources within sane bounds.
func (s *ResourceSpec) Resolve() (Resources, error) {
	r := DefaultResources
	if s == nil {
		return r, nil
	}
	if s.CPUs != 0 {
		if s.CPUs < 0.1 || s.CPUs > 256 {
			return r, fmt.Errorf("resources.cpus must be in [0.1,256]")
		}
		r.NanoCPUs = int64(math.Round(s.CPUs * 1e9))
	}
	for _, size := range []struct {
		name, value string
		min, max    int64
		target      *int64
	}{
		{"memory", s.Memory, 64 << 20, 1 << 40, &r.Memory},
		{"workspace_size", s.WorkspaceSize, 64 << 20, 1 << 40, &r.Workspace},
		{"tmp_size", s.TmpSize, 1 << 20, 1 << 40, &r.Tmp},
		{"shm_size", s.ShmSize, 1 << 20, 1 << 40, &r.Shm},
	} {
		if size.value == "" {
			continue
		}
		n, err := parseSize(size.value)
		if err != nil || n < size.min || n > size.max {
			return r, fmt.Errorf("resources.%s must be a size such as 512m or 4g within bounds", size.name)
		}
		*size.target = n
	}
	if s.Pids != 0 {
		if s.Pids < 64 || s.Pids > 1<<20 {
			return r, fmt.Errorf("resources.pids must be in [64,1048576]")
		}
		r.Pids = s.Pids
	}
	if s.NoFile != 0 {
		if s.NoFile < 256 || s.NoFile > 1<<20 {
			return r, fmt.Errorf("resources.nofile must be in [256,1048576]")
		}
		r.NoFile = s.NoFile
	}
	return r, nil
}

// parseSize reads Docker's binary size units: a byte count with an optional b, k, m or g suffix.
func parseSize(value string) (int64, error) {
	units := map[byte]int64{'b': 1, 'k': 1 << 10, 'm': 1 << 20, 'g': 1 << 30}
	number, unit := strings.ToLower(value), int64(1)
	if n := len(number); n > 0 && units[number[n-1]] != 0 {
		number, unit = number[:n-1], units[number[n-1]]
	}
	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil || n <= 0 || n > math.MaxInt64/unit {
		return 0, fmt.Errorf("invalid size")
	}
	return n * unit, nil
}

func (r Resources) workspaceTmpfs() string {
	return "rw,exec,nosuid,nodev,size=" + strconv.FormatInt(r.Workspace, 10) + ",mode=1777"
}
func (r Resources) tmpTmpfs() string {
	return "rw,noexec,nosuid,nodev,size=" + strconv.FormatInt(r.Tmp, 10) + ",mode=1777"
}

func (r Resources) args() []string {
	memory := strconv.FormatInt(r.Memory, 10)
	return []string{"--memory=" + memory, "--memory-swap=" + memory, "--cpus=" + strconv.FormatFloat(float64(r.NanoCPUs)/1e9, 'f', -1, 64),
		"--pids-limit=" + strconv.Itoa(r.Pids), fmt.Sprintf("--ulimit=nofile=%d:%d", r.NoFile, r.NoFile),
		"--tmpfs=/workspace:" + r.workspaceTmpfs(), "--tmpfs=/tmp:" + r.tmpTmpfs(), "--shm-size=" + strconv.FormatInt(r.Shm, 10)}
}

type ulimit struct {
	Name       string
	Soft, Hard int64
}

// matches reports whether inspected host limits are exactly these resources.
func (r Resources) matches(memory, swap, cpus, shm int64, pids int, tmpfs map[string]string, ulimits []ulimit) bool {
	return memory == r.Memory && swap == r.Memory && cpus == r.NanoCPUs && shm == r.Shm && pids == r.Pids &&
		len(tmpfs) == 2 && tmpfs["/workspace"] == r.workspaceTmpfs() && tmpfs["/tmp"] == r.tmpTmpfs() &&
		len(ulimits) == 1 && ulimits[0] == ulimit{"nofile", int64(r.NoFile), int64(r.NoFile)}
}
