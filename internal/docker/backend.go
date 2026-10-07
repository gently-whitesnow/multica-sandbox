package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
)

const ownerLabel = "io.multica-sandbox.owner"

var imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
var ownerPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

type Backend struct {
	Image   string
	Owner   string
	Command []string
}

func (b *Backend) Validate(ctx context.Context) error {
	if !imagePattern.MatchString(b.Image) || !ownerPattern.MatchString(b.Owner) || len(b.Command) == 0 || !strings.HasPrefix(b.Command[0], "/") {
		return fmt.Errorf("digest-pinned image, stable daemon UUID and absolute executable required")
	}
	data, err := command(ctx, "info", "--format", "{{json .}}")
	if err != nil {
		return err
	}
	var info struct {
		Architecture                                                 string
		OSType, CgroupVersion                                        string
		MemoryLimit, SwapLimit, PidsLimit, CpuCfsPeriod, CpuCfsQuota bool
		SecurityOptions                                              []string
	}
	if err = json.Unmarshal(data, &info); err != nil {
		return err
	}
	if info.OSType != "linux" || info.CgroupVersion != "2" || !info.MemoryLimit || !info.SwapLimit || !info.PidsLimit || !info.CpuCfsPeriod || !info.CpuCfsQuota || !strings.Contains(strings.Join(info.SecurityOptions, ","), "name=seccomp,profile=builtin") {
		return fmt.Errorf("Linux cgroup v2, resource controllers and builtin seccomp required")
	}
	data, err = command(ctx, "image", "inspect", b.Image, "--format", "{{json .}}")
	if err != nil {
		return fmt.Errorf("preload the pinned image: %w", err)
	}
	var image struct {
		Os, Architecture string
		Config           struct{ Volumes map[string]json.RawMessage }
	}
	if err = json.Unmarshal(data, &image); err != nil {
		return err
	}
	if host := engineArchitectures[info.Architecture]; image.Os != "linux" || host == "" || image.Architecture != host {
		return fmt.Errorf("image platform %.16s/%.16s does not match the Docker engine %.16s", image.Os, image.Architecture, info.Architecture)
	}
	if len(image.Config.Volumes) > 0 {
		return fmt.Errorf("image-declared volumes are unsupported")
	}
	return nil
}

// engineArchitectures maps Docker engine (uname) names to OCI platform names; emulation is unsupported.
var engineArchitectures = map[string]string{"x86_64": "amd64", "aarch64": "arm64"}

func (b *Backend) Reconcile(ctx context.Context) error {
	if !ownerPattern.MatchString(b.Owner) {
		return fmt.Errorf("valid daemon owner required for reconciliation")
	}
	data, err := command(ctx, "ps", "-aq", "--filter", "label="+ownerLabel+"="+b.Owner)
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(string(data)) {
		if _, err = command(ctx, "rm", "-fv", id); err != nil {
			return err
		}
	}
	return reconcileNetworks(ctx, b.Owner)
}
func (b *Backend) name(attempt string) string {
	sum := sha256.Sum256([]byte(b.Owner + ":" + attempt))
	return "multica-sandbox-" + hex.EncodeToString(sum[:16])
}
func (b *Backend) Start(ctx context.Context, attempt string) (execution.Run, error) {
	if err := b.Validate(ctx); err != nil {
		return nil, err
	}
	r := &run{name: b.name(attempt)}
	if _, err := command(ctx, b.createArgs(r.name)...); err != nil {
		return nil, errors.Join(err, b.cleanupUncertainCreate(r))
	}
	if err := r.check(ctx); err != nil {
		return nil, errors.Join(err, r.cleanup())
	}
	if _, err := command(ctx, "start", r.name); err != nil {
		return nil, errors.Join(err, r.cleanup())
	}
	return r, nil
}
func (b *Backend) cleanupUncertainCreate(r *run) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := command(ctx, "inspect", "--format", `{{index .Config.Labels "io.multica-sandbox.owner"}}`, r.name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(owner)) != b.Owner {
		return fmt.Errorf("refusing cleanup of a container with a different owner")
	}
	return r.Remove(ctx)
}
func (b *Backend) createArgs(name string) []string {
	args := []string{"create", "--name", name, "--label", ownerLabel + "=" + b.Owner,
		"--pull=never", "--runtime=runc", "--network=none", "--read-only", "--user=65532:65532",
		"--cap-drop=ALL", "--security-opt=no-new-privileges=true", "--cgroupns=private", "--ipc=private",
		"--memory=128m", "--memory-swap=128m", "--cpus=0.5", "--pids-limit=64", "--ulimit=nofile=256:256",
		"--restart=no", "--no-healthcheck", "--log-driver=none", "--workdir=/workspace",
		"--tmpfs=/workspace:rw,nosuid,nodev,size=67108864,mode=1777",
		"--tmpfs=/tmp:rw,noexec,nosuid,nodev,size=16777216,mode=1777", "--shm-size=8m",
		"--env=HOME=/workspace", "--entrypoint", b.Command[0], b.Image}
	return append(args, b.Command[1:]...)
}

type run struct{ name string }

func (r *run) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.Remove(ctx)
}
func (r *run) Remove(ctx context.Context) error {
	_, err := command(ctx, "rm", "-fv", r.name)
	return err
}
func (r *run) Result() execution.Result { return execution.Result{} }

func (r *run) Wait(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := command(ctx, "inspect", "--format", "{{json .State}}", r.name)
		if err != nil {
			return err
		}
		var state struct {
			Status    string
			ExitCode  int
			OOMKilled bool
		}
		if err = json.Unmarshal(data, &state); err != nil {
			return err
		}
		if state.Status == "exited" {
			if state.ExitCode != 0 || state.OOMKilled {
				return fmt.Errorf("execution failed (exit=%d, oom=%t)", state.ExitCode, state.OOMKilled)
			}
			return nil
		}
		if state.Status != "running" {
			return fmt.Errorf("unexpected execution state")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func EngineID(ctx context.Context) (string, error) {
	data, err := command(ctx, "info", "--format", "{{.ID}}")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("Docker engine identity unavailable")
	}
	return id, nil
}
