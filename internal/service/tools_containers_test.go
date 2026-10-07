//go:build containers

package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/docker"
)

const testImage = "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

// toolBundle builds a scratch bundle offline from executable files keyed by bundle path.
func toolBundle(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, "root", name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY root/ /\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("multica-sandbox-tool-test:%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", tag).Run() })
	out, err := exec.Command("docker", "image", "inspect", "--format", "{{index .RepoDigests 0}}", tag).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestCheckToolBundles(t *testing.T) {
	ok := toolBundle(t, map[string]string{"bin/fixture-tool": "#!/bin/sh\necho ok\n"})
	// A missing interpreter fails exec with ENOENT exactly like a glibc binary on musl.
	foreign := toolBundle(t, map[string]string{"bin/tac": "#!/lib/ld-missing.so.1\n"})
	reserved := toolBundle(t, map[string]string{"bin/cat": "#!/bin/sh\n", "bin/multica": "#!/bin/sh\n"})
	for name, test := range map[string]struct {
		tools []Tool
		want  string
	}{
		"compatible": {[]Tool{{Name: "fixture", Image: ok, Check: []string{"bin/fixture-tool"}}}, ""},
		"reserved":   {[]Tool{{Name: "coreutils", Image: reserved}}, `tool "coreutils" provides reserved command "cat"`},
		"duplicate":  {[]Tool{{Name: "one", Image: ok}, {Name: "two", Image: ok}}, `tools "one" and "two" both provide "fixture-tool"`},
		"missing":    {[]Tool{{Name: "fixture", Image: ok, Path: []string{"bin", "sbin"}}}, `PATH entry "/opt/multica-sandbox/tools/fixture/sbin" is missing`},
		"foreign":    {[]Tool{{Name: "text", Image: foreign, Check: []string{"bin/tac"}}}, `tool "text" check failed`},
	} {
		t.Run(name, func(t *testing.T) {
			w := &docker.Projected{Backend: docker.Backend{Image: testImage, Owner: "92000000-0000-4000-8000-000000000043", Command: []string{"/bin/sh"}}}
			var err error
			if w.Bundles, err = bundles(&OpenCodeConfig{}, test.tools); err != nil {
				t.Fatal(err)
			}
			err = checkTools(context.Background(), w, test.tools)
			if (test.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
	// Called by name, the foreign tool silently falls through to the image command, so checks use bundle paths.
	w := &docker.Projected{Backend: docker.Backend{Image: testImage, Owner: "92000000-0000-4000-8000-000000000043", Command: []string{"/bin/sh"}}, Bundles: []docker.Bundle{{Image: foreign, Target: toolsDir + "/text"}}}
	out, err := w.Output(context.Background(), []string{"/bin/sh", "-c", "command -v tac; echo fallthrough | tac"})
	if err != nil || string(out) != "/opt/multica-sandbox/tools/text/bin/tac\nfallthrough\n" {
		t.Fatalf("got %q %v", out, err)
	}
}
