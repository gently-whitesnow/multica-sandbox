package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

type Issuer interface {
	// AcquireForMCP issues the agent's token for a selected MCP URL; env is the claim's
	// agent custom_env, which may carry the agent's credentials (ADR 0022).
	AcquireForMCP(context.Context, identity.Ref, identity.Env, string) (identity.AccessToken, error)
}
type Authority interface {
	Apply(context.Context, attempt.Grant) error
}
type Workloads interface {
	Start(context.Context, string, execution.Workdir) (execution.ProjectedRun, error)
}
type Status interface {
	Status(context.Context, string) (string, error)
}

// Grants issues per-attempt relay credentials; upstream credentials stay in controller memory.
type Grants interface {
	Issue(string, relay.Upstream) (string, error)
	Revoke(string)
	// Refusal is the attempt's last upstream refusal status, 0 if none.
	Refusal(string) int
}

type Adapter struct {
	Server, Controller string
	Issuer             Issuer
	Inference          Inference
	Authority          Authority
	Workloads          Workloads
	Status             Status
	// Command replaces the `opencode acp` argv; it must speak ACP on stdio.
	Command  []string
	Reporter Reporter
	// Relay and RelayURL enable upstream-equivalent Multica CLI access (ADR 0014).
	Relay    Grants
	RelayURL string
	// MCPRelay and MCPRelayURL carry selected MCP connections with controller-held
	// tokens (ADR 0023); agents selecting MCP need them.
	MCPRelay    *MCPRelay
	MCPRelayURL string
	// InferenceRelay and InferenceRelayURL carry workspace-key inference (ADR 0012).
	InferenceRelay    Grants
	InferenceRelayURL string
	// GitRelay and Checkout serve upstream repository checkout (ADR 0015); they require
	// the Multica relay and a workload with a workdir volume.
	GitRelay *repo.Relay
	Checkout *repo.Checkout
	// EnvAllow names the agent custom_env variables attempts receive (ValidEnvAllow).
	EnvAllow []string
	// IdleWatchdog fails a run without native events for this long (default 10m, upstream's).
	IdleWatchdog time.Duration
}

type running struct {
	adapter     *Adapter
	task        multica.Task
	connections map[string]Remote
	workload    execution.ProjectedRun
	// mu guards tokens, which the MCP relay reads while the renewal loop replaces them.
	mu         sync.Mutex
	tokens     map[string]identity.AccessToken
	cancel     context.CancelFunc
	done       chan error
	exited     chan struct{}
	started    bool
	once       sync.Once
	cleanupErr error
	events     *eventStream
	model      string
	effort     string
	relayToken string
	// mcpToken is the opaque MCP relay credential named in session/new.
	mcpToken string
	// prompt is the turn's message; v2 marks an OpenCode 2.x agent.
	prompt []byte
	v2     bool
	// inferenceToken is the opaque per-attempt provider key; it never rotates.
	inferenceToken string
	// gitEnv routes claim repositories through the Git relay with an opaque credential.
	gitEnv map[string]string
	// workDir names the retained workdir; resume is the session OpenCode continues and
	// retired the prior session a failed resume abandoned (ADR 0015).
	workDir, resume, retired string
	// skills are the claim's skill files under SkillsDir, upstream tasks only (ADR 0017).
	skills map[string][]byte
}

// SessionDB keeps OpenCode's session store in the retained workdir, apart from
// XDG_DATA_HOME.
const SessionDB = "/workspace/work/.multica-sandbox/opencode.db"

var sessionID = regexp.MustCompile(`^ses_[A-Za-z0-9]{1,64}$`)

