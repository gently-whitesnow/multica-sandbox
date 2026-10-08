package main

import (
	"bytes"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"strings"
)

// gitFixture serves sandbox/fixture.git and sandbox/other.git over smart HTTP behind
// basic auth with the fixture-only /secrets/git password (ADR 0015 tests).
func gitFixture() error {
	password, err := os.ReadFile("/secrets/git")
	if err != nil {
		return err
	}
	for _, name := range []string{"fixture", "other"} {
		if err := createRepository("/srv/git/sandbox/"+name+".git", name); err != nil {
			return err
		}
	}
	backend := &cgi.Handler{Path: "/usr/lib/git-core/git-http-backend", Env: []string{"GIT_PROJECT_ROOT=/srv/git", "GIT_HTTP_EXPORT_ALL=1"}}
	return serve(":8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "fixture" || subtle.ConstantTimeCompare([]byte(pass), bytes.TrimSpace(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Header.Del("Authorization")
		// git-http-backend needs CONTENT_LENGTH; buffer chunked fixture bodies.
		body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
		backend.ServeHTTP(w, r)
	}))
}

func createRepository(bare, name string) error {
	work, err := os.MkdirTemp("", name)
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	script := `set -e
git init -q -b main "$1"
printf 'Repository fixture %s\n' "$3" > "$1/README"
git -C "$1" add README
git -C "$1" -c user.name=Fixture -c user.email=fixture@example.invalid commit -q -m fixture
git clone -q --bare "$1" "$2"
git -C "$2" config http.receivepack true`
	out, err := exec.Command("/bin/sh", "-c", script, "git", work, bare, name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
