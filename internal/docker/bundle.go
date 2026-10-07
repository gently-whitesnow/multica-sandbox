package docker

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Bundle is a controller-selected, digest-pinned image mounted read-only into every
// attempt with its bin directory first on PATH (ADR 0002). Docker applies neither
// nosuid nor nodev to image mounts; no-new-privileges, zero capabilities and the
// device cgroup neutralize both.
type Bundle struct{ Image, Target string }

var bundleTarget = regexp.MustCompile(`^/opt/multica-sandbox/[a-z0-9-]{1,32}$`)

// dockerPath is the engine default when an image sets no PATH.
const dockerPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// prepare validates the backend and bundles and returns the create options mounting
// the bundles and the resulting PATH: bundle bin directories, then the image PATH.
func (b *Projected) prepare(ctx context.Context) ([]string, string, error) {
	engine, image, err := b.validate(ctx)
	if err != nil || len(b.Bundles) == 0 {
		return nil, "", err
	}
	path := dockerPath
	for _, env := range image.Config.Env {
		if value, ok := strings.CutPrefix(env, "PATH="); ok {
			path = value
		}
	}
	var args, dirs []string
	seen := map[string]bool{}
	for _, bundle := range b.Bundles {
		if !imagePattern.MatchString(bundle.Image) || !bundleTarget.MatchString(bundle.Target) || seen[bundle.Target] {
			return nil, "", fmt.Errorf("digest-pinned bundle image and unique controller-owned target required")
		}
		seen[bundle.Target] = true
		if _, err := inspectImage(ctx, bundle.Image, engine); err != nil {
			return nil, "", fmt.Errorf("bundle %s: %w", bundle.Target, err)
		}
		args = append(args, "--mount=type=image,source="+bundle.Image+",target="+bundle.Target)
		dirs = append(dirs, bundle.Target+"/bin")
	}
	path = strings.Join(append(dirs, path), ":")
	return append(args, "--env=PATH="+path), path, nil
}
