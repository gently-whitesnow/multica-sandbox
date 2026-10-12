package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/pool"
)

// checkRuntime verifies a started attempt through an AttemptUser process instead of
// trusting HostConfig.CapDrop, which Sysbox CE does not apply to PID 1 (ADR 0019).
func checkRuntime(ctx context.Context, name string, sysbox bool) error {
	out, err := command(ctx, "exec", "--user", AttemptUser, name, "cat", "/proc/self/status", "/proc/self/uid_map", "/proc/self/mountinfo")
	if err != nil {
		return err
	}
	return runtimeFacts(out, sysbox)
}

// runtimeFacts requires no capabilities and no_new_privs; under sysbox also a user
// namespace mapping root to an unprivileged range, and, with the root filesystem
// read-only, a writable cgroup and engine data (sysbox-mgr --relaxed-read-only).
func runtimeFacts(out []byte, sysbox bool) error {
	status, mounts := map[string]string{}, map[string]string{}
	var uidMap [][]string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) > 0 && strings.HasSuffix(fields[0], ":"):
			if len(fields) == 2 {
				status[strings.TrimSuffix(fields[0], ":")] = fields[1]
			}
		case len(fields) == 3:
			uidMap = append(uidMap, fields)
		case len(fields) >= 10:
			// mountinfo: later entries shadow earlier ones on the same mount point.
			mounts[fields[4]], _, _ = strings.Cut(fields[5], ",")
		}
	}
	for _, capability := range []string{"CapEff", "CapPrm", "CapAmb"} {
		if status[capability] != "0000000000000000" {
			return fmt.Errorf("attempt process holds capabilities (%s)", capability)
		}
	}
	if status["NoNewPrivs"] != "1" || mounts["/"] != "ro" {
		return fmt.Errorf("attempt process lacks no_new_privs or a read-only root")
	}
	if !sysbox {
		return nil
	}
	if len(uidMap) == 0 || uidMap[0][0] != "0" || uidMap[0][1] == "0" {
		return fmt.Errorf("sysbox attempt is not in an unprivileged user namespace")
	}
	if mounts["/sys/fs/cgroup"] != "rw" || mounts["/var/lib/docker"] != "rw" {
		return fmt.Errorf("sysbox attempt lacks a writable cgroup and /var/lib/docker: run sysbox-mgr with --relaxed-read-only")
	}
	return nil
}

// Preflight fails closed before serving: a bounded pool slice matching the cgroup driver
// and, under sysbox, a canary attempt with the real policy and nested engine.
func (b *Projected) Preflight(ctx context.Context) error {
	if b.Pool != nil {
		data, err := command(ctx, "info", "--format", "{{json .CgroupDriver}}")
		var driver string
		if err == nil {
			err = json.Unmarshal(data, &driver)
		}
		if err == nil {
			err = pool.ValidParent(b.limits().Pool, driver)
		}
		if err != nil {
			return err
		}
		limits, err := b.Pool.Limits()
		if err != nil {
			return err
		}
		b.poolMax = limits.Max
	}
	if b.Runtime != Sysbox {
		return nil
	}
	return b.canary(ctx)
}

// canary starts an offline attempt under sysbox and removes it.
func (b *Projected) canary(ctx context.Context) error {
	if b.Helper == "" {
		return fmt.Errorf("sysbox attempts require the helper")
	}
	bundles, path, err := b.prepare(ctx)
	if err != nil {
		return err
	}
	r := &run{name: b.name("sysbox-canary")}
	_, err = b.launch(ctx, r, expected{network: "none", limits: b.limits(), bundles: b.mounts(), path: path, runtime: Sysbox}, bundles)
	cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err != nil {
		err = fmt.Errorf("sysbox canary: %w", err)
	}
	return errors.Join(err, r.Remove(cleanCtx))
}
