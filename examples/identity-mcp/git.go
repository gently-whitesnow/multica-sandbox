package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"strings"
)

// gitFixture serves sandbox/fixture.git and sandbox/other.git over smart HTTP behind
// basic auth with the fixture-only /secrets/git password, and an Enterprise-style
// pull request API behind "token <password>" that logs to /srv/git/pulls (ADR 0015 tests).
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
		if strings.HasPrefix(r.URL.Path, "/api/v3/") {
			pulls(w, r, bytes.TrimSpace(password))
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

// pulls accepts pull requests on sandbox/fixture like the GitHub REST API.
func pulls(w http.ResponseWriter, r *http.Request, password []byte) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	if subtle.ConstantTimeCompare([]byte(token), password) != 1 {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	var pr struct{ Title, Head, Base string }
	if r.Method != http.MethodPost || r.URL.Path != "/api/v3/repos/sandbox/fixture/pulls" || json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&pr) != nil || pr.Head == "" {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	f, err := os.OpenFile("/srv/git/pulls", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err == nil {
		_, err = fmt.Fprintf(f, "%s %s %q\n", pr.Head, pr.Base, pr.Title)
		f.Close()
	}
	if err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprint(w, `{"number":1,"html_url":"https://git.fixture.test/sandbox/fixture/pull/1"}`)
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