func (a *Adapter) Start(ctx context.Context, task multica.Task) (execution.Run, error) {
	connections, err := Select(task)
	if err != nil {
		return nil, &execution.RejectedError{Err: err}
	}
	if len(connections) != 0 && a.Issuer == nil {
		return nil, &execution.RejectedError{Err: fmt.Errorf("MCP connections require identity_file: %w", ErrDenied)}
	}
	if len(connections) != 0 && a.MCPRelay == nil {
		return nil, &execution.RejectedError{Err: fmt.Errorf("MCP connections require mcp_relay: %w", ErrDenied)}
	}
	if PinnedHere(task, a.Controller) {
		return nil, &execution.RejectedError{Err: fmt.Errorf("local_directory resources are not mounted: %w", ErrDenied)}
	}
	continuity := Fresh
	if task.PriorSessionID != "" {
		continuity = Lost
	}
	if _, _, err = Prompt(task, a.Relay != nil, a.Checkout != nil, continuity); err != nil {
		return nil, &execution.RejectedError{Err: err}
	}
	var skills map[string][]byte
	if a.Relay != nil {
		if skills, err = skillTree(task.Agent.Skills); err != nil {
			return nil, &execution.RejectedError{Err: err}
		}
	}
	r := &running{adapter: a, task: task, connections: connections, tokens: map[string]identity.AccessToken{}, done: make(chan error, 1), exited: make(chan struct{}), skills: skills}
	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	if a.Relay != nil {
		token, err := task.TaskToken()
		if err == nil {
			r.relayToken, err = a.Relay.Issue(task.AttemptKey(), relay.Upstream{Origin: a.Server, Credential: token})
		}
		if err != nil {
			return nil, r.reject(fmt.Errorf("Multica relay grant: %w", ErrDenied))
		}
	}
	if len(connections) != 0 {
		if r.mcpToken, err = a.MCPRelay.issue(r); err != nil {
			return nil, r.reject(fmt.Errorf("MCP relay grant: %w", ErrDenied))
		}
	}
	if a.GitRelay != nil {
		urls := make([]string, len(task.Repos))
		for i, repository := range task.Repos {
			urls[i] = repository.URL
		}
		if r.gitEnv, err = a.GitRelay.Issue(task.AttemptKey(), task.WorkspaceID, urls); err != nil {
			return nil, r.reject(fmt.Errorf("Git relay grant: %w", ErrDenied))
		}
	}
	if err = r.grantInference(runCtx); err != nil {
		cancel()
		return nil, r.reject(err)
	}
	if err = r.refresh(runCtx); err != nil {
		cancel()
		return nil, r.reject(err)
	}
	// Upstream chat and issue conversations keep a retained workdir when the backend offers one.
	var workdir execution.Workdir
	switch {
	case a.Relay == nil:
	case chatTask(task):
		workdir = execution.Workdir{Workspace: task.WorkspaceID, Agent: task.AgentID, Chat: task.ChatSessionID, Prior: task.PriorWorkDir}
	case upstreamTask(task):
		workdir = execution.Workdir{Workspace: task.WorkspaceID, Agent: task.AgentID, Issue: task.IssueID, Prior: task.PriorWorkDir}
	}
	r.workload, err = a.Workloads.Start(runCtx, task.AttemptKey(), workdir)
	if err != nil {
		cancel()
		// A backend refusal left no resources: the task fails, the controller keeps serving.
		if rejected := (*execution.RejectedError)(nil); errors.As(err, &rejected) {
			return nil, r.reject(rejected.Err)
		}
		return nil, errors.Join(err, r.Remove(context.Background()))
	}
	// Like upstream, the prior session resumes only in the workdir it was recorded with.
	var reused bool
	r.workDir, reused = r.workload.Workdir()
	if reused && r.workDir == task.PriorWorkDir && sessionID.MatchString(task.PriorSessionID) {
		r.resume, continuity = task.PriorSessionID, Resumed
	}
	var brief []byte
	r.prompt, brief, err = Prompt(task, a.Relay != nil, a.Checkout != nil, continuity)
	if err != nil {
		return nil, r.reject(err)
	}
	project, _ := projectFile(task)
	if err = r.initialize(runCtx, brief, project); err != nil {
		return nil, r.reject(err)
	}
	if a.Checkout != nil {
		repos := map[string]string{}
		for _, repository := range task.Repos {
			repos[strings.TrimSpace(repository.URL)] = repository.Ref
		}
		a.Checkout.Register(task.AttemptKey(), repo.Task{Workspace: task.WorkspaceID, ID: task.ID, AgentName: task.Agent.Name, Repos: repos, Target: r.workload, Env: r.gitEnv})
		// Retained checkouts follow the current setting before the agent commits.
		a.Checkout.Refresh(runCtx, task.AttemptKey())
	}
	r.events = &eventStream{reporter: a.Reporter, task: task.ID, model: r.model, workDir: r.workDir}
	r.events.touch()
	r.started = true
	go r.loop(runCtx)
	return r, nil
}

