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
