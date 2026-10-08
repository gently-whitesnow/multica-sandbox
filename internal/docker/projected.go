package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

var networkPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var envPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
var projectionPaths = map[string]bool{"/workspace/opencode.json": true, "/workspace/prompt.txt": true, "/workspace/AGENTS.md": true, "/workspace/data/opencode/mcp-auth.json": true}

type Projected struct {
	Backend
	Network string
	Peers   []string
	Bundles []Bundle
	// Helper is the digest-pinned sandbox-helper image (ADR 0015). With it, attempts get a
	// workdir volume at WorkDir and a loopback forwarder from DaemonPort to Forward.
	Helper, Forward string
}

// HelperDir holds the helper image mount; the workdir volume is mounted at repo.WorkDir.
const HelperDir = "/opt/multica-sandbox/helper"

// mounts returns the bundles followed by the helper, which stays off PATH.
func (b *Projected) mounts() []Bundle {
	if b.Helper == "" {
		return b.Bundles
	}
	return append(append([]Bundle{}, b.Bundles...), Bundle{Image: b.Helper, Target: HelperDir, NoPath: true})
}

func (b *Projected) ValidateNetwork(ctx context.Context) error {
	if !networkPattern.MatchString(b.Network) || len(b.Peers) == 0 || len(b.Peers) > 16 {
		return fmt.Errorf("approved internal network and peer names required")
	}
	data, err := command(ctx, "network", "inspect", b.Network)
	if err != nil {
		return err
	}
	var networks []struct {
		Internal   bool
		Driver     string
		Containers map[string]struct{ Name string }
	}
	if json.Unmarshal(data, &networks) != nil || len(networks) != 1 || !networks[0].Internal || networks[0].Driver != "bridge" {
		return fmt.Errorf("internal bridge network required")
	}
	seen := map[string]bool{}
	for _, name := range b.Peers {
		if !networkPattern.MatchString(name) || seen[name] {
			return fmt.Errorf("invalid or duplicate execution peer")
		}
		seen[name] = true
	}
	if len(networks[0].Containers) != len(b.Peers) {
		return fmt.Errorf("execution template must contain exactly the approved peers")
	}
	for _, peer := range networks[0].Containers {
		allowed := false
		for _, name := range b.Peers {
			allowed = allowed || peer.Name == name
		}
		if !allowed {
			return fmt.Errorf("unapproved peer on execution network")
		}
	}
	return nil
}

