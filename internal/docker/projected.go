package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/pool"
	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

var networkPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var envPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
var projectionPaths = map[string]bool{"/workspace/opencode.json": true, "/workspace/prompt.txt": true, "/workspace/AGENTS.md": true, "/workspace/data/opencode/mcp-auth.json": true, repo.CAPath: true}

type Projected struct {
	Backend
	Network string
	Peers   []string
	Bundles []Bundle
	// Helper is the digest-pinned sandbox-helper image (ADR 0015). With it, attempts get a
	// workdir volume at WorkDir and a loopback forwarder from DaemonPort to Forward.
	Helper, Forward string
	// ForgeHosts resolve to loopback, where the helper forwards repo.ForgePort to
	// ForgeForward, the forge relay for gh.
	ForgeHosts   []string
	ForgeForward string
	// Sessions, with the helper, retains issue workdirs across attempts.
	Sessions *Sessions
	// Resources bound attempts and image inspection; zero means the runtime's defaults.
	Resources Resources
	// DinD configures the nested engine under sysbox, which requires the helper.
	DinD DinD
	// Pool reads the slice of Resources.Pool (ADR 0020); Preflight records its memory.max.
	Pool    *pool.Pool
	poolMax int64
}

// DinD configures the nested engine of sysbox attempts (ADR 0019).
type DinD struct {
	RegistryMirrors, InsecureRegistries []string
	ReadyTimeout                        time.Duration
	// RyukDisabled sets TESTCONTAINERS_RYUK_DISABLED: removing the attempt removes its containers.
	RyukDisabled bool
}

