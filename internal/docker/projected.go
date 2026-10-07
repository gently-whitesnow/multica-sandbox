package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
)

var networkPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var envPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
var projectionPaths = map[string]bool{"/workspace/opencode.json": true, "/workspace/prompt.txt": true, "/workspace/AGENTS.md": true, "/workspace/data/opencode/mcp-auth.json": true}

type Projected struct {
	Backend
	Network string
	Peers   []string
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
	if err := b.Validate(ctx); err != nil {
		return nil, err
	}
	if err := b.ValidateNetwork(ctx); err != nil {
		return nil, err
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
	args := b.createArgs(r.name)
	for i, arg := range args {
		switch arg {
		case "--network=none":
			args[i] = "--network=" + isolated
		case "--memory=128m":
			args[i] = "--memory=1g"
		case "--memory-swap=128m":
			args[i] = "--memory-swap=1g"
		case "--pids-limit=64":
			args[i] = "--pids-limit=256"
		case "--tmpfs=/workspace:rw,nosuid,nodev,size=67108864,mode=1777":
			args[i] = "--tmpfs=/workspace:rw,nosuid,nodev,size=268435456,mode=1777"
		}
	}
	// The holding process never handles tokens or executes task-supplied shell text.
	args = args[:len(args)-len(b.Command)-2]
	args = append(args, "--env=XDG_DATA_HOME=/workspace/data", "--env=XDG_CONFIG_HOME=/workspace/config", "--env=XDG_CACHE_HOME=/workspace/cache", "--env=XDG_STATE_HOME=/workspace/state", "--env=OPENCODE_DISABLE_AUTOUPDATE=true", "--env=OPENCODE_DISABLE_MODELS_FETCH=true", "--env=OPENCODE_DISABLE_DEFAULT_PLUGINS=true", "--env=OPENCODE_DISABLE_LSP_DOWNLOAD=true", "--entrypoint", "/bin/sh", b.Image, "-c", "exec sleep 86400")
	if _, err := command(ctx, args...); err != nil {
		return nil, errors.Join(err, errors.Join(b.cleanupUncertainCreate(r), cleanup()))
	}
	data, err := command(ctx, "inspect", r.name)
	if err == nil {
		err = checkProjectedPolicy(data, isolated)
	}
	if err != nil {
		return nil, errors.Join(err, cleanup())
	}
	if _, err := command(ctx, "start", r.name); err != nil {
		return nil, errors.Join(err, cleanup())
	}
	return projection, nil
}

type projectedRun struct {
	*run
	network string
	peers   []string
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
	execArgs := []string{"exec"}
	// Values reach the docker client environment only; argv and diagnostics carry names.
	environ := os.Environ()
	for name, value := range env {
		if !envPattern.MatchString(name) || strings.ContainsAny(value, "\x00\r\n") || len(value) > 4096 {
			return fmt.Errorf("invalid agent environment")
		}
		execArgs = append(execArgs, "--env", name)
		environ = append(environ, name+"="+value)
	}
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(execCtx, "docker", append(append(execArgs, r.name), args...)...)
	cmd.Env = environ
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
