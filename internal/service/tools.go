package service

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// Tool is a user-owned, digest-pinned bundle mounted read-only into every agent attempt
// (ADR 0002). Path lists PATH directories inside the bundle (default bin). Check is an
// optional argv run offline in the agent image at startup; its first element is relative
// to the bundle root, so a libc mismatch cannot fall through to an image command.
type Tool struct {
	Name  string   `json:"name"`
	Image string   `json:"image"`
	Path  []string `json:"path,omitempty"`
	Check []string `json:"check,omitempty"`
}

const toolsDir = "/opt/multica-sandbox/tools"

var toolName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// reservedCommands come from the image or the controller: the agent, the CLI and the
// helpers the controller runs in attempts.
var reservedCommands = []string{"opencode", "multica", "sh", "sleep", "mkdir", "cat", "chmod", "mv"}

// bundles returns the controller CLI followed by the declared tools in PATH order.
func bundles(o *OpenCodeConfig, tools []Tool) ([]docker.Bundle, error) {
	var result []docker.Bundle
	if o.MulticaCLI != "" {
		result = append(result, docker.Bundle{Image: o.MulticaCLI, Target: multica.CLIDir})
	}
	if len(tools) > 16 {
		return nil, fmt.Errorf("at most 16 tools")
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		if !toolName.MatchString(tool.Name) || seen[tool.Name] || len(tool.Check) > 16 || (len(tool.Check) > 0 && !docker.BundlePath.MatchString(tool.Check[0])) {
			return nil, fmt.Errorf("tool %.40q: unique name of [a-z0-9-] and a check command relative to the bundle required", tool.Name)
		}
		seen[tool.Name] = true
		result = append(result, docker.Bundle{Image: tool.Image, Target: toolsDir + "/" + tool.Name, Path: tool.Path})
	}
	return result, nil
}

// toolCommands lists executables in every tool PATH directory. Its output is image-controlled data.
const toolCommands = `for d; do if [ -d "$d" ]; then for f in "$d"/*; do if [ -f "$f" ] && [ -x "$f" ]; then echo "command $f"; fi; done; else echo "missing $d"; fi; done`

// checkTools fails closed when a tool PATH entry is missing, a command is reserved or
// provided twice, or a declared check fails. It is a compatibility check, not a boundary.
func checkTools(ctx context.Context, w *docker.Projected, tools []Tool) error {
	if len(tools) == 0 {
		return nil
	}
	owners := map[string]string{}
	args := []string{"/bin/sh", "-c", toolCommands, "tools"}
	for _, bundle := range w.Bundles {
		if name, ok := strings.CutPrefix(bundle.Target, toolsDir+"/"); ok {
			for _, dir := range bundle.Dirs() {
				owners[dir] = name
				args = append(args, dir)
			}
		}
	}
	report, err := w.Output(ctx, args)
	if err != nil {
		return fmt.Errorf("inspect tools: %w", err)
	}
	var problems []string
	provided := map[string]string{}
	for _, line := range strings.Split(string(report), "\n") {
		kind, value, _ := strings.Cut(line, " ")
		switch kind {
		case "missing":
			problems = append(problems, fmt.Sprintf("tool %q: PATH entry %.96q is missing", owners[value], value))
		case "command":
			name, command := owners[path.Dir(value)], path.Base(value)
			switch {
			case name == "":
			case slices.Contains(reservedCommands, command):
				problems = append(problems, fmt.Sprintf("tool %q provides reserved command %.32q", name, command))
			case provided[command] != "" && provided[command] != name:
				problems = append(problems, fmt.Sprintf("tools %q and %q both provide %.32q", provided[command], name, command))
			default:
				provided[command] = name
			}
		}
	}
	for _, tool := range tools {
		if len(tool.Check) > 0 && len(problems) == 0 {
			if _, err := w.Output(ctx, append([]string{toolsDir + "/" + tool.Name + "/" + tool.Check[0]}, tool.Check[1:]...)); err != nil {
				problems = append(problems, fmt.Sprintf("tool %q check failed; the bundle may not run on this image (static or self-contained builds required)", tool.Name))
			}
		}
	}
	if len(problems) > 8 {
		problems = append(problems[:8], "...")
	}
	if len(problems) > 0 {
		return fmt.Errorf("incompatible tool bundles: %s", strings.Join(problems, "; "))
	}
	return nil
}
