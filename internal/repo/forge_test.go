package repo

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

func TestForgeRelaysGhToTheWorkspaceAPI(t *testing.T) {
	var seen []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization")+" "+r.Host)
		w.WriteHeader(http.StatusCreated)
	}))
	defer api.Close()
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte("host-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	other := "10000000-0000-4000-8000-000000000002"
	hosts, err := NewHosts(Config{Version: 1, AllowHTTP: true, Hosts: []Host{
		{WorkspaceID: workspace, Host: "git.example.test", Username: "x-access-token", PasswordFile: secret, API: api.URL},
		{WorkspaceID: other, Host: "github.com", Username: "x-access-token", PasswordFile: secret, API: api.URL},
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
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(g.CA())
	call := func(name, host, path, token string) (int, error) {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: name},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}}}
		r, _ := http.NewRequest(http.MethodPost, "https://"+host+path, nil)
		r.Header.Set("Authorization", "token "+token)
		resp, err := client.Do(r)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	if code, err := call("git.example.test", "git.example.test", "/api/v3/repos/team/fixture/pulls?per_page=1", env["GH_TOKEN"]); err != nil || code != http.StatusCreated {
		t.Fatalf("gh call: %d %v", code, err)
	}
	if len(seen) != 1 || seen[0] != "POST /api/v3/repos/team/fixture/pulls?per_page=1 token host-secret "+api.Listener.Addr().String() {
		t.Fatalf("upstream request: %v", seen)
	}
	for name, c := range map[string]struct{ name, host, path, token string }{
		"other workspace API": {"api.github.com", "api.github.com", "/repos/o/r/pulls", env["GH_TOKEN"]},
		"host mismatch":       {"git.example.test", "api.github.com", "/api/v3/user", env["GH_TOKEN"]},
		"not an API path":     {"git.example.test", "git.example.test", "/team/fixture.git/info/refs", env["GH_TOKEN"]},
		"Multica credential":  {"git.example.test", "git.example.test", "/api/v3/user", "mat_relay_x"},
	} {
		if code, err := call(c.name, c.host, c.path, c.token); err != nil || code < 400 {
			t.Errorf("%s: %d %v", name, code, err)
		}
	}
	if _, err := call("example.com", "example.com", "/", env["GH_TOKEN"]); err == nil {
		t.Error("unconfigured name got a certificate")
	}
	g.Revoke("attempt")
	if code, _ := call("git.example.test", "git.example.test", "/api/v3/user", env["GH_TOKEN"]); code != http.StatusUnauthorized {
		t.Errorf("revoked grant: %d", code)
	}
	if len(seen) != 1 {
		t.Fatalf("denied calls reached the upstream: %v", seen)
	}
}
