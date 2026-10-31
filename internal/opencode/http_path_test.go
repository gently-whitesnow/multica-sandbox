package opencode

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/relay"
)

type pathHTTPFixture struct {
	m                   *MCPRelay
	r                   *running
	front, opaque, file string
	seen                chan [4]string
	records             chan string
	status              atomic.Int32
}

func newPathHTTPFixture(t *testing.T) *pathHTTPFixture {
	t.Helper()
	f := &pathHTTPFixture{m: NewMCPRelay(), seen: make(chan [4]string, 16), records: make(chan string, 16)}
	f.status.Store(200)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.seen <- [4]string{r.URL.Path, r.Header.Get("Authorization"), r.URL.RawQuery, string(body)}
		if status := int(f.status.Load()); status != 200 {
			w.Header().Set("Location", "https://redirect.example"+r.URL.Path)
			w.Header().Set("X-Debug-Url", r.URL.String())
			w.Header().Set("Trailer", "X-Request-Url")
			http.Error(w, r.URL.String(), status)
			w.Header().Set("X-Request-Url", r.URL.String())
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	t.Cleanup(upstream.Close)
	f.file = filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(f.file, []byte("123:private-path-one"), 0600); err != nil {
		t.Fatal(err)
	}
	task := safeTask()
	service := identity.HTTPService{Name: "messenger", Origin: upstream.URL + "/base/", AgentIDs: []string{task.AgentID}, PathCredential: &identity.HTTPPathCredential{File: f.file, Prefix: "/bot"}, Routes: []identity.HTTPRoute{{Method: "POST", Path: "/sendMessage"}}}
	source, err := identity.New(identity.Config{Version: 1, AllowHTTP: true, Issuer: identity.IssuerConfig{URL: "https://issuer.example", TokenURL: "https://issuer.example/token", JWKSURL: "https://issuer.example/keys", MaxTTLSeconds: 300}, Bindings: []identity.Binding{{WorkspaceID: task.WorkspaceID, AgentID: task.AgentID, ClientID: "fixture", SecretFile: f.file}}, HTTPServices: []identity.HTTPService{service}}, "https://multica.example")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := relay.New(f.m, relay.Policy{Allow: MCPRelayPath, Limit: 1 << 20, Record: func(r *http.Request, _ int, err error, _ time.Duration) {
		f.records <- r.URL.String() + fmt.Sprint(err)
	}})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)
	f.front = front.URL
	f.r = &running{task: task, adapter: &Adapter{Server: "https://multica.example", Issuer: source, MCPRelay: f.m, MCPRelayURL: f.front}}
	if err = f.r.configureHTTP(); err != nil {
		t.Fatal(err)
	}
	if err = f.r.refreshHTTP(context.Background(), identity.Ref{Server: "https://multica.example", WorkspaceID: task.WorkspaceID, AgentID: task.AgentID}); err != nil {
		t.Fatal(err)
	}
	f.opaque, err = f.m.issue(f.r)
	if err != nil {
		t.Fatal(err)
	}
	f.r.mcpToken = f.opaque
	return f
}

func TestHTTPPathCredentialMockDeliveryRotationAndRevocation(t *testing.T) {
	f := newPathHTTPFixture(t)
	for _, secret := range []string{"123:private-path-one", "123:private-path-two"} {
		if err := os.WriteFile(f.file, []byte(secret), 0600); err != nil {
			t.Fatal(err)
		}
		f.r.httpTokens["messenger"] = identity.NewAccessToken("expired", time.Now())
		if err := f.r.refreshHTTP(context.Background(), identity.Ref{Server: "https://multica.example", WorkspaceID: f.r.task.WorkspaceID, AgentID: f.r.task.AgentID}); err != nil {
			t.Fatal(err)
		}
		body := `{"chat_id":"fixture-only","text":"Daily summary"}`
		request, _ := http.NewRequest("POST", f.front+"/http/messenger/sendMessage?override=ignored", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+f.opaque)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 || <-f.seen != [4]string{"/base/bot" + secret + "/sendMessage", "", "", body} {
			t.Fatal("mock delivery did not preserve body and privately inject path credential")
		}
		for _, value := range f.r.environment() {
			if strings.Contains(value, "private-path") {
				t.Fatal("path secret reached attempt environment")
			}
		}
	}
	f.m.revoke(f.r.task.AttemptKey())
	response := mcpRequest(t, "POST", f.front+"/http/messenger/sendMessage", f.opaque)
	response.Body.Close()
	if response.StatusCode < 400 || len(f.seen) != 0 {
		t.Fatal("ended attempt delivered a message")
	}
}

func TestHTTPPathCredentialRejectsForeignRoutesAndAgents(t *testing.T) {
	f := newPathHTTPFixture(t)
	for _, check := range []struct{ method, path string }{
		{"GET", "/http/messenger/sendMessage"},
		{"POST", "/http/messenger/sendDocument"},
		{"POST", "/http/messenger/sendMessage/extra"},
		{"POST", "/http/messenger/botcaller/sendMessage"},
		{"POST", "/http/messenger/%2e%2e/sendMessage"},
	} {
		response := mcpRequest(t, check.method, f.front+check.path, f.opaque)
		response.Body.Close()
		if response.StatusCode < 400 {
			t.Fatal("unapproved delivery route forwarded")
		}
	}
	foreign := &running{task: safeTask(), adapter: f.r.adapter}
	foreign.task.AgentID = "20000000-0000-4000-8000-000000000002"
	foreign.task.Agent.ID = foreign.task.AgentID
	foreign.task.ID = "40000000-0000-4000-8000-000000000002"
	if err := foreign.configureHTTP(); err != nil {
		t.Fatal(err)
	}
	opaque, err := f.m.issue(foreign)
	if err != nil {
		t.Fatal(err)
	}
	response := mcpRequest(t, "POST", f.front+"/http/messenger/sendMessage", opaque)
	response.Body.Close()
	if response.StatusCode < 400 || len(f.seen) != 0 || len(foreign.environment()) != 0 {
		t.Fatal("foreign agent discovered or used delivery service")
	}
}

func TestHTTPPathCredentialWithholdsErrorsRedirectsAndRecordedPaths(t *testing.T) {
	f := newPathHTTPFixture(t)
	for _, status := range []int{400, 401, 500, 302, 307} {
		f.status.Store(int32(status))
		response := mcpRequest(t, "POST", f.front+"/http/messenger/sendMessage", f.opaque)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode < 400 || strings.Contains(string(body)+fmt.Sprint(response.Header, response.Trailer)+<-f.records, "private-path") || response.Header.Get("Location") != "" || len(response.Trailer) != 0 {
			t.Fatal("path credential leaked through errors, redirect or recorded URL")
		}
	}
}
