package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPruneRemovesOnlyRebuildableOutputs(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	write := func(rel string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil || os.WriteFile(path, []byte("data"), 0600) != nil {
			t.Fatal(err)
		}
	}
	link := func(target, rel string) {
		if err := os.Symlink(target, filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{
		".multica-sandbox/home/.cache/go-build/x", ".multica-sandbox/home/.npm/_cacache/x",
		".multica-sandbox/home/.nuget/packages/p/x", ".multica-sandbox/tmp/x", ".multica-sandbox/opencode.db",
		".multica-sandbox/home/.gitconfig", ".multica/project/resources.json",
		"app/package.json", "app/node_modules/dep/index.js", "app/packages/web/.next/cache/x",
		"app/packages/web/.turbo/x", "app/packages/web/src/index.ts", "app/dist/bundle.js",
		"app/.git/HEAD", "app/.git/node_modules/x",
		"svc/Svc.csproj", "svc/bin/Debug/x.dll", "svc/obj/x.json", "svc/Program.cs",
		"tool/bin/run", "deep/1/2/3/4/5/6/7/8/node_modules/x",
	} {
		write(rel)
	}
	// The Go module cache is read-only, yet owned by the attempt user.
	write(".multica-sandbox/home/go/pkg/mod/example.com/m@v1/go.mod")
	for _, dir := range []string{"example.com/m@v1", "example.com"} {
		if err := os.Chmod(filepath.Join(root, ".multica-sandbox/home/go/pkg/mod", dir), 0500); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(outside, "node_modules"), 0700); err != nil || os.WriteFile(filepath.Join(outside, "keep"), nil, 0600) != nil {
		t.Fatal(err)
	}
	link(outside, "app/linked")
	link(outside, "app/packages/node_modules")
	if err := os.MkdirAll(filepath.Join(root, "other"), 0700); err != nil {
		t.Fatal(err)
	}
	link(outside, "other/.cache")

	var measured bytes.Buffer
	if err := report(&measured, root, false); err != nil {
		t.Fatal(err)
	}
	var before usage
	if json.Unmarshal(measured.Bytes(), &before) != nil || before.Size == 0 || before.Freed != 0 {
		t.Fatalf("du %s", measured.String())
	}
	if _, err := os.Stat(filepath.Join(root, "app/node_modules")); err != nil {
		t.Fatal("du removed files")
	}
	after, err := prune(root, true)
	if err != nil || after.Failed != 0 || after.Freed == 0 || after.Size >= before.Size {
		t.Fatalf("prune %+v %v", after, err)
	}
	for _, rel := range []string{
		".multica-sandbox/home/.cache", ".multica-sandbox/home/.npm", ".multica-sandbox/home/.nuget/packages",
		".multica-sandbox/home/go/pkg/mod", ".multica-sandbox/tmp", "app/node_modules", "app/packages/web/.next",
		"app/packages/web/.turbo", "svc/bin", "svc/obj",
	} {
		if _, err := os.Lstat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("%s kept", rel)
		}
	}
	// Sources, Git data, session state, dist, bin without a project, beyond the depth bound
	// and symlink targets stay.
	for _, rel := range []string{
		".multica-sandbox/opencode.db", ".multica-sandbox/home/.gitconfig", ".multica-sandbox/home/.nuget",
		".multica/project/resources.json", "app/package.json", "app/packages/web/src/index.ts", "app/dist/bundle.js",
		"app/.git/HEAD", "app/.git/node_modules/x", "svc/Svc.csproj", "svc/Program.cs", "tool/bin/run",
		"deep/1/2/3/4/5/6/7/8/node_modules/x", "app/linked", "app/packages/node_modules", "other/.cache",
	} {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Errorf("%s removed: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "node_modules")); err != nil {
		t.Fatal("followed a symlink")
	}
}

func TestPruneRefusesSymlinkedHomeAndBadRoot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "home/.npm"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".multica-sandbox"), 0700); err != nil || os.Symlink(filepath.Join(outside, "home"), filepath.Join(root, ".multica-sandbox/home")) != nil {
		t.Fatal(err)
	}
	if _, err := prune(root, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "home/.npm")); err != nil {
		t.Fatal("pruned through a symlinked HOME")
	}
	if _, err := prune(filepath.Join(root, ".multica-sandbox/home"), true); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("symlinked root accepted: %v", err)
	}
}