func (r *running) initialize(ctx context.Context, brief, project []byte) error {
	// The global config directory is sealed after skills are laid down in it; a stale project file goes.
	script, seal := `set -eu; mkdir -p /workspace/config/opencode; printf "{}" > /workspace/config/opencode/opencode.json; printf "*\n" > /workspace/config/opencode/.gitignore; rm -rf "$1" || true`, "chmod 555 /workspace/config/opencode"
	if len(r.skills) == 0 {
		script += "; " + seal
	}
	if r.workDir != "" {
		script += `; mkdir -p -m 700 "${0%/*}"`
	}
	if err := r.workload.Execute(ctx, []string{"/bin/sh", "-c", script, SessionDB, ProjectPath}); err != nil {
		return fmt.Errorf("OpenCode config directory: %w", ErrDenied)
	}
	if len(r.skills) > 0 {
		if err := r.workload.WriteTree(ctx, SkillsDir, r.skills); err != nil {
			return fmt.Errorf("OpenCode skills: %w", ErrDenied)
		}
		if err := r.workload.Execute(ctx, []string{"/bin/sh", "-c", seal}); err != nil {
			return fmt.Errorf("OpenCode config directory: %w", ErrDenied)
		}
	}
	config, err := Config(r.connections, r.relayToken != "")
	if err != nil {
		return err
	}
	config, err = r.configureInference(ctx, config)
	if err != nil {
		return err
	}
	files := map[string][]byte{"/workspace/opencode.json": config}
	if brief != nil {
		files[BriefPath] = brief
	}
	// gh trusts the forge relay through SSL_CERT_DIR; the certificate is public.
	if r.gitEnv["SSL_CERT_DIR"] != "" {
		files[repo.CAPath] = r.adapter.GitRelay.CA()
	}
	for path, data := range files {
		if err := r.workload.Write(ctx, path, data); err != nil {
			return err
		}
	}
	// As upstream, an unwritable project file degrades to the brief, which lists the resources.
	if brief != nil && project != nil {
		_ = r.workload.Write(ctx, ProjectPath, project)
	}
	return nil
}

func (r *running) grant(action string) attempt.Grant {
	return attempt.Grant{Controller: r.adapter.Controller, Attempt: r.task.AttemptKey(), Server: r.adapter.Server, Workspace: r.task.WorkspaceID, Agent: r.task.AgentID, Task: r.task.ID, Action: action}
}

func (r *running) refresh(ctx context.Context) error {
	status, err := r.adapter.Status.Status(ctx, r.task.ID)
	if err != nil || status != "running" {
		return fmt.Errorf("task status: %w", ErrDenied)
	}
	ref := identity.Ref{Server: r.adapter.Server, WorkspaceID: r.task.WorkspaceID, AgentID: r.task.AgentID}
	for name, connection := range r.connections {
		r.mu.Lock()
		token := r.tokens[name]
		r.mu.Unlock()
		if time.Until(token.ExpiresAt) <= 10*time.Second {
			token, err = r.adapter.Issuer.AcquireForMCP(ctx, ref, r.task.Agent.CustomEnv, connection.URL)
			if err != nil || time.Until(token.ExpiresAt) <= 10*time.Second {
				return fmt.Errorf("MCP issuance: %w", ErrDenied)
			}
		}
		// An optional authority registers the JWT fingerprint before the relay uses it.
		if r.adapter.Authority != nil {
			grant := r.grant("renew")
			grant.URL = connection.URL
			grant.TokenHash = attempt.Fingerprint(token.Bearer())
			grant.ExpiresAt = min(token.ExpiresAt.Unix(), time.Now().Add(15*time.Second).Unix())
			if err := r.adapter.Authority.Apply(ctx, grant); err != nil {
				return err
			}
		}
		r.mu.Lock()
		r.tokens[name] = token
		r.mu.Unlock()
	}
	return ctx.Err()
}

