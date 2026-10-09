package repo

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

var (
	//go:embed coauthor.sh
	coauthorScript string
	//go:embed checkout.sh
	checkoutScript string
	script         = coauthorScript + checkoutScript
	// hookText is upstream's gated prepare-commit-msg hook; it reads StatePath per commit.
	//go:embed prepare-commit-msg
	hookText string
)

// busyWait is upstream's lock wait before a retryable 503.
var busyWait = 10 * time.Second

const (
	// WorkDir is the attempt's workdir volume and agent working directory; checkouts land inside it.
	WorkDir = "/workspace/work"
	// DaemonPort is the upstream default daemon port; the helper forwards it on loopback.
	DaemonPort = "19514"
	// StatePath holds the published Co-authored-by setting in the attempt's tmpfs.
	StatePath = "/workspace/.multica_co_authored_by"
	// SettingsRefresh bounds how long a live attempt's hooks lag a setting change.
	SettingsRefresh = 10 * time.Second
)

// Settings reads the workspace Co-authored-by setting (upstream workspaceCoAuthoredByEnabled).
type Settings interface {
	CoAuthoredBy(context.Context, string) (bool, error)
}

// Target runs controller-owned commands as the attempt user (execution.ProjectedRun).
type Target interface {
	Capture(context.Context, []string, map[string]string) ([]byte, error)
}

// Task is the trusted claim context of an active attempt.
type Task struct {
	Workspace, ID, AgentName string
	// Repos maps exact claim URLs to their claim ref.
	Repos  map[string]string
	Target Target
	// Env is the attempt's Git relay environment.
	Env map[string]string
}

// Authorizer resolves the attempt of an opaque Multica relay credential.
type Authorizer interface {
	Authorize(*http.Request) (relay.Lease, bool)
}

// Checkout serves the upstream daemon's POST /repo/checkout for registered attempts.
// Hosts supplies commit identities and Settings the Co-authored-by setting.
type Checkout struct {
	Auth     Authorizer
	Hosts    *Hosts
	Settings Settings
	mu       sync.Mutex
	tasks    map[string]*active
}

type active struct {
	Task
	busy chan struct{}
	// publish orders setting publication with checkouts; coAuthor was last published.
	publish             sync.Mutex
	coAuthor, published bool
}

var flag = map[bool]string{false: "0", true: "1"}

func (c *Checkout) Register(attempt string, t Task) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tasks == nil {
		c.tasks = map[string]*active{}
	}
	c.tasks[attempt] = &active{Task: t, busy: make(chan struct{}, 1), coAuthor: true}
}

// Refresh republishes the setting to a live attempt's hooks, as upstream does after each
// settings refresh. A checkout in flight publishes itself, so Refresh then skips.
func (c *Checkout) Refresh(ctx context.Context, attempt string) {
	c.mu.Lock()
	task := c.tasks[attempt]
	c.mu.Unlock()
	if task != nil && task.publish.TryLock() {
		defer task.publish.Unlock()
		c.setting(ctx, task)
	}
}

// setting rereads the setting, keeping the last value (enabled when unknown) if the read
// fails. On change it writes StatePath and reconciles existing checkouts (upstream
// persistCoAuthoredByState); a failed write is retried on the next call.
func (c *Checkout) setting(ctx context.Context, t *active) bool {
	enabled := t.coAuthor
	if c.Settings != nil {
		if value, err := c.Settings.CoAuthoredBy(ctx, t.Workspace); err == nil {
			enabled = value
		}
	}
	if !t.published || enabled != t.coAuthor {
		_, err := t.Target.Capture(ctx, []string{"/bin/sh", "-c", coauthorScript + `publish "$@"`, "publish", flag[enabled], StatePath, WorkDir, hookText}, nil)
		t.coAuthor, t.published = enabled, err == nil
	}
	return enabled
}

func (c *Checkout) Unregister(attempt string) {
	c.mu.Lock()
	delete(c.tasks, attempt)
	c.mu.Unlock()
}

type request struct {
	URL          string `json:"url"`
	WorkspaceID  string `json:"workspace_id"`
	WorkDir      string `json:"workdir"`
	Ref          string `json:"ref"`
	AgentName    string `json:"agent_name"`
	TaskID       string `json:"task_id"`
	CheckoutMode string `json:"checkout_mode"`
	RetryBusy    bool   `json:"retry_busy"`
	Fresh        bool   `json:"fresh"`
}

type response struct {
	Path             string `json:"path"`
	BranchName       string `json:"branch_name"`
	Kept             string `json:"kept,omitempty"`
	UncommittedFiles int    `json:"uncommitted_files,omitempty"`
	UnpushedCommits  int    `json:"unpushed_commits,omitempty"`
}

var unsafeRef = regexp.MustCompile(`[\x00-\x20\x7f~^:?*\[\\]|\.\.|@\{|^[-/]|/$|\.lock$`)

