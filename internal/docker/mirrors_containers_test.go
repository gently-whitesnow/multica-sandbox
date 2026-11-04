//go:build containers

package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

// fakeGit stands in for git in the refresh container: init creates the directory and
// fetch records who fetched what with which grant, then writes a pack.
const fakeGit = `#!/bin/sh
dir=
while [ $# -gt 0 ]; do
	case $1 in
	-C | -c) [ "$1" = -C ] && dir=$2; shift 2 ;;
	init) for last; do :; done; mkdir -p "$last/objects"; exit ;;
	fetch) while [ "$1" != -- ]; do shift; done
		printf '%s %s %s\n' "$(id -u)" "${GIT_CONFIG_VALUE_0:-none}" "$2" >"$dir/fetched"
		head -c 65536 /dev/zero >"$dir/objects/pack"; exit ;;
	*) shift ;;
	esac
done
exit 1
`

// gitStubImage commits testImage with fakeGit, digest-pinned like agent images. A commit,
// unlike a build, does not resolve the base image in a registry.
func gitStubImage(t *testing.T) string {
	t.Helper()
	ctx, dir := context.Background(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(fakeGit), 0555); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("multica-sandbox-git-stub-test:%d", time.Now().UnixNano())
	out, err := command(ctx, "create", testImage, "true")
	container := strings.TrimSpace(string(out))
	if err == nil {
		_, err = command(ctx, "cp", filepath.Join(dir, "git"), container+":/usr/local/bin/git")
	}
	if err == nil {
		_, err = command(ctx, "commit", container, tag)
	}
	_, _ = command(ctx, "rm", "-f", container)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command(context.Background(), "rmi", "-f", tag) })
	digest, err := command(ctx, "image", "inspect", "--format", "{{index .RepoDigests 0}}", tag)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(digest))
}

type liveGrants struct{ workspace string }

func (g liveGrants) Serves(workspace, url string) bool { return workspace == g.workspace }
func (liveGrants) Issue(string, string, []string) (map[string]string, error) {
	return map[string]string{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "http.proxy", "GIT_CONFIG_VALUE_0": "http://grant.example.test:8094"}, nil
}
func (liveGrants) Revoke(string) {}

// A refresh container writes the workspace mirror volume as the attempt user with the
// relay grant and leaves nothing behind; attempts of the workspace read the mirror and
// cannot write it; the session sweep removes it with the helper.
func TestMirrorRefreshMountAndCollection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	owner := fmt.Sprintf("94000000-0000-4000-8000-%012d", time.Now().UnixNano()%1000000000000)
	template, peer := "sandbox-template-"+owner, "sandbox-peer-"+owner
	if _, err := command(ctx, "network", "create", "--internal", template); err != nil {
		t.Fatal(err)
	}
	if _, err := command(ctx, "run", "-d", "--name", peer, "--network", template, testImage, "sleep", "300"); err != nil {
		t.Fatal(err)
	}
	conversation := execution.Workdir{Workspace: "10000000-0000-4000-8000-000000000001", Agent: "a1000000-0000-4000-8000-000000000001", Issue: "a2000000-0000-4000-8000-000000000001"}
	sessions := &Sessions{Dir: t.TempDir(), TTL: time.Hour, Max: 4}
	backend := &Projected{Backend: Backend{Image: gitStubImage(t), Owner: owner, Command: []string{"/bin/sh"}}, Network: template, Peers: []string{peer},
		Helper: helperImage(t), Forward: "endpoint:8091", Sessions: sessions}
	mirrors := &Mirrors{Workloads: backend, Git: liveGrants{conversation.Workspace}, Dir: t.TempDir(), Refresh: time.Hour, Wait: time.Minute, Timeout: time.Minute, TTL: time.Hour,
		Report: func(err error) { t.Log(err) }}
	backend.Mirrors, sessions.Mirrors = mirrors, mirrors
	t.Cleanup(func() {
		ctx := context.Background()
		_ = backend.Reconcile(ctx)
		if out, err := command(ctx, "volume", "ls", "-q", "--filter", "label="+ownerLabel+"="+owner); err == nil {
			for _, name := range strings.Fields(string(out)) {
				_, _ = command(ctx, "volume", "rm", "-f", name)
			}
		}
		_, _ = command(ctx, "rm", "-fv", peer)
		_, _ = command(ctx, "network", "rm", template)
	})
	const url = "https://git.example.test/team/fixture.git"
	key, release, ok := mirrors.Ensure(ctx, conversation.Workspace, url)
	if !ok {
		t.Fatal("first refresh failed")
	}
	release()
	if out, _ := command(ctx, "ps", "-aq", "--filter", "label="+ownerLabel+"="+owner); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("refresh left containers %s", out)
	}
	if out, _ := command(ctx, "network", "ls", "-q", "--filter", "label="+ownerLabel+"="+owner); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("refresh left networks %s", out)
	}
	shell := func(script string) string {
		r, err := backend.Start(ctx, fmt.Sprintf("reader-%d", time.Now().UnixNano()), conversation)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Remove(ctx)
		out, err := r.Capture(ctx, []string{"/bin/sh", "-c", script}, nil)
		if err != nil {
			t.Fatalf("%s: %v %q", script, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mirror := repo.MirrorDir + "/" + key
	got := shell(`cat ` + mirror + `/fetched; touch ` + mirror + `/x 2>/dev/null && echo writable || echo read-only; test -e ` + mirror + `.new && echo unfinished; true`)
	if got != "65532 http://grant.example.test:8094 "+url+"\nread-only" {
		t.Fatalf("attempt view: %q", got)
	}
	// The sweep measures the mirror, then removes it once unused for TTL.
	r := Reclaim{}
	if err := mirrors.expire(ctx, &r); err != nil || r.Mirrors != 0 {
		t.Fatalf("expire %v %+v", err, r)
	}
	if idle, total := mirrors.idle([]string{mirrors.volumeName(conversation.Workspace)}); len(idle) != 1 || total < 65536 {
		t.Fatalf("measured %+v %d", idle, total)
	}
	mirrors.TTL = 0
	var reclaims []Reclaim
	sessions.Reclaimed = func(r Reclaim) { reclaims = append(reclaims, r) }
	if err := sessions.collect(ctx, owner, true); err != nil {
		t.Fatal(err)
	}
	if len(reclaims) != 1 || reclaims[0].Mirrors != 1 || reclaims[0].MirrorBytes < 65536 {
		t.Fatalf("reclaims %+v", reclaims)
	}
	if got := shell(`test -e ` + mirror + ` || echo gone`); got != "gone" {
		t.Fatalf("collected mirror: %q", got)
	}
}
