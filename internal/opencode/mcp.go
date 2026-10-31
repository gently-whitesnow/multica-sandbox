package opencode

import (
	"net/http"
	"net/url"
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
		return relay.Lease{}, false
	}
	m.mu.Lock()
	r := m.attempts[lease.Attempt]
	m.mu.Unlock()
	if r == nil {
		lease.Release()
		return relay.Lease{}, false
	}
	origin, path, bearer, ok := r.mcpUpstream(strings.TrimPrefix(req.URL.Path, "/"))
	httpService := strings.HasPrefix(req.URL.Path, "/http/")
	if httpService {
		origin, path, bearer, ok = r.httpUpstream(req.Method, req.URL.Path)
		name := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/http/"), "/", 2)[0]
		lease.Header = r.httpServices[name].Header()
	}
	if !ok {
		lease.Release()
		return relay.Lease{}, false
	}
	lease.Origin, lease.Path, lease.Credential = origin, path, bearer
	// OpenCode adds codemode=false; no caller query may change the approved URL.
	lease.DropQuery = !httpService
	return lease, true
}

// mcpUpstream resolves a selected connection and its registered, unexpired token; an
// anonymous connection gets none, and the relay still drops the caller's credentials.
func (r *running) mcpUpstream(name string) (*url.URL, string, string, bool) {
	connection, ok := r.connections[name]
	r.mu.Lock()
	token := r.tokens[name]
	r.mu.Unlock()
	u, err := url.Parse(connection.URL)
	if !ok || err != nil || !r.anonymous[name] && time.Until(token.ExpiresAt) <= time.Second {
		return nil, "", "", false
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, path, token.Bearer(), true
}

// validMCPURL admits an exact HTTP(S) endpoint the relay can address unchanged.
func validMCPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawPath == "" && !strings.ContainsAny(raw, "\r\n\x00")
}
