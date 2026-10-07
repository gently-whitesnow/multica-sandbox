package docker

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Bundle is a controller-selected, digest-pinned image mounted read-only into every
// attempt; its Path directories (default bin) precede the image PATH in order (ADR 0002).
// Docker applies neither nosuid nor nodev to image mounts; no-new-privileges, zero
// capabilities and the device cgroup neutralize both.
type Bundle struct {
	Image, Target string
	Path          []string
}

var bundleTarget = regexp.MustCompile(`^/opt/multica-sandbox(/[a-z0-9][a-z0-9-]{0,31}){1,2}$`)

// BundlePath matches a clean relative path inside a bundle; no segment starts with a dot.
var BundlePath = regexp.MustCompile(`^[A-Za-z0-9_+-][A-Za-z0-9._+-]*(/[A-Za-z0-9_+-][A-Za-z0-9._+-]*)*$`)

// Dirs returns the absolute PATH directories of the bundle.
func (b Bundle) Dirs() []string {
	if len(b.Path) == 0 {
		return []string{b.Target + "/bin"}
	}
	dirs := make([]string, len(b.Path))
	for i, path := range b.Path {
		dirs[i] = b.Target + "/" + path
	}
	return dirs
}

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
		if !imagePattern.MatchString(bundle.Image) || !bundleTarget.MatchString(bundle.Target) || seen[bundle.Target] || len(bundle.Path) > 8 {
			return nil, "", fmt.Errorf("digest-pinned bundle image and unique controller-owned target required")
		}
		for _, path := range bundle.Path {
			if !BundlePath.MatchString(path) {
				return nil, "", fmt.Errorf("bundle %s: PATH entries must stay inside the bundle", bundle.Target)
			}
		}
		seen[bundle.Target] = true
		if _, err := inspectImage(ctx, bundle.Image, engine); err != nil {
			return nil, "", fmt.Errorf("bundle %s: %w", bundle.Target, err)
		}
		args = append(args, "--mount=type=image,source="+bundle.Image+",target="+bundle.Target)
		dirs = append(dirs, bundle.Dirs()...)
	}
	path = strings.Join(append(dirs, path), ":")
	return append(args, "--env=PATH="+path), path, nil
}
