package repo

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

const (
	// GitPrefix marks per-attempt Git relay credentials.
	GitPrefix = "msg_"
	// PushLimit bounds a pushed pack, as large forges do; PushTimeout bounds the
	// upstream's pack processing before it answers.
	PushLimit   = 2 << 30
	PushTimeout = 5 * time.Minute
)

// Relay authorizes Git smart HTTP for the claim's repositories of each attempt. It is
// the HTTP proxy of per-host http://<host>/ remote aliases, so remotes keep a forge
// host gh resolves (ADR 0016). Fetch and push follow the native runtime: the
// deployment credential's scope and forge branch protection bound refs.
type Relay struct {
	URL    string
	hosts  *Hosts
	grants *relay.Grants
	mu     sync.Mutex
	access map[string]access
	// ca, set by NewForge, makes attempts trust the forge relay for gh.
	ca []byte
}

type access struct {
	workspace string
	repos     map[string]bool
}

func NewRelay(relayURL string, hosts *Hosts) *Relay {
	return &Relay{URL: strings.TrimRight(relayURL, "/"), hosts: hosts, grants: relay.NewGrants(GitPrefix), access: map[string]access{}}
}

// Issue grants the attempt its claim repositories and returns the Git environment that
// proxies their hosts through the relay. origin keeps the real URL. With the forge
// relay, the same credential is the gh token for the workspace's forge APIs.
func (g *Relay) Issue(attempt, workspace string, urls []string) (map[string]string, error) {
	a := access{workspace: workspace, repos: map[string]bool{}}
	hosts := []string{}
	for _, raw := range urls {
		host, key, ok := repoKey(raw)
		if !ok || !g.hosts.Has(workspace, host) {
			continue
		}
		hosts = append(hosts, host)
		a.repos[key] = true
	}
	opaque, err := g.grants.Bind(attempt)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.access[attempt] = a
	g.mu.Unlock()
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	config := [][2]string{}
	for _, host := range unique(hosts) {
		alias := "http://" + host + "/"
		config = append(config, [2]string{"http." + alias + ".extraHeader", "Authorization: Bearer " + opaque},
			[2]string{"http." + alias + ".proxy", g.URL}, [2]string{"url." + alias + ".insteadOf", "https://" + host + "/"})
		if !strings.Contains(host, ":") {
			config = append(config, [2]string{"url." + alias + ".insteadOf", "git@" + host + ":"})
		}
	}
	if apis := g.hosts.apiHosts(workspace); g.ca != nil && len(apis) > 0 {
		env["GH_TOKEN"], env["GH_ENTERPRISE_TOKEN"], env["GH_PROMPT_DISABLED"], env["GH_NO_UPDATE_NOTIFIER"], env["SSL_CERT_DIR"] = opaque, opaque, "1", "1", CertDir
		if len(apis) == 1 {
			env["GH_HOST"] = apis[0]
		}
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(len(config))
	for i, entry := range config {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)], env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = entry[0], entry[1]
	}
	return env, nil
}

// Revoke ends the attempt's access and cancels its in-flight requests.
func (g *Relay) Revoke(attempt string) {
	g.grants.Revoke(attempt)
	g.mu.Lock()
	delete(g.access, attempt)
	g.mu.Unlock()
}

// CA is the forge relay certificate attempts trust, nil without the forge relay.
func (g *Relay) CA() []byte { return g.ca }

func (g *Relay) workspace(attempt string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.access[attempt]
	return a.workspace, ok
}

func (g *Relay) Authorize(r *http.Request) (relay.Lease, bool) {
	lease, ok := g.grants.Authorize(r)
	if !ok {
		return lease, false
	}
	g.mu.Lock()
	a, granted := g.access[lease.Attempt]
	g.mu.Unlock()
	host := strings.ToLower(r.URL.Host)
	repo, service := smartHTTP(r.URL.Path)
	if service == "info/refs" {
		service = r.URL.Query().Get("service")
		if len(r.URL.Query()) != 1 || r.Method != http.MethodGet {
			service = ""
		}
	} else if r.Method != http.MethodPost || r.URL.RawQuery != "" {
		service = ""
	}
	origin, authorization, resolved := g.hosts.resolve(a.workspace, host)
	if !granted || (service != "git-upload-pack" && service != "git-receive-pack") || !a.repos[host+strings.TrimSuffix(repo, ".git")] || !resolved {
		lease.Release()
		return relay.Lease{}, false
	}
	lease.Origin, lease.Path, lease.Authorization = origin, r.URL.Path, authorization
	return lease, true
}

// GitPath admits only smart HTTP endpoints of repository paths.
func GitPath(p string) bool {
	_, service := smartHTTP(p)
	return service != "" && len(p) <= 1024
}

// smartHTTP splits a repository path from its smart HTTP endpoint.
func smartHTTP(p string) (string, string) {
	for _, service := range []string{"info/refs", "git-upload-pack", "git-receive-pack"} {
		if repo, ok := strings.CutSuffix(p, "/"+service); ok && repo != "" {
			return repo, service
		}
	}
	return "", ""
}

// repoKey returns the host and the comparison key of an HTTPS claim URL: lower-case
// host and path without a trailing slash or .git.
func repoKey(raw string) (string, string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return "", "", false
	}
	host := strings.ToLower(u.Host)
	p := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), ".git")
	if !hostName.MatchString(host) || strings.Count(p, "/") < 1 || strings.Contains(p, "//") || strings.Contains(p, "/.") {
		return "", "", false
	}
	return host, host + p, true
}

func unique(values []string) []string {
	seen, out := map[string]bool{}, []string{}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func basic(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}
