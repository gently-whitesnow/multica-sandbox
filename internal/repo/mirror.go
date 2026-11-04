package repo

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

// MirrorDir is where attempts see their workspace's read-only reference mirrors (ADR 0035).
const MirrorDir = "/opt/multica-sandbox/mirrors"

// MirrorScript creates or fetches a bare mirror; its arguments are the directory and URL.
//
//go:embed mirror.sh
var MirrorScript string

// Mirror refreshes a workspace repository's reference mirror and returns its name under
// MirrorDir and a release for after the clone, which keeps it from collection; false
// means a plain clone.
type Mirror interface {
	Ensure(ctx context.Context, workspace, url string) (string, func(), bool)
}

// MirrorKey names a repository's mirror after the relay's comparison key, so the remote
// forms of one repository share it.
func MirrorKey(raw string) (string, bool) {
	_, key, _, ok := repoKey(raw)
	if !ok {
		return "", false
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16]), true
}

// Serves reports a repository URL on a host the workspace binds.
func (g *Relay) Serves(workspace, raw string) bool {
	host, _, _, ok := repoKey(raw)
	return ok && g.hosts.Has(workspace, host)
}
