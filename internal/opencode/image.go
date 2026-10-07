package opencode

import (
	"fmt"
	"slices"
	"strings"
)

// Supported lists OpenCode releases verified by the conformance suite (README).
var Supported = []string{"1.18.34", "1.18.35"}

// ImageProbe reports image facts the adapter relies on. Its output is image-controlled data.
// OpenCode merges the probed paths over the projected configuration (managed and parent-directory config).
var ImageProbe = []string{"/bin/sh", "-c", `for p in /etc/opencode /opencode.json /opencode.jsonc /.opencode; do if [ -e "$p" ]; then echo "override $p"; fi; done
env | grep -oE '^(OPENCODE|MULTICA)_[A-Za-z0-9_]+=' | sed 's/^/env /;s/=$//'
command -v multica >/dev/null 2>&1 || echo "missing multica"
echo "version $(opencode --version 2>/dev/null | head -n 1)"`}

// CheckImage fails closed with every incompatibility an operator must fix in the image.
func CheckImage(output []byte, cli bool) error {
	var problems []string
	version := ""
	for _, line := range strings.Split(string(output), "\n") {
		kind, value, _ := strings.Cut(line, " ")
		switch kind {
		case "override":
			problems = append(problems, fmt.Sprintf("%.64q overrides the projected OpenCode configuration", value))
		case "env":
			problems = append(problems, fmt.Sprintf("image ENV sets reserved %.64q", value))
		case "missing":
			if cli {
				problems = append(problems, "multica CLI is not on PATH")
			}
		case "version":
			version = value
		}
	}
	if !slices.Contains(Supported, version) {
		problems = append(problems, fmt.Sprintf("OpenCode %.32q is not verified (supported: %s)", version, strings.Join(Supported, ", ")))
	}
	if len(problems) > 8 {
		problems = append(problems[:8], "...")
	}
	if len(problems) > 0 {
		return fmt.Errorf("incompatible agent image: %s", strings.Join(problems, "; "))
	}
	return nil
}