func (b *Projected) Start(ctx context.Context, attempt string) (execution.ProjectedRun, error) {
	bundles, path, err := b.prepare(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.ValidateNetwork(ctx); err != nil {
		return nil, err
	}
	if b.Helper != "" {
		if _, port, err := net.SplitHostPort(b.Forward); err != nil || port == "" || strings.ContainsAny(b.Forward, " \t\r\n") {
			return nil, fmt.Errorf("helper forward target host:port required")
		}
	}
	r := &run{name: b.name(attempt)}
	isolated, err := b.isolate(ctx, r.name+"-net")
	if err != nil {
		return nil, err
	}
	projection := &projectedRun{run: r, network: isolated, peers: b.Peers}
	cleanup := func() error {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return projection.Remove(cleanCtx)
	}
	if b.Helper != "" {
		projection.volume = r.name + "-work"
		if err := b.workdir(ctx, projection.volume); err != nil {
			return nil, errors.Join(err, cleanup())
		}
		bundles = append(bundles, "--mount=type=volume,source="+projection.volume+",target="+repo.WorkDir+",volume-nocopy")
	}
	args := attemptLimits(b.createArgs(r.name, bundles...), isolated)
	// The holding process never handles tokens or executes task-supplied shell text.
	args = args[:len(args)-len(b.Command)-2]
	args = append(args, "--env=XDG_DATA_HOME=/workspace/data", "--env=XDG_CONFIG_HOME=/workspace/config", "--env=XDG_CACHE_HOME=/workspace/cache", "--env=XDG_STATE_HOME=/workspace/state", "--env=OPENCODE_DISABLE_AUTOUPDATE=true", "--env=OPENCODE_DISABLE_MODELS_FETCH=true", "--env=OPENCODE_DISABLE_DEFAULT_PLUGINS=true", "--env=OPENCODE_DISABLE_LSP_DOWNLOAD=true", "--entrypoint", "/bin/sh", b.Image, "-c", "exec sleep 86400")
	if _, err := command(ctx, args...); err != nil {
		return nil, errors.Join(err, errors.Join(b.cleanupUncertainCreate(r), cleanup()))
	}
	data, err := command(ctx, "inspect", r.name)
	if err == nil {
		err = checkProjectedPolicy(data, isolated, b.mounts(), path, projection.volume)
	}
	if err != nil {
		return nil, errors.Join(err, cleanup())
	}
	if _, err := command(ctx, "start", r.name); err != nil {
		return nil, errors.Join(err, cleanup())
	}
	if b.Helper != "" {
		// The forwarder runs as the attempt user and reaches only the attempt network.
		if _, err := command(ctx, "exec", "-d", r.name, HelperDir+"/sandbox-helper", "forward", "127.0.0.1:"+repo.DaemonPort, b.Forward); err != nil {
			return nil, errors.Join(err, cleanup())
		}
	}
	return projection, nil
}

// workdir creates an owned volume and hands its root to the attempt user from the
// helper image: no network, a read-only rootfs and only CAP_CHOWN.
func (b *Projected) workdir(ctx context.Context, volume string) error {
	if _, err := command(ctx, "volume", "create", "--label", ownerLabel+"="+b.Owner, volume); err != nil {
		return err
	}
	_, err := command(ctx, "run", "--rm", "--name", volume+"-init", "--label", ownerLabel+"="+b.Owner, "--pull=never", "--runtime=runc",
		"--network=none", "--read-only", "--user=0:0", "--cap-drop=ALL", "--cap-add=CHOWN", "--security-opt=no-new-privileges=true",
		"--memory=32m", "--pids-limit=8", "--log-driver=none", "--mount=type=volume,source="+volume+",target=/work,volume-nocopy",
		"--entrypoint", "/sandbox-helper", b.Helper, "init", "/work")
	return err
}

// Output inspects the agent image offline within attempt resource limits and returns its bounded stdout.
func (b *Projected) Output(ctx context.Context, args []string) ([]byte, error) {
	probe := Projected{Backend: Backend{Image: b.Image, Owner: b.Owner, Command: args}, Bundles: b.Bundles}
	bundles, path, err := probe.prepare(ctx)
	if err != nil {
		return nil, err
	}
	r := &run{name: probe.name("image-inspection")}
	if _, err := command(ctx, attemptLimits(probe.createArgs(r.name, bundles...), "none")...); err != nil {
		return nil, errors.Join(err, probe.cleanupUncertainCreate(r))
	}
	data, err := command(ctx, "inspect", r.name)
	if err == nil {
		err = checkProjectedPolicy(data, "none", b.Bundles, path, "")
	}
	if err != nil {
		return nil, errors.Join(err, r.cleanup())
	}
	data, err = command(ctx, "start", "--attach", r.name)
	return data, errors.Join(err, r.cleanup())
}

func attemptLimits(args []string, network string) []string {
	for i, arg := range args {
		switch arg {
		case "--network=none":
			args[i] = "--network=" + network
		case "--memory=128m":
			args[i] = "--memory=1g"
		case "--memory-swap=128m":
			args[i] = "--memory-swap=1g"
		case "--pids-limit=64":
			args[i] = "--pids-limit=256"
		case "--tmpfs=/workspace:rw,exec,nosuid,nodev,size=67108864,mode=1777":
			args[i] = "--tmpfs=/workspace:rw,exec,nosuid,nodev,size=268435456,mode=1777"
		}
	}
	return args
}

type projectedRun struct {
	*run
	network, volume string
	peers           []string
}

func (r *projectedRun) Write(ctx context.Context, path string, data []byte) error {
	if !projectionPaths[path] || len(data) > 65536 {
		return fmt.Errorf("invalid projection")
	}
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", r.name, "/bin/sh", "-c", `set -eu; umask 077; mkdir -p "${1%/*}"; cat > "$1.next"; chmod 600 "$1.next"; mv -f "$1.next" "$1"`, "projection", path)
	cmd.Stdin = bytes.NewReader(data)
	if cmd.Run() != nil {
		return fmt.Errorf("atomic projection failed")
	}
	return nil
}

func (r *projectedRun) Execute(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", r.name}, args...)...)
	var out limitedOutput
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("agent execution failed; output withheld")
	}
	return nil
}

func (r *projectedRun) Remove(ctx context.Context) error {
	err := r.run.Remove(ctx)
	// A failed volume init leaves no container; the volume is still removed.
	if r.volume != "" {
		_, volumeErr := command(ctx, "volume", "rm", "-f", r.volume)
		err = errors.Join(err, volumeErr)
	}
	return errors.Join(err, removeNetwork(ctx, r.network, r.peers))
}

func removeNetwork(ctx context.Context, name string, peers []string) error {
	var result error
	for _, peer := range peers {
		_, err := command(ctx, "network", "disconnect", "-f", name, peer)
		result = errors.Join(result, err)
	}
	_, err := command(ctx, "network", "rm", name)
	return errors.Join(result, err)
}

func (b *Projected) isolate(ctx context.Context, name string) (string, error) {
	if _, err := command(ctx, "network", "create", "--internal", "--label", ownerLabel+"="+b.Owner, name); err != nil {
		return "", err
	}
	connected := []string{}
	for _, peer := range b.Peers {
		data, err := command(ctx, "inspect", peer)
		var containers []struct {
			NetworkSettings struct {
				Networks map[string]struct{ Aliases []string }
			}
		}
		if err == nil && (json.Unmarshal(data, &containers) != nil || len(containers) != 1) {
			err = fmt.Errorf("invalid peer inspection")
		}
		if err != nil {
			return "", errors.Join(err, removeNetwork(ctx, name, connected))
		}
		args := []string{"network", "connect"}
		for _, alias := range containers[0].NetworkSettings.Networks[b.Network].Aliases {
			args = append(args, "--alias", alias)
		}
		args = append(args, name, peer)
		if _, err := command(ctx, args...); err != nil {
			return "", errors.Join(err, removeNetwork(ctx, name, connected))
		}
		connected = append(connected, peer)
	}
	return name, nil
}

// reconcileVolumes removes owned workdir volumes; every one belongs to a finished attempt.
func reconcileVolumes(ctx context.Context, owner string) error {
	data, err := command(ctx, "volume", "ls", "-q", "--filter", "label="+ownerLabel+"="+owner)
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(string(data)) {
		if _, err := command(ctx, "volume", "rm", "-f", name); err != nil {
			return err
		}
	}
	return nil
}

func reconcileNetworks(ctx context.Context, owner string) error {
	data, err := command(ctx, "network", "ls", "-q", "--filter", "label="+ownerLabel+"="+owner)
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(string(data)) {
		data, err := command(ctx, "network", "inspect", id)
		if err != nil {
			return err
		}
		var networks []struct{ Containers map[string]json.RawMessage }
		if json.Unmarshal(data, &networks) != nil || len(networks) != 1 {
			return fmt.Errorf("invalid owned network")
		}
		peers := []string{}
		for peer := range networks[0].Containers {
			peers = append(peers, peer)
		}
		if err := removeNetwork(ctx, id, peers); err != nil {
			return err
		}
	}
	return nil
}

// Stream keeps native stdout out of diagnostics and bounds it in the adapter.
func (r *projectedRun) Stream(ctx context.Context, args []string, env map[string]string, consume func(io.Reader) error) error {
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := r.exec(execCtx, args, env)
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("agent output pipe failed")
	}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("agent execution failed")
	}
	readErr := consume(stdout)
	if readErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return fmt.Errorf("agent execution failed; output withheld")
	}
	return nil
}

// exec builds a docker exec in the agent working directory. Env values reach the docker
// client environment only; argv and diagnostics carry names.
func (r *projectedRun) exec(ctx context.Context, args []string, env map[string]string) (*exec.Cmd, error) {
	execArgs := []string{"exec"}
	if r.volume != "" {
		execArgs = append(execArgs, "--workdir", repo.WorkDir)
	}
	environ := os.Environ()
	for name, value := range env {
		if !envPattern.MatchString(name) || strings.ContainsAny(value, "\x00\r\n") || len(value) > 4096 {
			return nil, fmt.Errorf("invalid agent environment")
		}
		execArgs = append(execArgs, "--env", name)
		environ = append(environ, name+"="+value)
	}
	cmd := exec.CommandContext(ctx, "docker", append(append(execArgs, r.name), args...)...)
	cmd.Env = environ
	return cmd, nil
}

// Capture returns at most 64 KiB of stdout; stderr is discarded as attempt-controlled.
func (r *projectedRun) Capture(ctx context.Context, args []string, env map[string]string) ([]byte, error) {
	cmd, err := r.exec(ctx, args, env)
	if err != nil {
		return nil, err
	}
	var out cappedOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("attempt command failed")
	}
	return out.Bytes(), nil
}

// cappedOutput keeps the first 64 KiB and drops the rest without failing the writer.
type cappedOutput struct{ bytes.Buffer }

func (w *cappedOutput) Write(p []byte) (int, error) {
	if room := 65536 - w.Len(); room > 0 {
		w.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
