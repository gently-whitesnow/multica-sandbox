package opencode

import (
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

// MCPRelayPrefix marks opaque MCP relay credentials.
const MCPRelayPrefix = "mcp_relay_"

// MCPHeaderTimeout bounds the wait for an MCP response; a streamable HTTP server may
// answer a long tool call only when it completes.
const MCPHeaderTimeout = 15 * time.Minute

// MCPReservedHeaders keeps upstream OAuth challenges from the agent: authorization is
// the relay's, so OpenCode never starts its own OAuth flow.
var MCPReservedHeaders = []string{"www-authenticate"}

var mcpAlias = regexp.MustCompile(`^/[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

// MCPRelayPath admits one connection alias per request path: /<name>.
func MCPRelayPath(path string) bool {
	return mcpAlias.MatchString(path) || strings.HasPrefix(path, "/http/")
}

// MCPRelay forwards an attempt's MCP requests to the exact approved URL of the named
// connection with its current access token (ADR 0023); tokens never enter the attempt.
type MCPRelay struct {
	grants   *relay.Grants
	mu       sync.Mutex
	attempts map[string]*running
}

func NewMCPRelay() *MCPRelay {
	return &MCPRelay{grants: relay.NewGrants(MCPRelayPrefix), attempts: map[string]*running{}}
}

func (m *MCPRelay) issue(r *running) (string, error) {
	opaque, err := m.grants.Bind(r.task.AttemptKey())
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.attempts[r.task.AttemptKey()] = r
	m.mu.Unlock()
	return opaque, nil
}

// revoke denies new requests and cancels open streams.
func (m *MCPRelay) revoke(attempt string) {
	m.grants.Revoke(attempt)
	m.mu.Lock()
	delete(m.attempts, attempt)
	m.mu.Unlock()
}

func (m *MCPRelay) Authorize(req *http.Request) (relay.Lease, bool) {
	lease, ok := m.grants.Authorize(req)
	if !ok {
		return lease, false
	}
	m.mu.Lock()
	r := m.attempts[lease.Attempt]
	m.mu.Unlock()
	if r == nil {
		lease.Release()
		return relay.Lease{}, false
	}
	httpService := strings.HasPrefix(req.URL.Path, "/http/")
	if httpService {
		origin, path, bearer, allowed := r.httpUpstream(req.Method, req.URL.Path)
		if !allowed {
			lease.Release()
			return relay.Lease{}, false
		}
		lease.Origin, lease.Path, lease.Credential = origin, path, bearer
		name := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/http/"), "/", 2)[0]
		lease.Header = r.httpServices[name].Header()
		lease.WithholdErrors = r.httpServices[name].PathCredential != nil
	} else {
		target, allowed := r.mcpUpstream(strings.TrimPrefix(req.URL.Path, "/"))
		if !allowed {
			lease.Release()
			return relay.Lease{}, false
		}
		lease.Origin, lease.Path, lease.Credential = target.origin, target.path, target.bearer
		lease.Header, lease.ProxyURL, lease.WithholdErrors = target.header, target.proxy, target.withhold
	}
	// OpenCode adds codemode=false; no caller query may change the approved URL.
	lease.DropQuery = !httpService || lease.WithholdErrors
	return lease, true
}

type mcpTarget struct {
	origin, proxy *url.URL
	path, bearer  string
	header        http.Header
	withhold      bool
}

// mcpUpstream binds the exact selected endpoint, credentials and deployment egress.
// Only explicit IAM connections receive issuer tokens; deployment owns proxy routing.
func (r *running) mcpUpstream(name string) (mcpTarget, bool) {
	connection, ok := r.connections[name]
	r.mu.Lock()
	token := r.tokens[name]
	r.mu.Unlock()
	u, err := url.Parse(connection.URL)
	if !ok || err != nil || !r.anonymous[name] && time.Until(token.ExpiresAt) <= time.Second {
		return mcpTarget{}, false
	}
	target := mcpTarget{origin: &url.URL{Scheme: u.Scheme, Host: u.Host, RawQuery: u.RawQuery, ForceQuery: u.ForceQuery}, path: u.Path, bearer: token.Bearer()}
	if target.path == "" {
		target.path = "/"
	}
	if r.public[name] {
		if r.adapter == nil || r.adapter.MCPPublicProxy == nil {
			return mcpTarget{}, false
		}
		target.proxy, target.withhold = r.adapter.MCPPublicProxy, true
	}
	target.withhold = true
	if len(connection.headers) != 0 {
		target.header = http.Header{}
		for name, value := range connection.headers {
			target.header.Set(name, value)
		}
		if !connection.identity {
			target.bearer = ""
		}
	}
	return target, true
}

// validMCPURL admits an exact HTTP(S) endpoint the relay can address unchanged.
func validMCPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\r\n\x00") || strings.Contains(u.Path, "\\") {
		return false
	}
	trimmed := strings.TrimSuffix(u.Path, "/")
	return trimmed == "" || path.Clean(trimmed) == trimmed
}
