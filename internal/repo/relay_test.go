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
	handler, err := relay.New(g, relay.Policy{Allow: GitPath, Limit: 1 << 20, Proxy: true})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	g.URL = server.URL
	env, err := g.Issue("attempt", workspace, []string{"https://git.example.test/fixture.git", "https://other.example.test/other.git", "ssh://deploy@git.example.test:2222/fixture.git", "http://git.example.test/fixture.git"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(values(env), " "), "host-secret") || env["GIT_CONFIG_COUNT"] != "6" {
		t.Fatalf("unexpected attempt Git environment: %v", env)
	}
	work := filepath.Join(t.TempDir(), "c")
	run := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=A", "-c", "user.email=a@example.invalid"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		for name, value := range env {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Log(string(out))
		}
		return strings.TrimSpace(string(out)), err
	}
	attempt := func(args ...string) error {
		_, err := run(args...)
		return err
	}
	clone := func(url string) error { return attempt("clone", "-q", url, filepath.Join(t.TempDir(), "c")) }
	// The repository path has no .git suffix on disk lookups: the fixture is fixture.git.
	if err := attempt("clone", "-q", "https://git.example.test/fixture.git", work); err != nil {
		t.Fatal("claim repository clone failed:", err)
	}
	// SSH remote forms of claim repositories go through the relay as well.
	for _, url := range []string{"git@git.example.test:fixture.git", "ssh://git@git.example.test/fixture.git", "ssh://deploy@git.example.test:2222/fixture.git", "http://git.example.test/fixture.git"} {
		if err := clone(url); err != nil {
			t.Fatalf("%s not rewritten to the relay: %v", url, err)
		}
	}
	// origin keeps its URL; gh reads the alias from git remote, still on the forge host.
	stored, _ := run("-C", work, "config", "remote.origin.url")
	if alias, _ := run("-C", work, "remote", "get-url", "origin"); stored != "https://git.example.test/fixture.git" || alias != "http://git.example.test/fixture.git" {
		t.Fatalf("remote URLs: %q %q", stored, alias)
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
	fetch := "/fixture.git/info/refs?service=git-upload-pack"
	for name, c := range map[string]struct {
		method, target string
		ok             bool
	}{
		"claim fetch":     {"GET", "http://git.example.test" + fetch, true},
		"unlisted push":   {"POST", "http://git.example.test/other.git/git-receive-pack", false},
		"push refs GET":   {"POST", "http://git.example.test/fixture.git/info/refs?service=git-receive-pack", false},
		"dumb":            {"GET", "http://git.example.test/fixture.git/HEAD", false},
		"extra query":     {"GET", "http://git.example.test" + fetch + "&x=1", false},
		"traversal":       {"GET", "http://git.example.test/x/.." + fetch, false},
		"origin form":     {"GET", fetch, false},
		"https target":    {"GET", "https://git.example.test" + fetch, false},
		"userinfo":        {"GET", "http://u@git.example.test" + fetch, false},
		"other workspace": {"GET", "http://other.example.test/other.git/info/refs?service=git-upload-pack", false},
	} {
		r := httptest.NewRequest(c.method, c.target, nil)
		r.Header.Set("Authorization", env["GIT_CONFIG_VALUE_0"][len("Authorization: "):])
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if (w.Code == http.StatusOK) != c.ok {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	// A workspace repository joins the live grant on checkout; other hosts and attempts do not.
	if _, _, ok := g.Allow("other-attempt", "https://git.example.test/other.git"); ok {
		t.Fatal("unknown attempt extended")
	}
	if _, _, ok := g.Allow("attempt", "https://other.example.test/other.git"); ok {
		t.Fatal("grant extended to another workspace's host")
	}
	if alias, prefix, ok := g.Allow("attempt", "git@git.example.test:other.git"); !ok || alias != "http://git.example.test/" || prefix != "" {
		t.Fatalf("workspace repository grant: %q %q %v", alias, prefix, ok)
	}
	if err := clone("https://git.example.test/other.git"); err != nil {
		t.Fatal("granted workspace repository clone failed:", err)
	}
	if _, prefix, _ := g.Allow("attempt", "ssh://ops@git.example.test:2200/other.git"); prefix != "ssh://ops@git.example.test:2200/" {
		t.Fatalf("unrewritten prefix not returned: %q", prefix)
	}
	g.Revoke("attempt")
	if clone("https://git.example.test/fixture.git") == nil || attempt("-C", work, "push", "-q", "origin", "HEAD:refs/heads/late") == nil {
		t.Fatal("revoked grant still clones or pushes")
	}
	if _, _, ok := g.Allow("attempt", "https://git.example.test/other.git"); ok {
		t.Fatal("revoked attempt extended")
	}
}

func TestClaimURLFormsMatchUpstream(t *testing.T) {
	for raw, key := range map[string]string{
		"https://Git.Example.test/Team/Repo.git/":    "git.example.test/Team/Repo",
		"https://git.example.test:8443/team/repo":    "git.example.test:8443/team/repo",
		"http://git.example.test/team/repo.git":      "git.example.test/team/repo",
		"ssh://git@git.example.test/team/repo.git":   "git.example.test/team/repo",
		"ssh://deploy@git.example.test:22/team/repo": "git.example.test/team/repo",
		"git://git.example.test/team/repo.git":       "git.example.test/team/repo",
		"git@git.example.test:team/repo.git":         "git.example.test/team/repo",
		"git.example.test:team/repo":                 "git.example.test/team/repo",
	} {
		if _, got, _, ok := repoKey(raw); !ok || got != key {
			t.Errorf("%s: key %q, %v; want %q", raw, got, ok, key)
		}
	}
	for _, raw := range []string{
		"https://user@git.example.test/team/repo", "ssh://git:secret@git.example.test/team/repo", "https://git.example.test/team/repo?token=x",
		"https://git.example.test/team/repo#x", "file:///srv/repo", "ftp://git.example.test/team/repo", "git@git.example.test:/srv/repo",
		"git@git.example.test:../repo", "https://git.example.test/team/../repo", "https://git.example.test/re\npo", "git@git.example.test:team/re\npo",
		"git@:team/repo", "a@b@git.example.test:team/repo", "C:\\repo", "repo",
	} {
		if ValidURL(raw) {
			t.Errorf("%q accepted", raw)
		}
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