func (b *Projected) limits() Resources {
	switch {
	case b.Resources != (Resources{}):
		return b.Resources
	case b.Runtime == Sysbox:
		return SysboxResources
	}
	return DefaultResources
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

// Start creates an attempt; a Workdir with a conversation asks for its retained session volume.
func (b *Projected) Start(ctx context.Context, attempt string, w execution.Workdir) (execution.ProjectedRun, error) {
	bundles, path, err := b.prepare(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.ValidateNetwork(ctx); err != nil {
		return nil, err
	}
	if b.Helper != "" {
		targets := []string{b.Forward}
		if b.ForgeForward != "" {
			targets = append(targets, b.ForgeForward)
		}
		for _, target := range targets {
			if _, port, err := net.SplitHostPort(target); err != nil || port == "" || strings.ContainsAny(target, " \t\r\n") {
				return nil, fmt.Errorf("helper forward targets must be host:port")
			}
		}
	}
	if (b.ForgeForward == "") != (len(b.ForgeHosts) == 0) || (b.ForgeForward != "" && b.Helper == "") {
		return nil, fmt.Errorf("forge hosts and forward target require each other and the helper")
	}
	for _, name := range b.ForgeHosts {
		if !hostPattern.MatchString(name) {
			return nil, fmt.Errorf("invalid forge host name")
		}
	}
	if b.Runtime == Sysbox && b.Helper == "" {
		return nil, fmt.Errorf("sysbox attempts require the helper")
	}
	r := &run{name: b.name(attempt)}
	isolated, err := b.isolate(ctx, r.name+"-net")
	if err != nil {
		return nil, err
	}
	projection := &projectedRun{run: r, network: isolated, peers: b.Peers, owner: b.Owner, pool: b.Pool, poolMax: b.poolMax}
	cleanup := func() error {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return projection.Remove(cleanCtx)
	}
	if b.Helper != "" {
		if err := b.attachWorkdir(ctx, projection, w); err != nil {
			return nil, errors.Join(err, cleanup())
		}
		bundles = append(bundles, "--mount=type=volume,source="+projection.volume+",target="+repo.WorkDir+",volume-nocopy")
	}
	for _, host := range loopbackHosts(b.ForgeHosts) {
		bundles = append(bundles, "--add-host="+host)
	}
	bundles = append(bundles, "--env=TMPDIR="+TmpDir, "--env=XDG_DATA_HOME=/workspace/data", "--env=XDG_CONFIG_HOME=/workspace/config", "--env=XDG_CACHE_HOME=/workspace/cache", "--env=XDG_STATE_HOME=/workspace/state", "--env=OPENCODE_DISABLE_AUTOUPDATE=true", "--env=OPENCODE_DISABLE_MODELS_FETCH=true", "--env=OPENCODE_DISABLE_DEFAULT_PLUGINS=true", "--env=OPENCODE_DISABLE_LSP_DOWNLOAD=true")
	if b.Runtime == Sysbox && b.DinD.RyukDisabled {
		bundles = append(bundles, "--env=TESTCONTAINERS_RYUK_DISABLED=true")
	}
	want := expected{network: isolated, limits: b.limits(), bundles: b.mounts(), path: path, volume: projection.volume, hosts: b.ForgeHosts, runtime: b.Runtime}
	projection.id, err = b.launch(ctx, r, want, bundles)
	if err != nil {
		cleanErr := cleanup()
		if errors.Is(err, errEngine) && cleanErr == nil {
			return nil, &execution.RejectedError{Err: err}
		}
		return nil, errors.Join(err, cleanErr)
	}
	// Like the upstream task directory, TMPDIR allows executables (go test, build caches).
	if _, err := command(ctx, "exec", "--user", AttemptUser, r.name, "mkdir", "-p", "-m", "700", TmpDir); err != nil {
		return nil, errors.Join(err, cleanup())
	}
	if b.Helper != "" {
		// The forwarder runs as the attempt user and reaches only the attempt network.
		forward := []string{"exec", "-d", "--user", AttemptUser, r.name, HelperDir + "/sandbox-helper", "forward", "127.0.0.1:" + repo.DaemonPort, b.Forward}
		if b.ForgeForward != "" {
			forward = append(forward, "127.0.0.1:"+repo.ForgePort, b.ForgeForward)
		}
		if _, err := command(ctx, forward...); err != nil {
			return nil, errors.Join(err, cleanup())
		}
	}
	return projection, nil
}

// errEngine marks a nested engine that did not become ready: the task is rejected.
var errEngine = errors.New("nested Docker engine did not become ready")

// launch creates the attempt with the controller-owned entrypoint, compares it with the
// policy, starts it and waits until it may run commands. It returns the container ID.
func (b *Projected) launch(ctx context.Context, r *run, want expected, extra []string) (string, error) {
	args := attemptNetwork(b.createArgs(r.name, want.limits, extra...), want.network)
	args = append(args[:len(args)-len(b.Command)-2], b.entrypoint()...)
	out, err := command(ctx, args...)
	if err != nil {
		return "", errors.Join(err, b.cleanupUncertainCreate(r))
	}
	id := strings.TrimSpace(string(out))
	data, err := command(ctx, "inspect", r.name)
	if err == nil {
		err = checkExpectedPolicy(data, want)
	}
	if err == nil {
		_, err = command(ctx, "start", r.name)
	}
	if err == nil && b.Runtime == Sysbox {
		err = b.waitEngine(ctx, r.name)
	}
	if err == nil {
		err = checkRuntime(ctx, r.name, b.Runtime == Sysbox)
	}
	if err == nil && b.Pool != nil {
		err = b.Pool.CheckAttempt(id, want.limits.Memory)
	}
	return id, err
}

// entrypoint is the holding process, which never handles tokens or executes task-supplied
// shell text: sleep under runc, the helper's nested engine supervisor under sysbox.
func (b *Projected) entrypoint() []string {
	if b.Runtime != Sysbox {
		return []string{"--entrypoint", "/bin/sh", b.Image, "-c", "exec sleep 86400"}
	}
	args := []string{"--entrypoint", HelperDir + "/sandbox-helper", b.Image, "dind"}
	for _, mirror := range b.DinD.RegistryMirrors {
		args = append(args, "--registry-mirror="+mirror)
	}
	for _, registry := range b.DinD.InsecureRegistries {
		args = append(args, "--insecure-registry="+registry)
	}
	return args
}

// waitEngine waits for the supervisor's readiness marker (sandbox-helper dind-wait).
func (b *Projected) waitEngine(ctx context.Context, name string) error {
	timeout := b.DinD.ReadyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, "docker", "exec", "--user", AttemptUser, name, HelperDir+"/sandbox-helper", "dind-wait", timeout.String()).Run()
	if err != nil && ctx.Err() == nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("%w within %s", errEngine, timeout)
		}
	}
	return err
}

// attachWorkdir gives the attempt its conversation's session volume when it is free within
// sessionBusyWait, and a per-attempt volume otherwise.
func (b *Projected) attachWorkdir(ctx context.Context, p *projectedRun, w execution.Workdir) error {
	if b.Sessions != nil && w.Conversation() != "" {
		name, label := sessionName(b.Owner, w)
		if b.Sessions.acquire(ctx, name) {
			p.volume, p.sessions = name, b.Sessions
			var err error
			p.reused, err = b.sessionVolume(ctx, name, label)
			return err
		}
	}
	p.volume = p.name + "-work"
	return b.workdir(ctx, p.volume)
}

