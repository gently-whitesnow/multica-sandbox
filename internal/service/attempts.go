package service

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
)

// DinDConfig configures the nested engine of sysbox-runc attempts (ADR 0019).
type DinDConfig struct {
	// RegistryMirrors are pull-through registries for Docker Hub, reached as peers.
	RegistryMirrors    []string `json:"registry_mirrors,omitempty"`
	InsecureRegistries []string `json:"insecure_registries,omitempty"`
	ReadyTimeout       string   `json:"ready_timeout,omitempty"`
	// TestcontainersRyukDisabled: removing the attempt removes its inner containers.
	TestcontainersRyukDisabled bool `json:"testcontainers_ryuk_disabled,omitempty"`
}

var registryHost = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,252})(:[0-9]{1,5})?$`)

// attemptRuntime validates the runtime and its nested engine configuration.
func attemptRuntime(c *OpenCodeConfig) (string, docker.DinD, error) {
	var dind docker.DinD
	switch c.Runtime {
	case "", "runc":
		if c.DinD != nil {
			return "", dind, fmt.Errorf("dind requires runtime sysbox-runc")
		}
		return "", dind, nil
	case docker.Sysbox:
	default:
		return "", dind, fmt.Errorf("runtime must be runc or sysbox-runc")
	}
	if c.Helper == "" {
		return "", dind, fmt.Errorf("runtime sysbox-runc requires the helper")
	}
	d := c.DinD
	if d == nil {
		d = &DinDConfig{}
	}
	if len(d.RegistryMirrors) > 8 || len(d.InsecureRegistries) > 8 {
		return "", dind, fmt.Errorf("dind allows at most 8 registry mirrors and insecure registries")
	}
	for _, mirror := range d.RegistryMirrors {
		u, err := url.Parse(mirror)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return "", dind, fmt.Errorf("dind registry mirrors must be http(s) URLs without path or credentials")
		}
	}
	for _, registry := range d.InsecureRegistries {
		if _, err := netip.ParsePrefix(registry); err != nil && !registryHost.MatchString(registry) {
			return "", dind, fmt.Errorf("dind insecure registries must be host[:port] or CIDR")
		}
	}
	timeout, err := optionalDuration(d.ReadyTimeout, 30*time.Second)
	if err != nil || timeout < time.Second || timeout > 5*time.Minute {
		return "", dind, fmt.Errorf("dind ready_timeout must be within [1s,5m]")
	}
	return docker.Sysbox, docker.DinD{RegistryMirrors: d.RegistryMirrors, InsecureRegistries: d.InsecureRegistries, ReadyTimeout: timeout, RyukDisabled: d.TestcontainersRyukDisabled}, nil
}
