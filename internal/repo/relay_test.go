package repo

import (
	"bytes"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const workspace = "10000000-0000-4000-8000-000000000001"

// gitServer serves bare repositories under root with git-http-backend behind basic auth.
func gitServer(t *testing.T, root string, seen *[]string) *httptest.Server {
	core, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git unavailable")
	}
	backend := &cgi.Handler{Path: filepath.Join(strings.TrimSpace(string(core)), "git-http-backend"), Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*seen = append(*seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		if user, password, ok := r.BasicAuth(); !ok || user != "x-access-token" || password != "host-secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// git-http-backend needs CONTENT_LENGTH; buffer chunked fixture bodies.
		body, _ := io.ReadAll(r.Body)
		r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestGitRelayServesOnlyClaimRepositories(t *testing.T) {
	bare := origin(t)
	root := filepath.Dir(bare)
	git(t, root, "clone", "-q", "--bare", bare, filepath.Join(root, "other.git"))
	for _, name := range []string{"fixture.git", "other.git"} {
		git(t, filepath.Join(root, name), "config", "http.receivepack", "true")
	}
	var seen []string
	upstream := gitServer(t, root, &seen)
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte("host-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	hosts, err := NewHosts(Config{Version: 1, AllowHTTP: true, Hosts: []Host{
		{WorkspaceID: workspace, Host: "git.example.test", Upstream: upstream.URL, Username: "x-access-token", PasswordFile: secret},
		{WorkspaceID: "10000000-0000-4000-8000-000000000002", Host: "other.example.test", Upstream: upstream.URL},
	}})
	if err != nil {
		t.Fatal(err)
	}
	g := NewRelay("http://placeholder", hosts)
	handler, err := relay.New(g, relay.Policy{Allow: GitPath, Limit: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	g.URL = server.URL
	env, err := g.Issue("attempt", workspace, []string{"https://git.example.test/fixture.git", "https://other.example.test/other.git", "ssh://git.example.test/x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(values(env), " "), "host-secret") || env["GIT_CONFIG_COUNT"] != "3" {
		t.Fatalf("unexpected attempt Git environment: %v", env)
	}
	work := filepath.Join(t.TempDir(), "c")
	attempt := func(args ...string) error {
		cmd := exec.Command("git", append([]string{"-c", "user.name=A", "-c", "user.email=a@example.invalid"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		for name, value := range env {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Log(string(out))
		}
		return err
	}
	clone := func(url string) error { return attempt("clone", "-q", url, filepath.Join(t.TempDir(), "c")) }
	// The repository path has no .git suffix on disk lookups: the fixture is fixture.git.
	if err := attempt("clone", "-q", "https://git.example.test/fixture.git", work); err != nil {
		t.Fatal("claim repository clone failed:", err)
	}
	// Pushes follow the native runtime: any branch of a claim repository, nothing else.
	if err := attempt("-C", work, "commit", "-q", "--allow-empty", "-m", "agent"); err != nil {
		t.Fatal(err)
	}
	if err := attempt("-C", work, "push", "-q", "origin", "HEAD:refs/heads/agent/a/1", "HEAD:refs/heads/topic"); err != nil {
		t.Fatal("claim repository push failed:", err)
	}
	if git(t, bare, "rev-parse", "agent/a/1") != git(t, work, "rev-parse", "HEAD") {
		t.Fatal("pushed branch did not reach the upstream")
	}
	if attempt("-C", work, "push", "-q", "https://git.example.test/other.git", "HEAD:refs/heads/agent/a/1") == nil {
		t.Fatal("unlisted repository accepted a push")
	}
	for _, url := range []string{"https://git.example.test/other.git", "https://other.example.test/other.git"} {
		if clone(url) == nil {
			t.Fatalf("%s cloned without a grant", url)
		}
	}
	for _, request := range seen {
		if strings.Contains(request, "Bearer") || (strings.Contains(request, "other.git") && strings.Contains(request, "Basic")) {
			t.Fatalf("unexpected upstream request: %s", request)
		}
	}
	for name, r := range map[string]*http.Request{
		"unlisted push": httptest.NewRequest("POST", "/git.example.test/other.git/git-receive-pack", nil),
		"push refs GET": httptest.NewRequest("POST", "/git.example.test/fixture.git/info/refs?service=git-receive-pack", nil),
		"dumb":          httptest.NewRequest("GET", "/git.example.test/fixture.git/HEAD", nil),
		"extra query":   httptest.NewRequest("GET", "/git.example.test/fixture.git/info/refs?service=git-upload-pack&x=1", nil),
		"traversal":     httptest.NewRequest("GET", "/git.example.test/x/../fixture.git/info/refs?service=git-upload-pack", nil),
	} {
		r.Header.Set("Authorization", env["GIT_CONFIG_VALUE_0"][len("Authorization: "):])
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	g.Revoke("attempt")
	if clone("https://git.example.test/fixture.git") == nil || attempt("-C", work, "push", "-q", "origin", "HEAD:refs/heads/late") == nil {
		t.Fatal("revoked grant still clones or pushes")
	}
}

func TestHostConfigRequiresSafeBindings(t *testing.T) {
	valid := Host{WorkspaceID: workspace, Host: "github.com", Username: "x-access-token", PasswordFile: "/run/secrets/github"}
	if _, err := NewHosts(Config{Version: 1, Hosts: []Host{valid}}); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Host){
		"http upstream":   func(h *Host) { h.Upstream = "http://github.com" },
		"upstream path":   func(h *Host) { h.Upstream = "https://github.com/org" },
		"upstream user":   func(h *Host) { h.Upstream = "https://u:p@github.com" },
		"relative secret": func(h *Host) { h.PasswordFile = "secret" },
		"no username":     func(h *Host) { h.Username = "" },
		"host path":       func(h *Host) { h.Host = "github.com/org" },
		"upper host":      func(h *Host) { h.Host = "GitHub.com" },
		"workspace":       func(h *Host) { h.WorkspaceID = "default" },
		"name only":       func(h *Host) { h.CommitName = "Bot" },
		"email brackets":  func(h *Host) { h.CommitName, h.CommitEmail = "Bot", "<bot@example.invalid>" },
		"name newline":    func(h *Host) { h.CommitName, h.CommitEmail = "Bot\nX", "bot@example.invalid" },
	} {
		h := valid
		change(&h)
		if _, err := NewHosts(Config{Version: 1, Hosts: []Host{h}}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := NewHosts(Config{Version: 1, Hosts: []Host{valid, valid}}); err == nil {
		t.Error("duplicate binding accepted")
	}
	bot := valid
	bot.CommitName, bot.CommitEmail = "Bot", "bot@example.invalid"
	hosts, err := NewHosts(Config{Version: 1, Hosts: []Host{bot}})
	if err != nil {
		t.Fatal(err)
	}
	for url, want := range map[string]string{"https://github.com/o/r.git": "Bot bot@example.invalid", "https://gitlab.com/o/r": "Agent x " + DefaultEmail} {
		if name, email := hosts.Identity(workspace, url, "Agent <x>\n"); name+" "+email != want {
			t.Errorf("%s: %s %s", url, name, email)
		}
	}
}

func values(m map[string]string) []string {
	out := []string{}
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestDeployGitExampleDecodes(t *testing.T) {
	hosts, err := ReadConfig("../../deploy/git.example.json")
	if err != nil || !hosts.Has("10000000-0000-4000-8000-000000000001", "github.com") {
		t.Fatalf("example Git host configuration: %v", err)
	}
}