// workdir creates an owned volume and hands its root to the attempt user from the
// helper image: no network, a read-only rootfs and only CAP_CHOWN.
func (b *Projected) workdir(ctx context.Context, volume string, labels ...string) error {
	if _, err := command(ctx, append(append([]string{"volume", "create", "--label", ownerLabel + "=" + b.Owner}, labels...), volume)...); err != nil {
		return err
	}
	_, err := command(ctx, "run", "--rm", "--name", volume+"-init", "--label", ownerLabel+"="+b.Owner, "--pull=never", "--runtime=runc",
		"--network=none", "--read-only", "--user=0:0", "--cap-drop=ALL", "--cap-add=CHOWN", "--security-opt=no-new-privileges=true",
		"--memory=32m", "--pids-limit=8", "--log-driver=none", "--mount=type=volume,source="+volume+",target=/work,volume-nocopy",
		"--entrypoint", "/sandbox-helper", b.Helper, "init", "/work")
	if err != nil {
		_, rmErr := command(ctx, "volume", "rm", "-f", volume)
		err = errors.Join(err, rmErr)
	}
	return err
}

// Output inspects the agent image offline within attempt resource limits and returns its bounded stdout.
func (b *Projected) Output(ctx context.Context, args []string) ([]byte, error) {
	// Inspection reads files only: it runs under runc within the attempt resources.
	probe := Projected{Backend: Backend{Image: b.Image, Owner: b.Owner, Command: args}, Bundles: b.Bundles, Resources: b.limits()}
	bundles, path, err := probe.prepare(ctx)
	if err != nil {
		return nil, err
	}
	r := &run{name: probe.name("image-inspection")}
	if _, err := command(ctx, probe.createArgs(r.name, probe.limits(), bundles...)...); err != nil {
		return nil, errors.Join(err, probe.cleanupUncertainCreate(r))
	}
	data, err := command(ctx, "inspect", r.name)
	if err == nil {
		err = checkExpectedPolicy(data, expected{network: "none", limits: probe.limits(), bundles: b.Bundles, path: path})
	}
	if err != nil {
		return nil, errors.Join(err, r.cleanup())
	}
	data, err = command(ctx, "start", "--attach", r.name)
	return data, errors.Join(err, r.cleanup())
}

func attemptNetwork(args []string, network string) []string {
	for i, arg := range args {
		if arg == "--network=none" {
			args[i] = "--network=" + network
		}
	}
	return args
}

type projectedRun struct {
	*run
	id, network, volume, owner string
	peers                      []string
	// pool, when set, holds the attempt cgroup; poolMax is its memory.max.
	pool    *pool.Pool
	poolMax int64
	// sessions holds the retained volume's writer slot; reused means it already existed.
	sessions *Sessions
	reused   bool
}

func (r *projectedRun) Workdir() (string, bool) {
	if r.sessions == nil {
		return "", false
	}
	return r.volume, r.reused
}

