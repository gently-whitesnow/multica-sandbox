package repo

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

// GitPrefix marks per-attempt Git relay credentials.
const GitPrefix = "msg_"

// Relay authorizes Git smart HTTP for the claim's repositories of each attempt. Agents
// address it as <URL>/<host>/<repository path>; until push is decided (#47) only
// upload-pack is served.
type Relay struct {
	URL    string
	hosts  *Hosts
	grants *relay.Grants
	mu     sync.Mutex
	access map[string]access
}

type access struct {
	workspace string
	repos     map[string]bool
}

func NewRelay(relayURL string, hosts *Hosts) *Relay {
	return &Relay{URL: strings.TrimRight(relayURL, "/"), hosts: hosts, grants: relay.NewGrants(GitPrefix), access: map[string]access{}}
}

// Issue grants the attempt its claim repositories and returns the Git environment that
// routes them through the relay. origin keeps the real URL.
func (g *Relay) Issue(attempt, workspace string, urls []string) (map[string]string, error) {
	a := access{workspace: workspace, repos: map[string]bool{}}
	hosts := []string{}
	for _, raw := range urls {
		host, key, ok := repository(raw)
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
	config := [][2]string{{"http." + g.URL + "/.extraHeader", "Authorization: Bearer " + opaque}}
	for _, host := range unique(hosts) {
		base := "url." + g.URL + "/" + host + "/.insteadOf"
		config = append(config, [2]string{base, "https://" + host + "/"})
		if !strings.Contains(host, ":") {
			config = append(config, [2]string{base, "git@" + host + ":"})
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

func (g *Relay) Authorize(r *http.Request) (relay.Lease, bool) {
	lease, ok := g.grants.Authorize(r)
	if !ok {
		return lease, false
	}
	g.mu.Lock()
	a, granted := g.access[lease.Attempt]
	g.mu.Unlock()
	host, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	repo, service := smartHTTP("/" + rest)
	if service == "info/refs" {
		service = r.URL.Query().Get("service")
		if len(r.URL.Query()) != 1 || r.Method != http.MethodGet {
			service = ""
		}
	} else if r.Method != http.MethodPost || r.URL.RawQuery != "" {
		service = ""
	}
	origin, authorization, resolved := g.hosts.resolve(a.workspace, host)
	if !granted || service != "git-upload-pack" || !a.repos[host+strings.TrimSuffix(repo, ".git")] || !resolved {
		lease.Release()
		return relay.Lease{}, false
	}
	lease.Origin, lease.Path, lease.Authorization = origin, "/"+rest, authorization
	return lease, true
}

// GitPath admits only smart HTTP endpoints below a host segment.
func GitPath(p string) bool {
	host, rest, ok := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	repo, service := smartHTTP("/" + rest)
	return ok && hostName.MatchString(host) && service != "" && strings.Count(repo, "/") >= 1 && len(p) <= 1024
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

// repository returns the host and the comparison key of an HTTPS claim URL: lower-case
// host and path without a trailing slash or .git.
func repository(raw string) (string, string, bool) {
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
