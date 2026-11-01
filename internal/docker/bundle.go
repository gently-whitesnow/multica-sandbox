package docker

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Bundle is a controller-selected, digest-pinned image mounted read-only into every
// attempt; its Path directories (default bin) precede the image PATH in order (ADR 0002).
// Docker applies neither nosuid nor nodev to image mounts; no-new-privileges, zero
// capabilities and the device cgroup neutralize both.
type Bundle struct {
	Image, Target string
	Path          []string
	// NoPath keeps a controller helper off PATH.
	NoPath bool
}

var bundleTarget = regexp.MustCompile(`^/opt/multica-sandbox(/[a-z0-9][a-z0-9-]{0,31}){1,2}$`)

// BundlePath matches a clean relative path inside a bundle; no segment starts with a dot.
var BundlePath = regexp.MustCompile(`^[A-Za-z0-9_+-][A-Za-z0-9._+-]*(/[A-Za-z0-9_+-][A-Za-z0-9._+-]*)*$`)

// Dirs returns the absolute PATH directories of the bundle.
func (b Bundle) Dirs() []string {
	if b.NoPath {
		return nil
	}
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
	if err := b.configured(); err != nil {
		return nil, "", err
	}
	bundles, refs := b.mounts(), []string{b.Image}
	seen := map[string]bool{}
	for _, bundle := range bundles {
		if (bundle.NoPath && len(bundle.Path) > 0) || !imagePattern.MatchString(bundle.Image) || !bundleTarget.MatchString(bundle.Target) || seen[bundle.Target] || len(bundle.Path) > 8 {
			return nil, "", fmt.Errorf("digest-pinned bundle image and unique controller-owned target required")
		}
		for _, path := range bundle.Path {
			if !BundlePath.MatchString(path) {
				return nil, "", fmt.Errorf("bundle %s: PATH entries must stay inside the bundle", bundle.Target)
			}
		}
		seen[bundle.Target] = true
		refs = append(refs, bundle.Image)
	}
	engine, err := b.engine(ctx)
	if err != nil {
		return nil, "", err
	}
	images, err := inspectImages(ctx, refs...)
	if err == nil {
		err = images[0].check(engine)
	}
	if err != nil || len(bundles) == 0 {
		return nil, "", err
	}
	path := dockerPath
	for _, env := range images[0].Config.Env {
		if value, ok := strings.CutPrefix(env, "PATH="); ok {
			path = value
		}
	}
	args := forgeProxyBypass(images[0].Config.Env, b.ForgeHosts)
	var dirs []string
	for i, bundle := range bundles {
		if err := images[i+1].check(engine); err != nil {
			return nil, "", fmt.Errorf("bundle %s: %w", bundle.Target, err)
		}
		args = append(args, "--mount=type=image,source="+bundle.Image+",target="+bundle.Target)
		dirs = append(dirs, bundle.Dirs()...)
	}
	path = strings.Join(append(dirs, path), ":")
	return append(args, "--env=PATH="+path), path, nil
}

// hostTTL bounds reuse of a validated engine and template network, which change only
// when an operator reconfigures them. Image presence, the policy inspection and the
// runtime checks still run for every attempt.
const hostTTL = 30 * time.Second

// hostFacts holds the last successful host validations of a Projected backend.
type hostFacts struct {
	sync.Mutex
	engine              string
	engineAt, networkAt time.Time
}

func (b *Projected) engine(ctx context.Context) (string, error) {
	b.facts.Lock()
	defer b.facts.Unlock()
	if time.Since(b.facts.engineAt) < hostTTL {
		return b.facts.engine, nil
	}
	engine, err := b.host(ctx)
	if err == nil {
		b.facts.engine, b.facts.engineAt = engine, time.Now()
	}
	return engine, err
}

func (b *Projected) templateNetwork(ctx context.Context) error {
	b.facts.Lock()
	defer b.facts.Unlock()
	if time.Since(b.facts.networkAt) < hostTTL {
		return nil
	}
	err := b.ValidateNetwork(ctx)
	if err == nil {
		b.facts.networkAt = time.Now()
	}
	return err
}
