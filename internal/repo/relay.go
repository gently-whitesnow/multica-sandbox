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
	// repos are granted repository keys; rewrites are the remote prefixes the attempt
	// environment maps to host aliases.
	repos, rewrites map[string]bool
}

func NewRelay(relayURL string, hosts *Hosts) *Relay {
	return &Relay{URL: strings.TrimRight(relayURL, "/"), hosts: hosts, grants: relay.NewGrants(GitPrefix), access: map[string]access{}}
}

// Issue grants the attempt its claim repositories and returns the Git environment that
// proxies every host the workspace binds through the relay, so workspace repositories
// granted later by Allow need no new environment. origin keeps the real URL. With the
// forge relay, the same credential is the gh token for the workspace's forge APIs.
func (g *Relay) Issue(attempt, workspace string, urls []string) (map[string]string, error) {
	a := access{workspace: workspace, repos: map[string]bool{}, rewrites: map[string]bool{}}
	prefixes := map[string][]string{}
	for _, raw := range urls {
		host, key, prefix, ok := repoKey(raw)
		if !ok || !g.hosts.Has(workspace, host) {
			continue
		}
		prefixes[host] = append(prefixes[host], prefix)
		a.repos[key] = true
	}
	opaque, err := g.grants.Bind(attempt)
	if err != nil {
		return nil, err
	}
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	config := [][2]string{}
	for _, host := range g.hosts.gitHosts(workspace) {
		alias := "http://" + host + "/"
		config = append(config, [2]string{"http." + alias + ".extraHeader", "Authorization: Bearer " + opaque},
			[2]string{"http." + alias + ".proxy", g.URL})
		for _, prefix := range unique(append(rewrites(host), prefixes[host]...)) {
			if prefix != alias {
				config = append(config, [2]string{"url." + alias + ".insteadOf", prefix})
			}
			a.rewrites[prefix] = true
		}
	}
	g.mu.Lock()
	g.access[attempt] = a
	g.mu.Unlock()
	if g.ca != nil {
		forgeEnv(env, opaque, g.hosts.apiHosts(workspace, GitHub), g.hosts.apiHosts(workspace, GitLab))
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(len(config))
	for i, entry := range config {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)], env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = entry[0], entry[1]
	}
	return env, nil
}

// rewrites are the default remote prefixes of a host alias.
func rewrites(host string) []string {
	if strings.Contains(host, ":") {
		return []string{"https://" + host + "/", "http://" + host + "/"}
	}
	return []string{"https://" + host + "/", "http://" + host + "/", "git@" + host + ":", "ssh://git@" + host + "/"}
}

// Allow grants a live attempt a workspace repository its claim did not list, as upstream
// admits workspace repositories on checkout. It returns the host alias and, when the
// attempt environment does not rewrite the URL's remote prefix, that prefix.
func (g *Relay) Allow(attempt, raw string) (string, string, bool) {
	host, key, prefix, valid := repoKey(raw)
	g.mu.Lock()
	defer g.mu.Unlock()
	a, granted := g.access[attempt]
	if !valid || !granted || !g.hosts.Has(a.workspace, host) {
		return "", "", false
	}
	a.repos[key] = true
	if a.rewrites[prefix] {
		prefix = ""
	}
	return "http://" + host + "/", prefix, true
}

// forgeEnv points the unchanged gh and glab at the forge relay with the opaque credential,
// without prompts, update checks or telemetry; a single host of a kind is the default.
func forgeEnv(env map[string]string, opaque string, github, gitlab []string) {
	if len(github)+len(gitlab) > 0 {
		env["SSL_CERT_DIR"] = CertDirs
	}
	if len(github) > 0 {
		env["GH_TOKEN"], env["GH_ENTERPRISE_TOKEN"], env["GH_PROMPT_DISABLED"], env["GH_NO_UPDATE_NOTIFIER"] = opaque, opaque, "1", "1"
		if len(github) == 1 {
			env["GH_HOST"] = github[0]
		}
	}
	if len(gitlab) > 0 {
		env["GITLAB_TOKEN"], env["GLAB_NO_PROMPT"], env["GLAB_CHECK_UPDATE"], env["GLAB_SEND_TELEMETRY"] = opaque, "true", "false", "false"
		if len(gitlab) == 1 {
			env["GITLAB_HOST"] = gitlab[0]
		}
	}
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

// repoKey returns the host, the comparison key (lower-case host and path without a
// trailing slash or .git) and the remote prefix rewritten to the host alias of a claim
// URL in the forms upstream accepts: https, http, ssh, git and scp-like [user@]host:path.
// SSH and git ports are dropped: the relay reaches the host over HTTPS.
func repoKey(raw string) (string, string, string, bool) {
	raw = strings.TrimSpace(raw)
	if strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '\\' }) {
		return "", "", "", false
	}
	var host, p, prefix string
	if scheme, rest, found := strings.Cut(raw, "://"); found {
		u, err := url.Parse(raw)
		if err != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.ForceQuery {
			return "", "", "", false
		}
		_, password := u.User.Password()
		switch {
		case (u.Scheme == "https" || u.Scheme == "http") && u.User == nil:
			host = u.Host
		case (u.Scheme == "ssh" || u.Scheme == "git") && !password:
			host = u.Hostname()
		default:
			return "", "", "", false
		}
		authority, _, _ := strings.Cut(rest, "/")
		host, p, prefix = strings.ToLower(host), u.Path, scheme+"://"+authority+"/"
	} else {
		colon := strings.Index(raw, ":")
		at := strings.Index(raw, "@")
		if colon <= 0 || at >= colon || strings.HasPrefix(raw[colon+1:], "/") {
			return "", "", "", false
		}
		host, p, prefix = strings.ToLower(raw[at+1:colon]), "/"+raw[colon+1:], raw[:colon+1]
	}
	p = strings.TrimSuffix(strings.TrimRight(p, "/"), ".git")
	if !hostName.MatchString(host) || strings.Count(p, "/") < 1 || strings.Contains(p, "//") || strings.Contains(p, "/.") {
		return "", "", "", false
	}
	return host, host + p, prefix, true
}

// ValidURL reports a claim repository URL the Git relay can serve.
func ValidURL(raw string) bool {
	_, _, _, ok := repoKey(raw)
	return ok
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
