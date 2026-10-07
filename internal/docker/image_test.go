//go:build containers

package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

// userImage commits a user-owned image derived from the preloaded test image without network access.
func userImage(t *testing.T, setup string, changes ...string) string {
	t.Helper()
	ctx := context.Background()
	data, err := command(ctx, "create", "--network=none", "--entrypoint", "/bin/sh", testImage, "-c", `set -eu
printf '#!/bin/sh\necho 1.18.35\n' > /usr/local/bin/opencode
chmod 755 /usr/local/bin/opencode
`+setup)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(data))
	defer command(context.Background(), "rm", "-fv", id)
	if _, err = command(ctx, "start", "--attach", id); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("multica-sandbox-user-image:%d", time.Now().UnixNano())
	args := []string{"commit"}
	for _, change := range changes {
		args = append(args, "--change", change)
	}
	if _, err = command(ctx, append(args, id, tag)...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command(context.Background(), "rmi", "-f", tag) })
	digest, err := command(ctx, "image", "inspect", "--format", "{{index .RepoDigests 0}}", tag)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(digest))
}

func TestUserImageCannotOverridePolicy(t *testing.T) {
	image := userImage(t, `cp /bin/busybox /usr/local/bin/busybox-suid
chmod 4755 /usr/local/bin/busybox-suid`, "USER root", "HEALTHCHECK CMD true", "ENV BUN_RUNTIME_TRANSPILER_CACHE_PATH=0", "ENTRYPOINT [\"/bin/false\"]")
	b := testBackend(t, "true")
	b.Image = image
	w := &Projected{Backend: *b}
	ctx := context.Background()
	report, err := w.Output(ctx, opencode.ImageProbe)
	if err != nil {
		t.Fatal(err)
	}
	if err = opencode.CheckImage(report, false); err != nil {
		t.Fatal(err)
	}
	identity, err := w.Output(ctx, []string{"/bin/sh", "-c", `id -u; busybox-suid id -u; grep '^NoNewPrivs' /proc/self/status`})
	if err != nil {
		t.Fatal(err)
	}
	if string(identity) != "65532\n65532\nNoNewPrivs:\t1\n" {
		t.Fatalf("image changed execution identity: %q", identity)
	}
}

func TestIncompatibleUserImages(t *testing.T) {
	for name, test := range map[string]struct {
		setup   string
		changes []string
		want    []string
	}{
		"managed config": {setup: "mkdir /etc/opencode; echo {} > /etc/opencode/opencode.json", want: []string{`"/etc/opencode" overrides`}},
		"root config":    {setup: "echo {} > /opencode.json", want: []string{`"/opencode.json" overrides`}},
		"reserved env":   {changes: []string{`ENV OPENCODE_CONFIG_CONTENT={"mcp":{}}`, "ENV MULTICA_PROFILE=other"}, want: []string{`reserved "OPENCODE_CONFIG_CONTENT"`, `reserved "MULTICA_PROFILE"`}},
		"version":        {setup: `printf '#!/bin/sh\necho 1.17.0\necho version 1.18.35\n' > /usr/local/bin/opencode`, want: []string{`OpenCode "1.17.0" is not verified`}},
		"volume":         {changes: []string{"VOLUME /data"}, want: []string{"image-declared volumes are unsupported"}},
	} {
		t.Run(name, func(t *testing.T) {
			b := testBackend(t, "true")
			b.Image = userImage(t, test.setup, test.changes...)
			w := &Projected{Backend: *b}
			report, err := w.Output(context.Background(), opencode.ImageProbe)
			if err == nil {
				err = opencode.CheckImage(report, false)
			}
			for _, want := range test.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("got %v, want %q", err, want)
				}
			}
		})
	}
}

// cliBundle builds a scratch CLI artifact offline whose multica runs script.
func cliBundle(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "multica"), []byte("#!/bin/sh\n"+script+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY bin /bin\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("multica-sandbox-cli-test:%d", time.Now().UnixNano())
	if _, err := command(context.Background(), "build", "-q", "-t", tag, dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command(context.Background(), "rmi", "-f", tag) })
	digest, err := command(context.Background(), "image", "inspect", "--format", "{{index .RepoDigests 0}}", tag)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(digest))
}

var pinnedCLI = `echo "multica dev (commit: ` + multica.UpstreamRevision + `, built: unknown)"`

