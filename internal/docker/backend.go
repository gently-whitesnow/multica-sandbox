package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	// Runtime is runc (empty) or Sysbox, which runs a nested engine (ADR 0019).
	Runtime string
}

// Sysbox is the OCI runtime that gives attempts their own user namespace and Docker engine.
const Sysbox = "sysbox-runc"

// AttemptUser runs every agent and controller command inside attempts.
const AttemptUser = "65532:65532"

func (b *Backend) Validate(ctx context.Context) error {
	_, _, err := b.validate(ctx)
	return err
}

type imageConfig struct {
	Os, Architecture string
	Config           struct {
		Env     []string
		Volumes map[string]json.RawMessage
	}
}

// validate checks the host and the pinned image and returns the engine architecture and image configuration.
func (b *Backend) validate(ctx context.Context) (string, imageConfig, error) {
	var image imageConfig
	if !imagePattern.MatchString(b.Image) || !ownerPattern.MatchString(b.Owner) || len(b.Command) == 0 || !strings.HasPrefix(b.Command[0], "/") {
		return "", image, fmt.Errorf("digest-pinned image, stable daemon UUID and absolute executable required")
	}
	data, err := command(ctx, "info", "--format", "{{json .}}")
	if err != nil {
		return "", image, err
	}
	var info struct {
		Architecture                                                 string
		OSType, CgroupVersion                                        string
		MemoryLimit, SwapLimit, PidsLimit, CpuCfsPeriod, CpuCfsQuota bool
		SecurityOptions                                              []string
		Runtimes                                                     map[string]json.RawMessage
	}
	if err = json.Unmarshal(data, &info); err != nil {
		return "", image, err
	}
	if info.OSType != "linux" || info.CgroupVersion != "2" || !info.MemoryLimit || !info.SwapLimit || !info.PidsLimit || !info.CpuCfsPeriod || !info.CpuCfsQuota || !strings.Contains(strings.Join(info.SecurityOptions, ","), "name=seccomp,profile=builtin") {
		return "", image, fmt.Errorf("Linux cgroup v2, resource controllers and builtin seccomp required")
	}
	if b.Runtime != "" && (b.Runtime != Sysbox || info.Runtimes[Sysbox] == nil) {
		return "", image, fmt.Errorf("runtime %.32q is not registered with the Docker engine", b.Runtime)
	}
	image, err = inspectImage(ctx, b.Image, info.Architecture)
	return info.Architecture, image, err
}

// inspectImage requires a preloaded image matching the engine platform without declared volumes.
func inspectImage(ctx context.Context, ref, engine string) (imageConfig, error) {
	var image imageConfig
	data, err := command(ctx, "image", "inspect", ref, "--format", "{{json .}}")
	if err != nil {
		return image, fmt.Errorf("preload the pinned image: %w", err)
	}
	if err = json.Unmarshal(data, &image); err != nil {
		return image, err
	}
	if host := engineArchitectures[engine]; image.Os != "linux" || host == "" || image.Architecture != host {
		return image, fmt.Errorf("image platform %.16s/%.16s does not match the Docker engine %.16s", image.Os, image.Architecture, engine)
	}
	if len(image.Config.Volumes) > 0 {
		return image, fmt.Errorf("image-declared volumes are unsupported")
	}
	return image, nil
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
	if err = reconcileVolumes(ctx, b.Owner); err != nil {
		return err
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
	if _, err := command(ctx, b.createArgs(r.name, probeResources)...); err != nil {
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

// createArgs places extra controller-owned options before the entrypoint. Under sysbox
// PID 1 is root of the user namespace for the nested engine; commands run as AttemptUser.
func (b *Backend) createArgs(name string, limits Resources, extra ...string) []string {
	runtime, user := "runc", AttemptUser
	if b.Runtime == Sysbox {
		runtime, user = Sysbox, "0:0"
		extra = append([]string{"--tmpfs=/run:" + runTmpfs}, extra...)
	}
	args := []string{"create", "--name", name, "--label", ownerLabel + "=" + b.Owner,
		"--pull=never", "--runtime=" + runtime, "--network=none", "--read-only", "--user=" + user,
		"--cap-drop=ALL", "--security-opt=no-new-privileges=true", "--cgroupns=private", "--ipc=private",
		"--restart=no", "--no-healthcheck", "--log-driver=none", "--workdir=/workspace"}
	// HOME is the tmpfs unless the caller places it elsewhere; one value avoids getenv ambiguity.
	if !slices.ContainsFunc(extra, func(arg string) bool { return strings.HasPrefix(arg, "--env=HOME=") }) {
		args = append(args, "--env=HOME=/workspace")
	}
	args = append(append(append(args, limits.args()...), extra...), "--entrypoint", b.Command[0], b.Image)
	return append(args, b.Command[1:]...)
}

type run struct{ name string }

func (r *run) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.Remove(ctx)
}
func (r *run) Remove(ctx context.Context) error {
	ctx, cancel := removal(ctx)
	defer cancel()
	_, err := commandWithin(ctx, RemoveTimeout, "rm", "-fv", r.name)
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
