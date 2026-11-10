//go:build upstream

package integration

import (
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/service"
)

// Exercise a real operator transport change against the same retained workdir
// and controller state; the following ACP turn must retire the interrupted HTTP DB.
func steeringSwitchToACP(t *testing.T, f serviceFixture, cid, config string, c service.Config) {
	t.Helper()
	f.t = t
	started := dockerTest(t, "inspect", "--format", "{{.State.StartedAt}}", cid)
	ready := strings.Count(dockerTest(t, "logs", cid), readyEvent)
	f.compose("stop", "controller")
	writeJSON(t, config, c)
	f.compose("up", "-d", "--no-build", "controller")
	eventually(t, "ACP controller ready after disabling steering", func() bool {
		return dockerTest(t, "inspect", "--format", "{{.State.StartedAt}}", cid) != started &&
			strings.Count(dockerTest(t, "logs", cid), readyEvent) > ready
	})
}
