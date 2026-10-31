package service

import (
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/metrics"
	"github.com/gently-whitesnow/multica-sandbox/internal/pool"
)

// DinDConfig configures the nested engine of sysbox-runc attempts (ADR 0019).
type DinDConfig struct {
	// RegistryMirrors are pull-through registries for Docker Hub, reached as peers.
	RegistryMirrors    []string `json:"registry_mirrors,omitempty"`
	InsecureRegistries []string `json:"insecure_registries,omitempty"`
	ReadyTimeout       string   `json:"ready_timeout,omitempty"`
	// TestcontainersRyukDisabled: removing the attempt removes its inner containers.
	TestcontainersRyukDisabled bool `json:"testcontainers_ryuk_disabled,omitempty"`
}

// PoolConfig places attempts in a host cgroup slice with memory limits and swap
// (ADR 0020). CgroupDir is the slice directory mounted read-only into the controller.
type PoolConfig struct {
	CgroupParent string `json:"cgroup_parent"`
	CgroupDir    string `json:"cgroup_dir"`
	// AutoAdmission staggers cold starts and reserves estimated build headroom.
	AutoAdmission bool `json:"auto_admission,omitempty"`
	// DiskPath is a read-only mount of the host Docker/workdir backing filesystem.
	DiskPath string `json:"disk_path,omitempty"`
	// AttemptMemoryMax optionally caps one attempt: a size such as 6g or a percentage
	// of the pool's memory.max such as 75%.
	AttemptMemoryMax string `json:"attempt_memory_max,omitempty"`
	// Claims wait while PSI "some" avg10 exceeds MaxPressure percent (default 10) or the
	// working set exceeds MaxWorkingSet of memory.high (default 0.9).
	MaxPressure   float64 `json:"max_pressure,omitempty"`
	MaxWorkingSet float64 `json:"max_working_set,omitempty"`
}

var registryHost = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,252})(:[0-9]{1,5})?$`)

// attemptRuntime validates the runtime and its nested engine configuration.
func attemptRuntime(c *OpenCodeConfig) (string, docker.DinD, error) {
	var dind docker.DinD
	switch c.Runtime {
	case "", "runc":
		if c.DinD != nil {
			return "", dind, fmt.Errorf("dind requires runtime sysbox-runc")
		}
		return "", dind, nil
	case docker.Sysbox:
	default:
		return "", dind, fmt.Errorf("runtime must be runc or sysbox-runc")
	}
	if c.Helper == "" {
		return "", dind, fmt.Errorf("runtime sysbox-runc requires the helper")
	}
	d := c.DinD
	if d == nil {
		d = &DinDConfig{}
	}
	if len(d.RegistryMirrors) > 8 || len(d.InsecureRegistries) > 8 {
		return "", dind, fmt.Errorf("dind allows at most 8 registry mirrors and insecure registries")
	}
	for _, mirror := range d.RegistryMirrors {
		u, err := url.Parse(mirror)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return "", dind, fmt.Errorf("dind registry mirrors must be http(s) URLs without path or credentials")
		}
	}
	for _, registry := range d.InsecureRegistries {
		if _, err := netip.ParsePrefix(registry); err != nil && !registryHost.MatchString(registry) {
			return "", dind, fmt.Errorf("dind insecure registries must be host[:port] or CIDR")
		}
	}
	timeout, err := optionalDuration(d.ReadyTimeout, 30*time.Second)
	if err != nil || timeout < time.Second || timeout > 5*time.Minute {
		return "", dind, fmt.Errorf("dind ready_timeout must be within [1s,5m]")
	}
	return docker.Sysbox, docker.DinD{RegistryMirrors: d.RegistryMirrors, InsecureRegistries: d.InsecureRegistries, ReadyTimeout: timeout, RyukDisabled: d.TestcontainersRyukDisabled}, nil
}

// attemptPool resolves the pool and moves resources into it; per-attempt memory is
// then only the optional ceiling.
func attemptPool(c *OpenCodeConfig, resources docker.Resources) (*pool.Pool, docker.Resources, error) {
	p := c.Pool
	if c.Resources != nil && c.Resources.Memory != "" {
		return nil, resources, fmt.Errorf("resources.memory conflicts with pool: use pool.attempt_memory_max")
	}
	if p.CgroupParent == "" || !filepath.IsAbs(p.CgroupDir) || filepath.Clean(p.CgroupDir) != p.CgroupDir {
		return nil, resources, fmt.Errorf("pool requires cgroup_parent and an absolute cgroup_dir")
	}
	if p.AutoAdmission && (!filepath.IsAbs(p.DiskPath) || filepath.Clean(p.DiskPath) != p.DiskPath) {
		return nil, resources, fmt.Errorf("auto admission requires an absolute disk_path")
	}
	if p.DiskPath != "" && !p.AutoAdmission {
		return nil, resources, fmt.Errorf("disk_path requires auto_admission")
	}
	attempts := &pool.Pool{Parent: p.CgroupParent, Dir: p.CgroupDir, MaxPressure: p.MaxPressure, MaxWorkingSet: p.MaxWorkingSet}
	if attempts.MaxPressure == 0 {
		attempts.MaxPressure = pool.DefaultMaxPressure
	}
	if attempts.MaxWorkingSet == 0 {
		attempts.MaxWorkingSet = pool.DefaultMaxWorkingSet
	}
	if attempts.MaxPressure < 0 || attempts.MaxPressure > 100 || attempts.MaxWorkingSet < 0 || attempts.MaxWorkingSet > 1 {
		return nil, resources, fmt.Errorf("pool max_pressure must be within (0,100] and max_working_set within (0,1]")
	}
	var ceiling int64
	if percent, ok := strings.CutSuffix(p.AttemptMemoryMax, "%"); ok {
		n, err := strconv.Atoi(percent)
		limits, limitsErr := attempts.Limits()
		if err != nil || n < 10 || n > 100 || limitsErr != nil {
			return nil, resources, fmt.Errorf("pool attempt_memory_max must be 10%%-100%% of a bounded pool")
		}
		ceiling = limits.Max * int64(n) / 100
	} else if p.AttemptMemoryMax != "" {
		n, err := docker.ParseSize(p.AttemptMemoryMax)
		if err != nil || n < 64<<20 {
			return nil, resources, fmt.Errorf("pool attempt_memory_max must be a size of at least 64m or a percentage")
		}
		ceiling = n
	}
	// The kernel rounds memory.max down to pages; a MiB multiple is exact for every page size.
	ceiling &^= 1<<20 - 1
	return attempts, resources.InPool(p.CgroupParent, ceiling), nil
}

// admission defers claims while the pool is under memory pressure and records each
// reading in m; without a pool every claim is admitted.
func admission(p *pool.Pool, m *metrics.Metrics) func() (bool, string) {
	if p == nil {
		return nil
	}
	return func() (bool, string) {
		ok, status, err := p.Admit()
		if err != nil {
			return false, "attempt pool unreadable: " + err.Error()
		}
		m.Pool(metrics.PoolReading{Pressure: status.SomeAvg10 / 100, Usage: status.WorkingSet, Limit: status.High})
		return ok, status.String()
	}
}