func TestControllerCLICannotBeShadowedOrChanged(t *testing.T) {
	ctx := context.Background()
	b := testBackend(t, "true")
	b.Image = userImage(t, `printf '#!/bin/sh\necho image\n' > /usr/local/bin/multica
mkdir -p /opt/multica-sandbox/multica/bin
cp /usr/local/bin/multica /opt/multica-sandbox/multica/bin/multica
chmod 755 /usr/local/bin/multica /opt/multica-sandbox/multica/bin/multica`, "ENV PATH=/usr/local/bin:/usr/bin:/bin")
	w := &Projected{Backend: *b, Bundles: []Bundle{{Image: cliBundle(t, pinnedCLI), Target: multica.CLIDir}}}
	report, err := w.Output(ctx, opencode.ImageProbe)
	if err == nil {
		err = opencode.CheckImage(report, true)
	}
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.Output(ctx, []string{"/bin/sh", "-c", `d=/opt/multica-sandbox/multica
multica | cut -d' ' -f1-2
! touch "$d/bin/x" 2>/dev/null && ! cp /bin/sh "$d/bin/multica" 2>/dev/null && ! mount -o remount,rw "$d" 2>/dev/null && echo immutable
grep " $d " /proc/self/mountinfo | cut -d' ' -f6 | cut -d, -f1
echo "$PATH"`})
	if want := "multica dev\nimmutable\nro\n/opt/multica-sandbox/multica/bin:/usr/local/bin:/usr/bin:/bin\n"; err != nil || string(out) != want {
		t.Fatalf("got %q %v, want %q", out, err, want)
	}
}

func TestRejectInvalidCLIArtifact(t *testing.T) {
	b := testBackend(t, "true")
	b.Image = userImage(t, "")
	for name, test := range map[string]struct {
		bundle Bundle
		want   string
	}{
		"revision": {Bundle{Image: cliBundle(t, `echo "multica dev (commit: unknown, built: unknown)"`), Target: multica.CLIDir}, "is not built from Multica"},
		"absent":   {Bundle{Image: "example.invalid/cli@sha256:" + strings.Repeat("0", 64), Target: multica.CLIDir}, "preload the pinned image"},
		"unpinned": {Bundle{Image: "alpine:latest", Target: multica.CLIDir}, "digest-pinned bundle image"},
		"target":   {Bundle{Image: testImage, Target: "/usr/local"}, "controller-owned target"},
	} {
		t.Run(name, func(t *testing.T) {
			w := &Projected{Backend: *b, Bundles: []Bundle{test.bundle}}
			report, err := w.Output(context.Background(), opencode.ImageProbe)
			if err == nil {
				err = opencode.CheckImage(report, true)
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}

// TestToolBundleCannotElevate mounts a setuid-root binary that reports its effective uid.
func TestToolBundleCannotElevate(t *testing.T) {
	ctx := context.Background()
	engine, err := command(ctx, "info", "--format", "{{.Architecture}}")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nimport (\"fmt\"; \"os\")\n\nfunc main() { fmt.Println(os.Getuid(), os.Geteuid()) }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "euid"), filepath.Join(dir, "main.go"))
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+engineArchitectures[strings.TrimSpace(string(engine))])
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY --chmod=4755 euid /bin/euid\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("multica-sandbox-tool-test:%d", time.Now().UnixNano())
	if _, err = command(ctx, "build", "-q", "-t", tag, dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command(context.Background(), "rmi", "-f", tag) })
	ref, err := command(ctx, "image", "inspect", "--format", "{{index .RepoDigests 0}}", tag)
	if err != nil {
		t.Fatal(err)
	}
	b := testBackend(t, "true")
	bundle := Bundle{Image: strings.TrimSpace(string(ref)), Target: "/opt/multica-sandbox/tools/euid"}
	w := &Projected{Backend: *b, Bundles: []Bundle{bundle}}
	out, err := w.Output(ctx, []string{"/bin/sh", "-c", `d=/opt/multica-sandbox/tools/euid/bin; stat -c %a "$d/euid"; euid; ! touch "$d/x" 2>/dev/null && echo read-only`})
	if want := "4755\n65532 65532\nread-only\n"; err != nil || string(out) != want {
		t.Fatalf("got %q %v, want %q", out, err, want)
	}
	bundle.Path = []string{"../../../workspace"}
	w.Bundles = []Bundle{bundle}
	if _, err = w.Output(ctx, []string{"/bin/true"}); err == nil || !strings.Contains(err.Error(), "inside the bundle") {
		t.Fatalf("escaping PATH entry accepted: %v", err)
	}
}
