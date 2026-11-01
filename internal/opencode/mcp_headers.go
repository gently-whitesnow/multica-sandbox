package opencode

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

var mcpEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// IAMAuthorization explicitly selects the current agent's managed identity.
const IAMAuthorization = "Bearer {iam:access_token}"

func containsIAMTemplate(value string) bool { return strings.Contains(strings.ToLower(value), "{iam") }

// selectIAMHeader consumes only the literal trusted configuration marker. Values
// produced by ENV expansion cannot select an identity or expose a template upstream.
func selectIAMHeader(headers map[string]string) (bool, error) {
	managed := false
	for name, value := range headers {
		if !containsIAMTemplate(value) {
			continue
		}
		if name != "Authorization" || value != IAMAuthorization {
			return false, fmt.Errorf("IAM requires Authorization: %s", IAMAuthorization)
		}
		managed = true
		delete(headers, name)
	}
	return managed, nil
}

func mcpHeaders(raw map[string]string) (map[string]string, error) {
	if len(raw) > 32 {
		return nil, ErrDenied
	}
	headers := map[string]string{}
	size := 0
	for name, value := range raw {
		if !validMCPHeaderName(name) || !validMCPHeaderValue(value) {
			return nil, ErrDenied
		}
		canonical := http.CanonicalHeaderKey(name)
		if _, duplicate := headers[canonical]; duplicate || reservedMCPHeader(canonical) {
			return nil, ErrDenied
		}
		size += len(name) + len(value)
		if size > 16384 {
			return nil, ErrDenied
		}
		headers[canonical] = value
	}
	return headers, nil
}

func validMCPHeaderName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for _, character := range name {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", character) {
			continue
		}
		return false
	}
	return true
}

func validMCPHeaderValue(value string) bool {
	if len(value) > 4096 {
		return false
	}
	for _, character := range value {
		if character < 32 && character != '\t' || character == 127 {
			return false
		}
	}
	return true
}

func reservedMCPHeader(name string) bool {
	switch name {
	case "Host", "Connection", "Content-Length", "Transfer-Encoding", "Upgrade", "Te", "Trailer", "Forwarded", "X-Real-Ip":
		return true
	}
	return strings.HasPrefix(name, "Proxy-") || strings.HasPrefix(name, "X-Forwarded-")
}

// resolveRemoteHeaders reads only the claim's agent ENV. Referenced names are
// retained so the adapter can exclude those secret values from attempt ENV.
func resolveRemoteHeaders(connections map[string]Remote, env multica.SecretEnv) (map[string]Remote, error) {
	out := map[string]Remote{}
	for name, connection := range connections {
		headers := map[string]string{}
		names := []string{}
		for header, template := range connection.headers {
			value, used, err := mcpHeaderENV(template, env)
			if err != nil {
				return nil, fmt.Errorf("%w: MCP server %q header %q: %s", ErrDenied, name, header, err)
			}
			if containsIAMTemplate(value) {
				return nil, fmt.Errorf("%w: MCP server %q header %q has an unresolved IAM template", ErrDenied, name, header)
			}
			headers[header] = value
			names = append(names, used...)
		}
		resolved, err := mcpHeaders(headers)
		if err != nil {
			return nil, fmt.Errorf("%w: MCP server %q has invalid resolved headers", ErrDenied, name)
		}
		connection.headers = resolved
		slices.Sort(names)
		connection.headerEnv = slices.Compact(names)
		out[name] = connection
	}
	return out, nil
}

func mcpHeaderENV(template string, env multica.SecretEnv) (string, []string, error) {
	var value strings.Builder
	names := []string{}
	for {
		prefix, rest, found := strings.Cut(template, "{env:")
		value.WriteString(prefix)
		if !found {
			return value.String(), names, nil
		}
		name, suffix, closed := strings.Cut(rest, "}")
		if !closed || !mcpEnvName.MatchString(name) {
			return "", nil, fmt.Errorf("malformed {env:NAME} reference")
		}
		if name == identity.EnvClientID || name == identity.EnvClientSecret {
			return "", nil, fmt.Errorf("IAM credentials cannot be referenced in MCP headers; use %s", IAMAuthorization)
		}
		secret, exists := env.Allowed([]string{name})[name]
		if !exists || secret == "" {
			return "", nil, fmt.Errorf("agent ENV %q is not set", name)
		}
		if len(secret) > 4096 {
			return "", nil, fmt.Errorf("resolved header exceeds size limit")
		}
		value.WriteString(secret)
		names = append(names, name)
		template = suffix
		if value.Len() > 4096 {
			return "", nil, fmt.Errorf("resolved header exceeds size limit")
		}
	}
}
