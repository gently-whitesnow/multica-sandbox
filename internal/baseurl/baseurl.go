// Package baseurl validates base paths of trusted service URLs, such as a
// self-hosted service published under a shared ingress prefix.
package baseurl

import (
	"net/url"
	"path"
	"strings"
)

// Path admits a base URL path: empty or absolute and clean apart from one
// trailing slash, without escaped characters, so joined paths cannot leave or alias it.
func Path(u *url.URL) bool {
	if u.RawPath != "" {
		return false
	}
	if u.Path == "" || u.Path == "/" {
		return true
	}
	p := strings.TrimSuffix(u.Path, "/")
	return p != "/" && strings.HasPrefix(p, "/") && path.Clean(p) == p && !strings.ContainsAny(p, "\\%;")
}

// Join returns the origin and base path of u without a trailing slash.
func Join(u *url.URL) string {
	return u.Scheme + "://" + u.Host + strings.TrimSuffix(u.Path, "/")
}
