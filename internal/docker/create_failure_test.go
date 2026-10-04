package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
info) printf '%s' '{"OSType":"linux","CgroupVersion":"2","MemoryLimit":true,"SwapLimit":true,"PidsLimit":true,"CpuCfsPeriod":true,"CpuCfsQuota":true,"SecurityOptions":["name=seccomp,profile=builtin"]}' ;;
image) printf '{}' ;;
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