func (r *projectedRun) Write(ctx context.Context, path string, data []byte) error {
	if !projectionPaths[path] || len(data) > 65536 {
		return fmt.Errorf("invalid projection")
	}
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", "--user", AttemptUser, r.name, "/bin/sh", "-c", `set -eu; umask 077; mkdir -p "${1%/*}"; cat > "$1.next"; chmod 600 "$1.next"; mv -f "$1.next" "$1"`, "projection", path)
	cmd.Stdin = bytes.NewReader(data)
	if cmd.Run() != nil {
		return fmt.Errorf("atomic projection failed")
	}
	return nil
}

// treeRoots are the directories WriteTree may fill; the attempt user creates them.
var treeRoots = map[string]bool{"/workspace/config/opencode/skills": true}

// WriteTree streams a controller-built tar of regular files to the attempt. Paths are
// clean and relative, so extraction stays under root; the agent has not run yet.
func (r *projectedRun) WriteTree(ctx context.Context, root string, files map[string][]byte) error {
	if !treeRoots[root] {
		return fmt.Errorf("invalid projection tree")
	}
	var archive bytes.Buffer
	if err := writeTar(&archive, files, 16<<20); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", "--user", AttemptUser, r.name, "/bin/sh", "-c", `set -eu; umask 022; mkdir -p "$1"; tar -x -f - -C "$1"`, "projection", root)
	cmd.Stdin = &archive
	if cmd.Run() != nil {
		return fmt.Errorf("tree projection failed")
	}
	return nil
}

func writeTar(w io.Writer, files map[string][]byte, limit int) error {
	names := slices.Sorted(maps.Keys(files))
	tw, dirs, total, now := tar.NewWriter(w), map[string]bool{}, 0, time.Now()
	for _, name := range names {
		if name == "" || name == "." || path.Clean(name) != name || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00") {
			return fmt.Errorf("invalid projection tree path")
		}
		if total += len(files[name]); total > limit {
			return fmt.Errorf("projection tree exceeds limit")
		}
		var parents []string
		for dir := path.Dir(name); dir != "." && !dirs[dir]; dir = path.Dir(dir) {
			parents = append(parents, dir)
		}
		for i := len(parents) - 1; i >= 0; i-- {
			dirs[parents[i]] = true
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: parents[i] + "/", Mode: 0755, ModTime: now}); err != nil {
				return err
			}
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0644, Size: int64(len(files[name])), ModTime: now}); err != nil {
			return err
		}
		if _, err := tw.Write(files[name]); err != nil {
			return err
		}
	}
	return tw.Close()
}

func (r *projectedRun) Execute(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", "--user", AttemptUser, r.name}, args...)...)
	var out limitedOutput
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("agent execution failed; output withheld")
	}
	return nil
}

func (r *projectedRun) Remove(ctx context.Context) error {
	ctx, cancel := removal(ctx)
	defer cancel()
	err := r.run.Remove(ctx)
	switch {
	case r.sessions != nil:
		// The container is gone, so the next run of this conversation may write.
		err = errors.Join(err, r.sessions.release(r.volume), r.sessions.Collect(ctx, r.owner))
	case r.volume != "":
		// A failed volume init leaves no container; the volume is still removed.
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
	if err := createNetwork(ctx, b.Owner, name); err != nil {
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

// createNetwork creates an attempt network. When the daemon refuses it, the task is
// rejected instead of stopping the controller, after a best-effort removal in case a
// partial network exists (Reconcile removes what remains); exhausted address pools are
// named for the operator. Timeouts stay uncertain and fatal.
func createNetwork(ctx context.Context, owner, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "network", "create", "--internal", "--label", ownerLabel+"="+owner, name)
	var stderr cappedOutput
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil || !errors.As(err, &exit):
		return fmt.Errorf("docker network failed: %w", err)
	}
	_ = removeNetwork(ctx, name, nil)
	switch {
	case strings.Contains(stderr.String(), "address pools have been fully subnetted"):
		return &execution.RejectedError{Err: errors.New("Docker address pools are exhausted: raise default-address-pools or lower concurrency")}
	}
	return &execution.RejectedError{Err: errors.New("Docker refused the attempt network")}
}

// reconcileVolumes removes owned per-attempt workdir volumes, which all belong to
// finished attempts; session volumes stay for Sessions.Collect.
func reconcileVolumes(ctx context.Context, owner string) error {
	data, err := command(ctx, "volume", "ls", "--filter", "label="+ownerLabel+"="+owner, "--format", `{{.Name}} {{.Label "`+sessionLabel+`"}}`)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, session, _ := strings.Cut(line, " ")
		if name == "" || session != "" {
			continue
		}
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
	// An agent killed for memory also ends its stream early; the kill decides the failure,
	// which Multica must not retry as a provider stream cut.
	if readErr != nil || waitErr != nil {
		if oom := r.outOfMemory(); oom != nil {
			return oom
		}
	}
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return fmt.Errorf("agent execution failed; output withheld")
	}
	return nil
}

// outOfMemory reports an OOM kill in the attempt cgroup: read on the host through the
// pool, else inside the attempt, else Docker's record of a container that is gone.
func (r *projectedRun) outOfMemory() *execution.OOMError {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var m pool.Memory
	var err error
	if r.pool != nil {
		m, err = r.pool.AttemptMemory(r.id)
	} else {
		var out []byte
		out, err = command(ctx, "exec", "--user", AttemptUser, r.name, "cat", "/sys/fs/cgroup/memory.events", "/sys/fs/cgroup/memory.peak", "/sys/fs/cgroup/memory.max")
		if err == nil {
			m, err = pool.ParseMemory(out)
		}
	}
	if err != nil {
		data, inspectErr := command(ctx, "inspect", "--format", "{{.State.OOMKilled}}", r.name)
		if inspectErr != nil || strings.TrimSpace(string(data)) != "true" {
			return nil
		}
		m = pool.Memory{Peak: -1, Max: -1}
	} else if m.OOMKill == 0 {
		return nil
	}
	oom := &execution.OOMError{Killed: m.OOMKill, Peak: m.Peak, Limit: m.Max}
	// Without its own limit hit, the pool's memory.max was reached.
	if r.pool != nil && (m.Max == pool.Unlimited || m.OOM == 0) {
		oom.Pool, oom.Limit = true, r.poolMax
	}
	return oom
}

// exec builds a docker exec in the agent working directory. Env values reach the docker
// client environment only; argv and diagnostics carry names.
func (r *projectedRun) exec(ctx context.Context, args []string, env map[string]string) (*exec.Cmd, error) {
	execArgs := []string{"exec", "--user", AttemptUser}
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
