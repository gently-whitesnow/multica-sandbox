package repo

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestGitRelayServesBoundHostRepositories(t *testing.T) {
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
	// Pushes follow the native runtime: any branch of any repository on a bound host.
	if err := attempt("-C", work, "commit", "-q", "--allow-empty", "-m", "agent"); err != nil {
		t.Fatal(err)
	}
	if err := attempt("-C", work, "push", "-q", "origin", "HEAD:refs/heads/agent/a/1", "HEAD:refs/heads/topic"); err != nil {
		t.Fatal("claim repository push failed:", err)
	}
	if git(t, bare, "rev-parse", "agent/a/1") != git(t, work, "rev-parse", "HEAD") {
		t.Fatal("pushed branch did not reach the upstream")
	}
	if err := attempt("-C", work, "push", "-q", "https://git.example.test/other.git", "HEAD:refs/heads/agent/a/1"); err != nil {
		t.Fatal("unlisted repository push failed:", err)
	}
	if err := clone("git@git.example.test:other.git"); err != nil {
		t.Fatal("unlisted repository clone failed:", err)
	}
	// The other workspace's host is not proxied, so the clone never reaches its upstream.
	before := len(seen)
	if clone("https://other.example.test/other.git") == nil || len(seen) != before {
		t.Fatal("another workspace's host cloned through the relay")
	}
	for _, request := range seen {
		if strings.Contains(request, "Bearer") {
			t.Fatalf("relay credential reached the upstream: %s", request)
		}
	}
	fetch := "/fixture.git/info/refs?service=git-upload-pack"
	for name, c := range map[string]struct {
		method, target string
		ok             bool
	}{
		"claim fetch":     {"GET", "http://git.example.test" + fetch, true},
		"unlisted fetch":  {"GET", "http://git.example.test/other.git/info/refs?service=git-upload-pack", true},
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
	// Checkout of a bound host's repository returns its alias; other hosts and attempts do not.
	if _, _, ok := g.Allow("other-attempt", "https://git.example.test/other.git"); ok {
		t.Fatal("unknown attempt extended")
	}
	if _, _, ok := g.Allow("attempt", "https://other.example.test/other.git"); ok {
		t.Fatal("grant extended to another workspace's host")
	}
	if alias, prefix, ok := g.Allow("attempt", "git@git.example.test:other.git"); !ok || alias != "http://git.example.test/" || prefix != "" {
		t.Fatalf("workspace repository grant: %q %q %v", alias, prefix, ok)
	}
	if _, prefix, _ := g.Allow("attempt", "ssh://ops@git.example.test:2200/other.git"); prefix != "ssh://ops@git.example.test:2200/" {
		t.Fatalf("unrewritten prefix not returned: %q", prefix)
	}
	g.Revoke("attempt")
	if clone("https://git.example.test/fixture.git") == nil || attempt("-C", work, "push", "-q", "origin", "HEAD:refs/heads/late") == nil {
		t.Fatal("revoked grant still clones or pushes")
	}
	if _, _, ok := g.Allow("attempt", "https://git.example.test/other.git"); ok {
		t.Fatal("revoked attempt admitted")
	}
}

// Checkout admits repositories while the attempt fetches; run under -race.
func TestGitRelayAllowsDuringAuthorize(t *testing.T) {
	hosts, err := NewHosts(Config{Version: 1, Hosts: []Host{{WorkspaceID: workspace, Host: "git.example.test"}}})
	if err != nil {
		t.Fatal(err)
	}
	g := NewRelay("http://relay.example.test", hosts)
	env, err := g.Issue("attempt", workspace, []string{"https://git.example.test/fixture.git"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 200 {
			g.Allow("attempt", "https://git.example.test/repo"+strconv.Itoa(i)+".git")
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			r := httptest.NewRequest(http.MethodPost, "http://git.example.test/fixture.git/git-upload-pack", nil)
			r.Header.Set("Authorization", env["GIT_CONFIG_VALUE_0"][len("Authorization: "):])
			if lease, ok := g.Authorize(r); !ok {
				t.Error("bound host repository refused")
			} else {
				lease.Release()
			}
		}
	}()
	wg.Wait()
}

// A live grant's refused request is a policy or host credential refusal, not an invalid
// relay credential: only unknown and revoked credentials answer 401.
func TestGitRelayClassifiesRefusals(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "token")
	hosts, err := NewHosts(Config{Version: 1, AllowHTTP: true, Hosts: []Host{
		{WorkspaceID: workspace, Host: "git.example.test", Upstream: "http://127.0.0.1:1", Username: "x-access-token", PasswordFile: secret},
		{WorkspaceID: "10000000-0000-4000-8000-000000000002", Host: "other.example.test", Upstream: "http://127.0.0.1:1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	g := NewRelay("http://relay.example.test", hosts)
	var outcomes []error
	handler, err := relay.New(g, relay.Policy{Allow: GitPath, Limit: 1 << 20, Proxy: true, Record: func(_ *http.Request, _ string, _ int, err error, _ time.Duration) {
		outcomes = append(outcomes, err)
	}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := g.Issue("attempt", workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential := env["GIT_CONFIG_VALUE_0"][len("Authorization: "):]
	fetch := "/fixture.git/info/refs?service=git-upload-pack"
	for name, c := range map[string]struct {
		method, target, credential string
		status                     int
		err                        error
	}{
		"push refs by POST":    {"POST", "http://git.example.test/fixture.git/info/refs?service=git-receive-pack", credential, http.StatusForbidden, relay.ErrPolicy},
		"extra query":          {"GET", "http://git.example.test" + fetch + "&x=1", credential, http.StatusForbidden, relay.ErrPolicy},
		"other workspace host": {"GET", "http://other.example.test" + fetch, credential, http.StatusForbidden, relay.ErrPolicy},
		"host secret missing":  {"GET", "http://git.example.test" + fetch, credential, http.StatusBadGateway, relay.ErrUpstreamCredential},
		"unknown credential":   {"GET", "http://git.example.test" + fetch, "Bearer " + GitPrefix + strings.Repeat("0", 64), http.StatusUnauthorized, relay.ErrCredential},
	} {
		outcomes = nil
		r := httptest.NewRequest(c.method, c.target, nil)
		r.Header.Set("Authorization", c.credential)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != c.status || len(outcomes) != 1 || !errors.Is(outcomes[0], c.err) {
			t.Errorf("%s: %d %v, want %d %v", name, w.Code, outcomes, c.status, c.err)
		}
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
	// The global identity is the one the workspace's hosts agree on, else the agent's.
	quoted := bot
	quoted.Host, quoted.CommitName = "gitlab.com", `Bot "q" \ x`
	other := quoted
	other.WorkspaceID = "10000000-0000-4000-8000-000000000002"
	for name, c := range map[string]struct {
		hosts []Host
		want  string
	}{
		"agreed":    {[]Host{bot, func() Host { h := bot; h.Host = "gitlab.com"; return h }()}, "Bot bot@example.invalid"},
		"differ":    {[]Host{bot, quoted}, "Agent x " + DefaultEmail},
		"none":      {[]Host{valid}, "Agent x " + DefaultEmail},
		"workspace": {[]Host{valid, other}, "Agent x " + DefaultEmail},
		"quoted":    {[]Host{quoted}, `Bot "q" \ x bot@example.invalid`},
	} {
		hosts, err := NewHosts(Config{Version: 1, Hosts: c.hosts})
		if err != nil {
			t.Fatal(name, err)
		}
		file := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(file, NewRelay("http://relay.example.test", hosts).GitConfig(workspace, "Agent <x>\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if got := git(t, filepath.Dir(file), "config", "-f", file, "user.name") + " " + git(t, filepath.Dir(file), "config", "-f", file, "user.email"); got != c.want {
			t.Errorf("%s: %s", name, got)
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
