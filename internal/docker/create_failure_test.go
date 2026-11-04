package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
			var rejected *execution.RejectedError
			// Only a confirmed removal makes the failure a transient rejection.
			if run != nil || errors.As(err, &rejected) != (owner == b.Owner) || (rejected != nil && !rejected.Transient()) {
				t.Fatalf("uncertain create: %v", err)
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

// A create the engine refused (no space left) leaves no container: the task alone is
// rejected and retried. A killed create may still land, so it stays uncertain.
func TestRefusedCreateWithoutContainer(t *testing.T) {
	for create, transient := range map[string]bool{
		`echo 'no space left on device' >&2; exit 125`: true,
		`kill -9 $$`: false,
	} {
		dir := t.TempDir()
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		script := `#!/bin/sh
case "$1" in
info) printf '%s' '{"Architecture":"x86_64","OSType":"linux","CgroupVersion":"2","MemoryLimit":true,"SwapLimit":true,"PidsLimit":true,"CpuCfsPeriod":true,"CpuCfsQuota":true,"SecurityOptions":["name=seccomp,profile=builtin"]}' ;;
image) printf '%s' '{"Os":"linux","Architecture":"amd64"}' ;;
create) ` + create + ` ;;
inspect) echo 'Error: No such object: attempt' >&2; exit 1 ;;
*) exit 2 ;;
esac
`
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		b := &Backend{Owner: "90000000-0000-4000-8000-000000000001", Image: "example.invalid/test@sha256:" + strings.Repeat("a", 64), Command: []string{"/bin/true"}}
		_, err := b.Start(context.Background(), "attempt")
		var rejected *execution.RejectedError
		if got := errors.As(err, &rejected) && rejected.Transient(); got != transient {
			t.Fatalf("create %q: transient=%v, want %v: %v", create, got, transient, err)
		}
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
		if !errors.As(err, &rejected) || !rejected.Transient() || !strings.Contains(err.Error(), want) || !strings.HasSuffix(string(events), "network rm multica-sandbox-a-net\n") {
			t.Fatalf("%v; events %q", err, events)
		}
	}
}

// A daemon failure before the attempt network exists rejects only the task.
func TestDaemonFailureBeforeResourcesIsRejected(t *testing.T) {
	for _, failing := range []string{"info", "image", "network inspect"} {
		dir := t.TempDir()
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("DOCKER_TEST_EVENTS", filepath.Join(dir, "events"))
		t.Setenv("DOCKER_TEST_FAIL", failing)
		script := `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_TEST_EVENTS"
case "$1 $2" in "$DOCKER_TEST_FAIL"*) exit 1 ;; esac
case "$1" in
info) printf '%s' '{"Architecture":"x86_64","OSType":"linux","CgroupVersion":"2","MemoryLimit":true,"SwapLimit":true,"PidsLimit":true,"CpuCfsPeriod":true,"CpuCfsQuota":true,"SecurityOptions":["name=seccomp,profile=builtin"]}' ;;
image) printf '%s' '{"Os":"linux","Architecture":"amd64"}' ;;
*) exit 2 ;;
esac
`
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		b := &Projected{Backend: Backend{Owner: "90000000-0000-4000-8000-000000000001", Image: "example.invalid/test@sha256:" + strings.Repeat("a", 64), Command: []string{"/bin/true"}},
			Network: "template", Peers: []string{"peer"}}
		_, err := b.Start(context.Background(), "attempt", execution.Workdir{})
		var rejected *execution.RejectedError
		events, _ := os.ReadFile(filepath.Join(dir, "events"))
		if !errors.As(err, &rejected) || strings.Contains(string(events), "create") {
			t.Fatalf("%s: %v; events %q", failing, err, events)
		}
	}
}

// An unanswered network create is rejected only once removal confirms it is gone.
func TestUnansweredNetworkCreate(t *testing.T) {
	defer func(timeout time.Duration) { commandTimeout = timeout }(commandTimeout)
	commandTimeout = 200 * time.Millisecond
	for removed, transient := range map[string]bool{"exit 0": true, "exit 1": false} {
		fakeDocker(t, `case "$2" in
create) exec sleep 1 ;;
rm) `+removed+` ;;
esac
exit 2
`)
		err := createNetwork(context.Background(), "owner", "multica-sandbox-a-net")
		var rejected *execution.RejectedError
		if err == nil || (errors.As(err, &rejected) && rejected.Transient()) != transient {
			t.Fatalf("removal %s: %v", removed, err)
		}
	}
}

// Failures after the attempt network exists reject only the task, as transient, when
// cleanup removed everything; a failed removal stays a backend error.
func TestFailureAfterResourcesIsTransient(t *testing.T) {
	const owner = "90000000-0000-4000-8000-000000000001"
	for _, tc := range []struct{ failing, cleanup string }{
		{"network connect", ""},
		{"create", ""},
		{"create", "network rm"},
		{"inspect multica-sandbox", ""},
		{"inspect multica-sandbox", "rm -fv"},
	} {
		dir := t.TempDir()
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("DOCKER_TEST_EVENTS", filepath.Join(dir, "events"))
		t.Setenv("DOCKER_TEST_FAIL", tc.failing)
		t.Setenv("DOCKER_TEST_CLEANUP", tc.cleanup)
		t.Setenv("DOCKER_TEST_OWNER", owner)
		script := `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_TEST_EVENTS"
case "$*" in "$DOCKER_TEST_FAIL"*) echo "no space left on device" >&2; exit 1 ;; esac
if [ -n "$DOCKER_TEST_CLEANUP" ]; then case "$*" in "$DOCKER_TEST_CLEANUP"*) exit 1 ;; esac; fi
case "$1 $2" in
"network inspect") printf '%s' '[{"Internal":true,"Driver":"bridge","Containers":{"c":{"Name":"peer"}}}]' ;;
"inspect peer") printf '%s' '[{"NetworkSettings":{"Networks":{"template":{"Aliases":["relay"]}}}}]' ;;
"inspect --format") printf '%s' "$DOCKER_TEST_OWNER" ;;
info*) printf '%s' '{"Architecture":"x86_64","OSType":"linux","CgroupVersion":"2","MemoryLimit":true,"SwapLimit":true,"PidsLimit":true,"CpuCfsPeriod":true,"CpuCfsQuota":true,"SecurityOptions":["name=seccomp,profile=builtin"]}' ;;
image*) printf '%s' '{"Os":"linux","Architecture":"amd64"}' ;;
esac
exit 0
`
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		b := &Projected{Backend: Backend{Owner: owner, Image: "example.invalid/test@sha256:" + strings.Repeat("a", 64), Command: []string{"/bin/true"}},
			Network: "template", Peers: []string{"peer"}}
		_, err := b.Start(context.Background(), "attempt", execution.Workdir{})
		var rejected *execution.RejectedError
		events, _ := os.ReadFile(filepath.Join(dir, "events"))
		transient := errors.As(err, &rejected) && rejected.Transient()
		if err == nil || transient != (tc.cleanup == "") || !strings.Contains(string(events), "network rm") {
			t.Fatalf("%s, cleanup failing %q: %v; events %q", tc.failing, tc.cleanup, err, events)
		}
	}
}
