package repo

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

// createPR is the mutation gh 2.46 pr create sends after its RepositoryInfo and
// PullRequestForBranch queries, all as POST to the GraphQL endpoint.
const createPR = `{"query":"mutation PullRequestCreate($input: CreatePullRequestInput!) {createPullRequest(input: $input) {pullRequest {id url}}}","variables":{"input":{"baseRefName":"main","headRefName":"agent/a/1","repositoryId":"R_1","title":"Work"}}}`

func TestForgeRelaysGhToTheWorkspaceAPI(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization")+" "+r.Host+" "+string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer api.Close()
	// requests returns and clears what the upstream received.
	requests := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := seen
		seen = nil
		return out
	}
	dir := t.TempDir()
	secret := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	other := "10000000-0000-4000-8000-000000000002"
	hosts, err := NewHosts(Config{Version: 1, AllowHTTP: true, Hosts: []Host{
		{WorkspaceID: workspace, Host: "git.example.test", Username: "x-access-token", PasswordFile: secret("host-secret"), API: api.URL},
		{WorkspaceID: other, Host: "github.com", Username: "x-access-token", PasswordFile: secret("hub-secret"), API: api.URL},
	}})
	if err != nil {
		t.Fatal(err)
	}
	g := NewRelay("http://git-relay:8093", hosts)
	forge, err := NewForge(g)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := relay.New(forge, relay.Policy{Allow: ForgePath, Limit: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = forge.TLSConfig()
	server.StartTLS()
	defer server.Close()
	env, err := g.Issue("attempt", workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	if env["GH_TOKEN"] == "" || env["GH_TOKEN"] != env["GH_ENTERPRISE_TOKEN"] || env["GH_HOST"] != "git.example.test" || env["SSL_CERT_DIR"] != CertDir {
		t.Fatalf("gh environment: %v", env)
	}
	hub, err := g.Issue("hub", other, nil)
	if err != nil || hub["GH_HOST"] != "github.com" {
		t.Fatalf("github.com gh environment: %v %v", hub, err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(g.CA())
	call := func(name, host, path, token string) (int, error) {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: name},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}}}
		r, _ := http.NewRequest(http.MethodPost, "https://"+host+path, strings.NewReader(createPR))
		r.Header.Set("Authorization", "token "+token)
		resp, err := client.Do(r)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	upstream := api.Listener.Addr().String() + " " + createPR
	for _, c := range []struct{ name, path, token, want string }{
		{"git.example.test", "/api/v3/repos/team/fixture/pulls?per_page=1", env["GH_TOKEN"], "POST /api/v3/repos/team/fixture/pulls?per_page=1 token host-secret "},
		{"git.example.test", "/api/graphql", env["GH_TOKEN"], "POST /api/graphql token host-secret "},
		{"api.github.com", "/graphql", hub["GH_TOKEN"], "POST /graphql token hub-secret "},
	} {
		code, err := call(c.name, c.name, c.path, c.token)
		if got := requests(); err != nil || code != http.StatusCreated || len(got) != 1 || got[0] != c.want+upstream {
			t.Fatalf("%s%s: %d %v %v", c.name, c.path, code, err, got)
		}
	}
	denials := map[string]struct{ name, host, path, token string }{
		"other workspace API":       {"api.github.com", "api.github.com", "/graphql", env["GH_TOKEN"]},
		"other workspace GHES":      {"git.example.test", "git.example.test", "/api/graphql", hub["GH_TOKEN"]},
		"Enterprise root GraphQL":   {"git.example.test", "git.example.test", "/graphql", env["GH_TOKEN"]},
		"Host differs from SNI":     {"git.example.test", "api.github.com", "/graphql", env["GH_TOKEN"]},
		"SNI differs from Host":     {"api.github.com", "git.example.test", "/api/graphql", hub["GH_TOKEN"]},
		"not an API path":           {"git.example.test", "git.example.test", "/team/fixture.git/info/refs", env["GH_TOKEN"]},
		"Multica credential":        {"git.example.test", "git.example.test", "/api/graphql", "mat_relay_x"},
		"traversal to GraphQL root": {"git.example.test", "git.example.test", "/api/../graphql", env["GH_TOKEN"]},
	}
	for name, c := range denials {
		if code, err := call(c.name, c.host, c.path, c.token); err != nil || code < 400 {
			t.Errorf("%s: %d %v", name, code, err)
		}
	}
	if _, err := call("example.com", "example.com", "/graphql", env["GH_TOKEN"]); err == nil {
		t.Error("unconfigured name got a certificate")
	}
	g.Revoke("attempt")
	g.Revoke("hub")
	for _, c := range [][3]string{{"git.example.test", "/api/graphql", env["GH_TOKEN"]}, {"api.github.com", "/graphql", hub["GH_TOKEN"]}} {
		if code, _ := call(c[0], c[0], c[1], c[2]); code != http.StatusUnauthorized {
			t.Errorf("revoked %s grant: %d", c[0], code)
		}
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("denied calls reached the upstream: %v", got)
	}
}

// glab 1.99.0 sends PRIVATE-TOKEN to /api/v4 and /api/graphql. `glab mr create` reads the
// project by its encoded path, then posts the merge request; `glab api` passes paths through.
const (
	glabProject = "/api/v4/projects/team%2Ffixture?license=true&with_custom_attributes=true"
	glabCreate  = `{"title":"Work","description":"Body","source_branch":"agent/a/1","target_branch":"main","labels":"","target_project_id":7}`
)

func TestForgeRelaysGlabToGitLab(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, strings.Join([]string{r.Method, r.URL.RequestURI(), r.Header.Get("Private-Token"), r.Header.Get("Authorization"), string(body)}, " "))
		mu.Unlock()
	}))
	defer api.Close()
	requests := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := seen
		seen = nil
		return out
	}
	dir := t.TempDir()
	secret := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	other := "10000000-0000-4000-8000-000000000002"
	hosts, err := NewHosts(Config{Version: 1, AllowHTTP: true, Hosts: []Host{
		{WorkspaceID: workspace, Host: "gitlab.example.test", Username: "bot", PasswordFile: secret("lab-secret"), API: api.URL, Forge: GitLab},
		{WorkspaceID: other, Host: "code.example.test", Username: "bot", PasswordFile: secret("bearer-secret"), API: api.URL, Forge: GitLab, APIAuth: "bearer"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	g := NewRelay("http://git-relay:8093", hosts)
	forge, err := NewForge(g)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := relay.New(forge, relay.Policy{Allow: ForgePath, Limit: 1 << 20, EncodedSlash: true})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = forge.TLSConfig()
	server.StartTLS()
	defer server.Close()
	env, err := g.Issue("attempt", workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	if env["GITLAB_TOKEN"] == "" || env["GITLAB_HOST"] != "gitlab.example.test" || env["GLAB_NO_PROMPT"] != "true" || env["GLAB_CHECK_UPDATE"] != "false" ||
		env["GLAB_SEND_TELEMETRY"] != "false" || env["SSL_CERT_DIR"] != CertDir || env["GH_TOKEN"] != "" {
		t.Fatalf("glab environment: %v", env)
	}
	bearer, err := g.Issue("bearer", other, nil)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(g.CA())
	call := func(name, method, target string, header http.Header, body string) int {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: name},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}}}
		r, _ := http.NewRequest(method, "https://"+name+target, strings.NewReader(body))
		r.Header = header
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	token := func(v string) http.Header { return http.Header{"Private-Token": {v}} }
	for _, c := range []struct{ name, method, target, body, want string }{
		{"gitlab.example.test", "GET", glabProject, "", "GET " + glabProject + " lab-secret  "},
		{"gitlab.example.test", "POST", "/api/v4/projects/team%2Ffixture/merge_requests", glabCreate, "POST /api/v4/projects/team%2Ffixture/merge_requests lab-secret  " + glabCreate},
		{"gitlab.example.test", "POST", "/api/graphql", `{"query":"{currentUser{username}}"}`, `POST /api/graphql lab-secret  {"query":"{currentUser{username}}"}`},
		// Merging and creating tokens are left to the token's role and scope.
		{"gitlab.example.test", "PUT", "/api/v4/projects/team%2Ffixture/merge_requests/1/merge", "", "PUT /api/v4/projects/team%2Ffixture/merge_requests/1/merge lab-secret  "},
		{"gitlab.example.test", "POST", "/api/v4/projects/team%2Ffixture/access_tokens", "", "POST /api/v4/projects/team%2Ffixture/access_tokens lab-secret  "},
		{"gitlab.example.test", "GET", "/api/v4/personal_access_tokens/self", "", "GET /api/v4/personal_access_tokens/self lab-secret  "},
		{"gitlab.example.test", "GET", "/api/v4/projects/team%2Frotate/access_tokens", "", "GET /api/v4/projects/team%2Frotate/access_tokens lab-secret  "},
	} {
		if code, got := call(c.name, c.method, c.target, token(env["GITLAB_TOKEN"]), c.body), requests(); code != http.StatusOK || len(got) != 1 || got[0] != c.want {
			t.Fatalf("%s %s: %d %q", c.method, c.target, code, got)
		}
	}
	if code, got := call("code.example.test", "GET", "/api/v4/user", http.Header{"Authorization": {"Bearer " + bearer["GITLAB_TOKEN"]}}, ""), requests(); code != http.StatusOK || len(got) != 1 || got[0] != "GET /api/v4/user  Bearer bearer-secret " {
		t.Fatalf("bearer host: %d %q", code, got)
	}
	for name, c := range map[string]struct {
		host, method, target string
		header               http.Header
		want                 int
	}{
		"encoded traversal":     {"gitlab.example.test", "GET", "/api/v4/projects/team%2F..%2Fother", token(env["GITLAB_TOKEN"]), http.StatusForbidden},
		"encoded dot":           {"gitlab.example.test", "GET", "/api/v4/projects/%2e%2e", token(env["GITLAB_TOKEN"]), http.StatusForbidden},
		"not an API path":       {"gitlab.example.test", "GET", "/team/fixture.git/info/refs", token(env["GITLAB_TOKEN"]), http.StatusUnauthorized},
		"two credentials":       {"gitlab.example.test", "GET", "/api/v4/user", http.Header{"Private-Token": {env["GITLAB_TOKEN"]}, "Authorization": {"Bearer " + env["GITLAB_TOKEN"]}}, http.StatusUnauthorized},
		"other workspace token": {"gitlab.example.test", "GET", "/api/v4/user", token(bearer["GITLAB_TOKEN"]), http.StatusUnauthorized},
		"job token":             {"gitlab.example.test", "GET", "/api/v4/user", http.Header{"Job-Token": {env["GITLAB_TOKEN"]}}, http.StatusUnauthorized},
	} {
		if code := call(c.host, c.method, c.target, c.header, ""); code != c.want {
			t.Errorf("%s: %d, want %d", name, code, c.want)
		}
	}
	// Rotation returns the successor of the relay's token and revokes the password file.
	for _, target := range []string{
		"/api/v4/personal_access_tokens/self/rotate",
		"/api/v4/personal_access_tokens/42/rotate",
		"/api/v4/groups/team/access_tokens/self/rotate",
		"/api/v4/projects/team%2Ffixture/access_tokens/self/rotate",
		"/api/v4/projects/7/access_tokens/42/rotate",
		"/api/v4/groups/7/service_accounts/9/personal_access_tokens/42/rotate",
		"/api/v4/personal_access_tokens/self/r%6ftate",
		"/api/v4/personal_access_tokens/self/rotate/",
		"/api/v4/personal_access_tokens/self/rotate.json",
		"/api/v4/personal_access_tokens/self/rotate.xml",
		"/api/v4/Personal_Access_Tokens/SELF/Rotate",
		"/api/v4/p%65rsonal_access_tokens/self/rotate?expires_at=2030-01-01",
	} {
		for _, method := range []string{"POST", "GET", "PUT"} {
			if code := call("gitlab.example.test", method, target, token(env["GITLAB_TOKEN"]), ""); code != http.StatusForbidden {
				t.Errorf("%s %s: %d", method, target, code)
			}
		}
	}
	g.Revoke("attempt")
	if code := call("gitlab.example.test", "GET", "/api/v4/user", token(env["GITLAB_TOKEN"]), ""); code != http.StatusUnauthorized {
		t.Errorf("revoked grant: %d", code)
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("denied calls reached the upstream: %v", got)
	}
}

func TestForgeConfigRejectsBadGitLabPolicy(t *testing.T) {
	base := Host{WorkspaceID: workspace, Host: "gitlab.example.test", Username: "bot", PasswordFile: "/run/secrets/lab", API: "https://gitlab.example.test", Forge: GitLab}
	for name, change := range map[string]func(*Host){
		"unknown forge": func(h *Host) { h.Forge = "gitea" },
		"github auth":   func(h *Host) { h.Forge, h.APIAuth = GitHub, "bearer" },
		"auth":          func(h *Host) { h.APIAuth = "basic" },
		"no api":        func(h *Host) { h.API = "" },
		"github.com":    func(h *Host) { h.Host = "github.com" },
	} {
		host := base
		change(&host)
		if _, err := NewHosts(Config{Version: 1, Hosts: []Host{host}}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := NewHosts(Config{Version: 1, Hosts: []Host{base}}); err != nil {
		t.Fatal(err)
	}
}
