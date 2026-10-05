package attempt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
