//go:build containers

package docker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
)

// userImage commits a user-owned image derived from the preloaded test image without network access.
func userImage(t *testing.T, setup string, changes ...string) string {
	t.Helper()
	ctx := context.Background()
	data, err := command(ctx, "create", "--network=none", "--entrypoint", "/bin/sh", testImage, "-c", `set -eu
printf '#!/bin/sh\necho 1.18.35\n' > /usr/local/bin/opencode
printf '#!/bin/sh\n' > /usr/local/bin/multica
chmod 755 /usr/local/bin/opencode /usr/local/bin/multica
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
	ctx := context.Background()
	report, err := b.Output(ctx, opencode.ImageProbe)
	if err != nil {
		t.Fatal(err)
	}
	if err = opencode.CheckImage(report, true); err != nil {
		t.Fatal(err)
	}
	identity, err := b.Output(ctx, []string{"/bin/sh", "-c", `id -u; busybox-suid id -u; grep '^NoNewPrivs' /proc/self/status`})
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
		"cli":            {setup: "rm /usr/local/bin/multica", want: []string{"multica CLI is not on PATH"}},
		"volume":         {changes: []string{"VOLUME /data"}, want: []string{"image-declared volumes are unsupported"}},
	} {
		t.Run(name, func(t *testing.T) {
			b := testBackend(t, "true")
			b.Image = userImage(t, test.setup, test.changes...)
			report, err := b.Output(context.Background(), opencode.ImageProbe)
			if err == nil {
				err = opencode.CheckImage(report, true)
			}
			for _, want := range test.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("got %v, want %q", err, want)
				}
			}
		})
	}
}
