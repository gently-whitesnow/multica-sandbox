package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
)

func TestUncertainCreateCleanup(t *testing.T) {
	for _, owner := range []string{"90000000-0000-4000-8000-000000000001", "another-owner"} {
		t.Run(owner, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("DOCKER_TEST_OWNER", owner)
			t.Setenv("DOCKER_TEST_EVENTS", filepath.Join(dir, "events"))
			script := `#!/bin/sh
printf '%s\n' "$1" >> "$DOCKER_TEST_EVENTS"
case "$1" in
info) printf '%s' '{"Architecture":"x86_64","OSType":"linux","CgroupVersion":"2","MemoryLimit":true,"SwapLimit":true,"PidsLimit":true,"CpuCfsPeriod":true,"CpuCfsQuota":true,"SecurityOptions":["name=seccomp,profile=builtin"]}' ;;
image) printf '%s' '{"Os":"linux","Architecture":"amd64"}' ;;
create) exit 1 ;;
inspect) printf '%s' "$DOCKER_TEST_OWNER" ;;
rm) exit 0 ;;
*) exit 2 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			b := &Backend{Owner: "90000000-0000-4000-8000-000000000001", Image: "example.invalid/test@sha256:" + strings.Repeat("a", 64), Command: []string{"/bin/true"}}
			run, err := b.Start(context.Background(), "attempt")
			if err == nil || run != nil {
				t.Fatal("uncertain create must fail")
			}
			events, err := os.ReadFile(filepath.Join(dir, "events"))
			if err != nil {
				t.Fatal(err)
			}
			want := "info\nimage\ncreate\ninspect\n"
			if owner == b.Owner {
				want += "rm\n"
			}
			if string(events) != want {
				t.Fatalf("calls=%q, want %q", events, want)
			}
		})
	}
}

func TestImagePlatform(t *testing.T) {
	for image, want := range map[string]string{
		`{"Os":"linux","Architecture":"amd64"}`:                                   "",
		`{"Os":"linux","Architecture":"arm64"}`:                                   "image platform linux/arm64 does not match the Docker engine x86_64",
		`{"Os":"windows","Architecture":"amd64"}`:                                 "does not match",
		`{"Os":"linux","Architecture":"amd64","Config":{"Volumes":{"/data":{}}}}`: "image-declared volumes are unsupported",
	} {
		dir := t.TempDir()
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("DOCKER_TEST_IMAGE", image)
		script := `#!/bin/sh
case "$1" in
info) printf '%s' '{"Architecture":"x86_64","OSType":"linux","CgroupVersion":"2","MemoryLimit":true,"SwapLimit":true,"PidsLimit":true,"CpuCfsPeriod":true,"CpuCfsQuota":true,"SecurityOptions":["name=seccomp,profile=builtin"]}' ;;
image) printf '%s' "$DOCKER_TEST_IMAGE" ;;
*) exit 2 ;;
esac
`
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		b := &Backend{Owner: "90000000-0000-4000-8000-000000000001", Image: "example.invalid/test@sha256:" + strings.Repeat("a", 64), Command: []string{"/bin/true"}}
		err := b.Validate(context.Background())
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Fatalf("%s: got %v, want %q", image, err, want)
		}
	}
}

// A refused network leaves the task rejected and removes whatever the daemon kept.
func TestRefusedNetworkIsRejectedAndRemoved(t *testing.T) {
	for stderr, want := range map[string]string{
		"all predefined address pools have been fully subnetted": "address pools are exhausted",
		"plugin failed": "refused the attempt network",
	} {
		dir := t.TempDir()
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("DOCKER_TEST_EVENTS", filepath.Join(dir, "events"))
		t.Setenv("DOCKER_TEST_STDERR", stderr)
		script := `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_TEST_EVENTS"
case "$2" in
create) printf '%s\n' "$DOCKER_TEST_STDERR" >&2; exit 1 ;;
rm) exit 1 ;;
esac
exit 2
`
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		err := createNetwork(context.Background(), "owner", "multica-sandbox-a-net")
		var rejected *execution.RejectedError
		events, _ := os.ReadFile(filepath.Join(dir, "events"))
		if !errors.As(err, &rejected) || !strings.Contains(err.Error(), want) || !strings.HasSuffix(string(events), "network rm multica-sandbox-a-net\n") {
			t.Fatalf("%v; events %q", err, events)
		}
	}
}