// ServeHTTP follows upstream validation order and messages (health.go repoCheckoutHandler).
func (c *Checkout) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	lease, ok := c.Auth.Authorize(r)
	if !ok {
		http.Error(w, "repo checkout credential is not bound to a task running in this daemon: the task it belongs to has already finished, or the daemon restarted after this agent started. Repo checkout is only available while the task that owns this workdir is still running.", http.StatusUnauthorized)
		return
	}
	defer lease.Release()
	c.mu.Lock()
	task := c.tasks[lease.Attempt]
	c.mu.Unlock()
	if task == nil {
		http.Error(w, "repo checkout is unavailable for this task", http.StatusUnauthorized)
		return
	}
	var req request
	if err := json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	switch {
	case req.URL == "":
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	case req.WorkspaceID == "":
		http.Error(w, "workspace_id is required", http.StatusBadRequest)
		return
	case req.WorkDir == "":
		http.Error(w, "workdir is required", http.StatusBadRequest)
		return
	case req.CheckoutMode != "" && req.CheckoutMode != "isolated":
		http.Error(w, "invalid checkout_mode", http.StatusBadRequest)
		return
	case req.WorkspaceID != task.Workspace || req.TaskID != task.ID:
		http.Error(w, "repo checkout task context does not match the active task", http.StatusForbidden)
		return
	case !path.IsAbs(req.WorkDir) || path.Clean(req.WorkDir) != req.WorkDir || (req.WorkDir != WorkDir && !strings.HasPrefix(req.WorkDir, WorkDir+"/")) || len(req.WorkDir) > 1024:
		http.Error(w, "repo checkout workdir is not owned by the active task: "+strconv.Quote(req.WorkDir)+" is outside the active task workdir "+WorkDir, http.StatusForbidden)
		return
	}
	claimRef, allowed := task.Repos[req.URL]
	name := Name(req.URL)
	if !allowed || name == "." || name == ".." {
		http.Error(w, "repo is not configured for this workspace", http.StatusBadRequest)
		return
	}
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		ref = strings.TrimSpace(claimRef)
	}
	if ref != "" && (unsafeRef.MatchString(ref) || len(ref) > 256) {
		http.Error(w, "invalid ref", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	defer context.AfterFunc(lease.Context, cancel)()
	wait := ctx
	if req.RetryBusy {
		var stop context.CancelFunc
		wait, stop = context.WithTimeout(ctx, busyWait)
		defer stop()
	}
	select {
	case task.busy <- struct{}{}:
		defer func() { <-task.busy }()
	case <-wait.Done():
		if ctx.Err() == nil {
			w.Header().Set("X-Multica-Retryable", "repo-busy")
			w.Header().Set("Retry-After", "2")
			http.Error(w, "repository is busy with another operation; retry later", http.StatusServiceUnavailable)
		}
		return
	}
	// Like upstream ensureRepoReady, every checkout rereads the setting first.
	task.publish.Lock()
	defer task.publish.Unlock()
	commitName, commitEmail := c.Hosts.Identity(task.Workspace, req.URL, task.AgentName)
	args := []string{"/bin/sh", "-c", script, "checkout", req.URL, ref, Branch(task.AgentName, task.ID), flag[req.Fresh], req.WorkDir, name, WorkDir, commitName, commitEmail, flag[c.setting(ctx, task)], hookText}
	out, _ := task.Target.Capture(ctx, args, task.Env)
	if ctx.Err() != nil {
		return
	}
	result, kind, message := parse(out)
	switch {
	case kind == "forbidden":
		http.Error(w, "repo checkout workdir is not owned by the active task: "+message, http.StatusForbidden)
	case kind != "":
		http.Error(w, message, http.StatusInternalServerError)
	case result.Path == "":
		http.Error(w, "repo checkout failed", http.StatusInternalServerError)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}

// parse reads the script's result lines; their values are attempt-controlled data.
func parse(out []byte) (response, string, string) {
	var result response
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		key, value, _ := strings.Cut(scanner.Text(), " ")
		switch key {
		case "error":
			kind, message, _ := strings.Cut(value, " ")
			return response{}, kind, message
		case "path":
			result.Path = value
		case "branch":
			result.BranchName = value
		case "kept":
			result.Kept = value
		case "uncommitted":
			result.UncommittedFiles, _ = strconv.Atoi(value)
		case "unpushed":
			result.UnpushedCommits, _ = strconv.Atoi(value)
		}
	}
	return result, "", ""
}

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)

// Branch is upstream's agent/<sanitized agent name>/<last 12 task characters>.
func Branch(agent, task string) string {
	name := strings.Trim(nonAlphanumeric.ReplaceAllString(strings.ToLower(strings.TrimSpace(agent)), "-"), "-")
	if len(name) > 30 {
		name = strings.TrimRight(name[:30], "-")
	}
	if name == "" {
		name = "agent"
	}
	key := strings.ReplaceAll(task, "-", "")
	return "agent/" + name + "/" + key[max(0, len(key)-12):]
}

// Name is upstream's checkout directory name for a repository URL.
func Name(raw string) string {
	name := strings.TrimSuffix(strings.TrimRight(raw, "/"), ".git")
	name = name[strings.LastIndex(name, "/")+1:]
	name = strings.TrimSpace(name[strings.LastIndex(name, ":")+1:])
	if name == "" {
		return "repo"
	}
	return name
}
