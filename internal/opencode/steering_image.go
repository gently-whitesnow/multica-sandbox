package opencode

import (
	"fmt"
	"strings"
)

// CheckSteeringImage gates the additional HTTP/SSE transport independently of ACP admission.
func CheckSteeringImage(report []byte) error {
	if imageVersion(report) != "2.2.5" {
		return fmt.Errorf("live_steering requires verified OpenCode 2.2.5")
	}
	wget, sync := false, false
	for _, line := range strings.Split(string(report), "\n") {
		if path, ok := strings.CutPrefix(line, "wget "); ok && strings.HasPrefix(path, "/") {
			wget = true
		}
		if path, ok := strings.CutPrefix(line, "sync "); ok && strings.HasPrefix(path, "/") {
			sync = true
		}
	}
	if wget && sync {
		return nil
	}
	return fmt.Errorf("live_steering requires wget for native SSE and sync for durable attempt fencing")
}
