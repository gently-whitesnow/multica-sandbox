package docker

import (
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

// Forge APIs use loopback TLS, while Git's HTTP aliases still need their relay.
func forgeProxyBypass(imageEnv, hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	values := []string{"localhost", "127.0.0.1", "::1"}
	for _, entry := range imageEnv {
		name, value, _ := strings.Cut(entry, "=")
		if name == "NO_PROXY" || name == "no_proxy" {
			values = append(values, value)
		}
	}
	for _, host := range hosts {
		values = append(values, host+":"+repo.ForgePort)
	}
	value := strings.Join(values, ",")
	return []string{"--env=NO_PROXY=" + value, "--env=no_proxy=" + value}
}
