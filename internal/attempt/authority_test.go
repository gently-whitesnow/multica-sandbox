package attempt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAuthorityRejectsRedirectAndRereadsSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(path, []byte("first"), 0600)
	leaked := false
	recipient := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true; w.WriteHeader(204) }))
	defer recipient.Close()
	mode := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == 0 {
			http.Redirect(w, r, recipient.URL, 307)
			return
		}
		if r.Header.Get("Authorization") != "Bearer second" {
			w.WriteHeader(403)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	a, err := New(Config{URL: server.URL, BearerFile: path, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.Apply(context.Background(), Grant{Action: "renew"}) == nil || leaked {
		t.Fatal("authority redirect accepted")
	}
	mode = 1
	_ = os.WriteFile(path, []byte("second"), 0600)
	if err := a.Apply(context.Background(), Grant{Action: "recover"}); err != nil {
		t.Fatal(err)
	}
}

// The cached bearer follows rotation by atomic replacement and in-place rewrites.
func TestAuthorityFollowsBearerRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	_ = os.WriteFile(path, []byte("one"), 0600)
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(204)
	}))
	defer server.Close()
	a, err := New(Config{URL: server.URL, BearerFile: path, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	apply := func(want string) {
		t.Helper()
		if err := a.Apply(context.Background(), Grant{Action: "renew"}); err != nil || got != "Bearer "+want {
			t.Fatalf("authorization %q want %q: %v", got, want, err)
		}
	}
	apply("one")
	apply("one")
	// Same size, swapped in like a mounted secret.
	_ = os.WriteFile(filepath.Join(dir, "next"), []byte("two"), 0600)
	if err := os.Rename(filepath.Join(dir, "next"), path); err != nil {
		t.Fatal(err)
	}
	apply("two")
	// Same size, rewritten in place.
	_ = os.WriteFile(path, []byte("six"), 0600)
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(path, later, later)
	apply("six")
	_ = os.Remove(path)
	if a.Apply(context.Background(), Grant{Action: "renew"}) == nil {
		t.Fatal("missing bearer file accepted")
	}
}
