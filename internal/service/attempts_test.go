package service

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/metrics"
)

func TestAttemptRuntimeConfiguration(t *testing.T) {
	helper := "example.invalid/helper@sha256:" + strings.Repeat("b", 64)
	if runtime, _, err := attemptRuntime(&OpenCodeConfig{}); runtime != "" || err != nil {
		t.Fatal("runc is not the default", runtime, err)
	}
	runtime, dind, err := attemptRuntime(&OpenCodeConfig{Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{RegistryMirrors: []string{"http://mirror:5000"}, InsecureRegistries: []string{"mirror:5000", "10.0.0.0/8"}, TestcontainersRyukDisabled: true}})
	if runtime != docker.Sysbox || err != nil || dind.ReadyTimeout != 30*time.Second || !dind.RyukDisabled || len(dind.InsecureRegistries) != 2 {
		t.Fatal(runtime, dind, err)
	}
	for name, c := range map[string]OpenCodeConfig{
		"unknown runtime":   {Runtime: "kata", Helper: helper},
		"dind under runc":   {DinD: &DinDConfig{}},
		"sysbox no helper":  {Runtime: "sysbox-runc"},
		"mirror path":       {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{RegistryMirrors: []string{"http://mirror:5000/v2"}}},
		"mirror userinfo":   {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{RegistryMirrors: []string{"https://u:p@mirror"}}},
		"mirror scheme":     {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{RegistryMirrors: []string{"mirror:5000"}}},
		"insecure flag":     {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{InsecureRegistries: []string{"--debug"}}},
		"insecure spaces":   {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{InsecureRegistries: []string{"a b"}}},
		"short timeout":     {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{ReadyTimeout: "10ms"}},
		"too many mirrors":  {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{RegistryMirrors: strings.Fields(strings.Repeat("http://m ", 9))}},
		"negative timeout":  {Runtime: "sysbox-runc", Helper: helper, DinD: &DinDConfig{ReadyTimeout: "-1s"}},
		"runtime case":      {Runtime: "Sysbox-runc", Helper: helper},
		"dind with no flag": {Runtime: "", DinD: &DinDConfig{TestcontainersRyukDisabled: true}},
	} {
		if _, _, err := attemptRuntime(&c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Sysbox defaults fit a nested engine; configured values still apply.
	workloads, err := attemptWorkloads(&OpenCodeConfig{Runtime: "sysbox-runc", Helper: helper, Resources: &docker.ResourceSpec{CPUs: 4}}, &docker.Backend{})
	if err != nil || workloads.Runtime != docker.Sysbox || workloads.Resources.Pids != 2048 || workloads.Resources.NoFile != 65536 || workloads.Resources.NanoCPUs != 4e9 {
		t.Fatal(workloads, err)
	}
}

// fakeSlice writes a pool with 10 GiB memory.max and the given pressure.
func fakeSlice(t *testing.T, avg10 string) string {
	dir := t.TempDir()
	for name, content := range map[string]string{"memory.high": "8589934592", "memory.max": "10737418240", "memory.swap.max": "max", "memory.current": "1073741824",
		"memory.stat": "inactive_file 0\n", "memory.pressure": "some avg10=" + avg10 + " avg60=0 avg300=0 total=0\nfull avg10=0 avg60=0 avg300=0 total=0\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestAttemptPoolConfiguration(t *testing.T) {
	dir := fakeSlice(t, "0.00")
	base, _ := (*docker.ResourceSpec)(nil).Resolve(docker.Sysbox)
	p, r, err := attemptPool(&OpenCodeConfig{Pool: &PoolConfig{CgroupParent: "multica_attempts.slice", CgroupDir: dir}}, base)
	if err != nil || r.Pool != "multica_attempts.slice" || r.Memory != 0 || p.MaxPressure != 10 || p.MaxWorkingSet != 0.9 {
		t.Fatal(p, r, err)
	}
	if _, r, err = attemptPool(&OpenCodeConfig{Pool: &PoolConfig{CgroupParent: "multica_attempts.slice", CgroupDir: dir, AttemptMemoryMax: "75%"}}, base); err != nil || r.Memory != 10737418240*75/100&^(1<<20-1) {
		t.Fatal("percentage ceiling", r.Memory, err)
	}
	if _, r, err = attemptPool(&OpenCodeConfig{Pool: &PoolConfig{CgroupParent: "multica_attempts.slice", CgroupDir: dir, AttemptMemoryMax: "6g"}}, base); err != nil || r.Memory != 6<<30 {
		t.Fatal("size ceiling", r.Memory, err)
	}
	// Ceilings land on MiB boundaries, which the kernel keeps exactly in memory.max.
	for ceiling, want := range map[string]int64{"25%": 2684354560, "33%": 3543138304, "1000001k": 976 << 20} {
		if _, r, err = attemptPool(&OpenCodeConfig{Pool: &PoolConfig{CgroupParent: "multica_attempts.slice", CgroupDir: dir, AttemptMemoryMax: ceiling}}, base); err != nil || r.Memory != want {
			t.Errorf("%s: %d %v", ceiling, r.Memory, err)
		}
	}
	for name, c := range map[string]OpenCodeConfig{
		"memory and pool": {Resources: &docker.ResourceSpec{Memory: "4g"}, Pool: &PoolConfig{CgroupParent: "p.slice", CgroupDir: dir}},
		"no parent":       {Pool: &PoolConfig{CgroupDir: dir}},
		"relative dir":    {Pool: &PoolConfig{CgroupParent: "p.slice", CgroupDir: "pool"}},
		"unclean dir":     {Pool: &PoolConfig{CgroupParent: "p.slice", CgroupDir: dir + "/../x"}},
		"tiny percentage": {Pool: &PoolConfig{CgroupParent: "p.slice", CgroupDir: dir, AttemptMemoryMax: "5%"}},
		"over 100%":       {Pool: &PoolConfig{CgroupParent: "p.slice", CgroupDir: dir, AttemptMemoryMax: "150%"}},
		"unbounded %":     {Pool: &PoolConfig{CgroupParent: "p.slice", CgroupDir: t.TempDir(), AttemptMemoryMax: "75%"}},
		"tiny ceiling":    {Pool: &PoolConfig{CgroupParent: "p.slice", CgroupDir: dir, AttemptMemoryMax: "1m"}},
		"pressure":        {Pool: &PoolConfig{CgroupParent: "p.slice", CgroupDir: dir, MaxPressure: 101}},
		"working set":     {Pool: &PoolConfig{CgroupParent: "p.slice", CgroupDir: dir, MaxWorkingSet: 1.5}},
	} {
		if _, _, err := attemptPool(&c, base); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestClaimsWaitWhilePoolIsUnderPressure(t *testing.T) {
	dir := fakeSlice(t, "42.00")
	p, _, err := attemptPool(&OpenCodeConfig{Pool: &PoolConfig{CgroupParent: "multica_attempts.slice", CgroupDir: dir}}, docker.DefaultResources)
	if err != nil {
		t.Fatal(err)
	}
	a := &fleetFake{queued: map[string]int{"a": 3}}
	f := newFleet(t, a)
	var log bytes.Buffer
	m, _ := metrics.New()
	f.log, f.admit, f.metrics, f.ready = newLogger(&log, false), admission(p, m), m, map[string]string{"a": "a"}
	execute, _, release := track()
	defer close(release)
	for range 3 {
		if err := f.claim(context.Background(), 10, execute); err != nil {
			t.Fatal(err)
		}
	}
	if a.claims != 0 || len(f.active) != 0 || strings.Count(log.String(), `"msg":"claims paused","pool":"memory pressure some avg10=42.00%`) != 1 {
		t.Fatalf("claimed under pressure: claims=%d log=%q", a.claims, log.String())
	}
	if text := exposition(m); !strings.Contains(text, "sandbox_claims_paused 1") || !strings.Contains(text, "sandbox_pool_memory_pressure_ratio 0.42") {
		t.Fatalf("pause not exported:\n%s", text)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.pressure"), []byte("some avg10=1.00 avg60=0 avg300=0 total=0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A calm reading resumes claims only after the cooldown.
	now := time.Now()
	f.clock = func() time.Time { return now }
	if err := f.claim(context.Background(), 10, execute); err != nil || len(f.active) != 0 {
		t.Fatalf("claimed within the cooldown: %v %d", err, len(f.active))
	}
	now = now.Add(admissionCooldown)
	if err := f.claim(context.Background(), 10, execute); err != nil {
		t.Fatal(err)
	}
	if len(f.active) != 3 || strings.Count(log.String(), `"msg":"claims resumed"`) != 1 || strings.Count(log.String(), `"msg":"claims paused"`) != 1 {
		t.Fatalf("claims did not resume: %d active, log %q", len(f.active), log.String())
	}
	if text := exposition(m); !strings.Contains(text, "sandbox_claims_paused 0") || !strings.Contains(text, "sandbox_attempt_active 3") {
		t.Fatalf("resume not exported:\n%s", text)
	}
	if err := os.Remove(filepath.Join(dir, "memory.pressure")); err != nil {
		t.Fatal(err)
	}
	a.queued["a"] = 1
	if err := f.claim(context.Background(), 10, execute); err != nil || len(f.active) != 3 || !strings.Contains(log.String(), "attempt pool unreadable") {
		t.Fatalf("unreadable pool admitted: %v %d", err, len(f.active))
	}
}