func (r *running) loop(ctx context.Context) {
	defer close(r.exited)
	command := r.adapter.Command
	if len(command) == 0 {
		command = []string{"opencode", "acp"}
	}
	agent := make(chan error, 1)
	go func() {
		agent <- r.workload.Stream(ctx, command, r.environment(), func(stdin io.Writer, stdout io.Reader) error {
			return r.converse(ctx, stdin, stdout)
		})
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	// Polling stands in for upstream's workspaces-changed hint (ADR 0015).
	var settings <-chan time.Time
	if r.adapter.Checkout != nil {
		refresh := time.NewTicker(repo.SettingsRefresh)
		defer refresh.Stop()
		settings = refresh.C
	}
	var err error
	agentDone := false
loop:
	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()
			break loop
		case err = <-agent:
			agentDone = true
			break loop
		case <-ticker.C:
			if watchdog := r.adapter.idleWatchdog(); r.events.idle(watchdog) {
				err = &execution.IdleError{After: watchdog}
				break loop
			}
			if err = r.refresh(ctx); err != nil {
				break loop
			}
		case <-settings:
			r.adapter.Checkout.Refresh(ctx, r.task.AttemptKey())
		}
	}
	r.cancel()
	// Wait for the Docker exec client to exit before allowing container cleanup.
	if !agentDone {
		<-agent
	}
	r.done <- err
}

func (a *Adapter) idleWatchdog() time.Duration {
	if a.IdleWatchdog > 0 {
		return a.IdleWatchdog
	}
	return idleWatchdog
}

// Result reports the session as retained only with a retained workdir.
func (r *running) Result() execution.Result {
	t := execution.Telemetry{Model: r.model, Effort: r.effort}
	if peak, ok := r.workload.(interface{ PeakMemory() int64 }); ok {
		t.PeakMemory = peak.PeakMemory()
	}
	if r.events == nil {
		return execution.Result{Disposable: true, Telemetry: t}
	}
	output, session, u := r.events.result()
	t.InputTokens, t.OutputTokens, t.CachedTokens = u.Input, u.Output, u.CacheRead
	// A failed resume clears resume and continues in a new session.
	switch {
	case r.task.PriorSessionID == "":
		t.Continuity = "fresh"
	case r.resume != "":
		t.Continuity = "resumed"
	default:
		t.Continuity = "lost"
	}
	return execution.Result{Output: output, SessionID: session, WorkDir: r.workDir, RetiredSessionID: r.retired, Disposable: r.workDir == "", Telemetry: t}
}

func (r *running) Wait(ctx context.Context) error {
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *running) Remove(ctx context.Context) error {
	r.once.Do(func() {
		// Relay access ends first, before the agent process is joined; revocation cancels in-flight streams.
		if r.relayToken != "" {
			r.adapter.Relay.Revoke(r.task.AttemptKey())
		}
		if r.inferenceToken != "" {
			r.adapter.InferenceRelay.Revoke(r.task.AttemptKey())
		}
		if r.mcpToken != "" {
			r.adapter.MCPRelay.revoke(r.task.AttemptKey())
		}
		if r.gitEnv != nil {
			r.adapter.Checkout.Unregister(r.task.AttemptKey())
			r.adapter.GitRelay.Revoke(r.task.AttemptKey())
		}
		r.cancel()
		if r.started {
			<-r.exited
		}
		cleanCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if r.adapter.Authority != nil {
			r.cleanupErr = r.adapter.Authority.Apply(cleanCtx, r.grant("revoke"))
		}
		if r.workload != nil {
			r.cleanupErr = errors.Join(r.cleanupErr, r.workload.Remove(cleanCtx))
		}
		r.mu.Lock()
		clear(r.tokens)
		r.mu.Unlock()
	})
	return r.cleanupErr
}

func (r *running) reject(cause error) error {
	if err := r.Remove(context.Background()); err != nil {
		return errors.Join(cause, err)
	}
	return &execution.RejectedError{Err: cause}
}
