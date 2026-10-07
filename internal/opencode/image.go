package opencode

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// Supported lists OpenCode releases verified by the conformance suite (README).
var Supported = []string{"1.18.34", "1.18.35"}

// ImageProbe reports image facts the adapter relies on. Its output is image-controlled data.
// OpenCode merges the probed paths over the projected configuration (managed and parent-directory config).
var ImageProbe = []string{"/bin/sh", "-c", `for p in /etc/opencode /opencode.json /opencode.jsonc /.opencode; do if [ -e "$p" ]; then echo "override $p"; fi; done
env | grep -oE '^(OPENCODE|MULTICA)_[A-Za-z0-9_]+=' | sed 's/^/env /;s/=$//'
echo "version $(opencode --version 2>/dev/null | head -n 1)"
echo "cli $(command -v multica)"
echo "cli-version $(multica version 2>/dev/null | head -n 1)"`}

// CheckImage fails closed with every incompatibility an operator must fix in the image.
// With cli, multica must resolve to the controller artifact built from the pinned revision.
func CheckImage(output []byte, cli bool) error {
	var problems []string
	version, path, build := "", "", ""
	for _, line := range strings.Split(string(output), "\n") {
		kind, value, _ := strings.Cut(line, " ")
		switch kind {
		case "override":
			problems = append(problems, fmt.Sprintf("%.64q overrides the projected OpenCode configuration", value))
		case "env":
			problems = append(problems, fmt.Sprintf("image ENV sets reserved %.64q", value))
		case "version":
			version = value
		case "cli":
			path = value
		case "cli-version":
			build = value
		}
	}
	if cli && path != multica.CLIDir+"/bin/multica" {
		problems = append(problems, fmt.Sprintf("multica resolves to %.64q, not the controller artifact", path))
	}
	if cli && !strings.Contains(build, "(commit: "+multica.UpstreamRevision+",") {
		problems = append(problems, fmt.Sprintf("multica artifact %.96q is not built from Multica %s", build, multica.UpstreamRevision))
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
