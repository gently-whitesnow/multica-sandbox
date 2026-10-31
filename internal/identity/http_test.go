package identity

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestHTTPRecipientConfigurationAndIssuance(t *testing.T) {
	f := newTokenFixture(t)
	c := f.config(t)
	c.HTTPServices = []HTTPService{{Name: "control", Origin: "https://control.example", Routes: []HTTPRoute{{Method: "GET", Path: "/api/v1/settings"}, {Method: "POST", PathPrefix: "/api/v1/deployment-access/"}}}}
	s, err := New(c, identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireForHTTP(context.Background(), ref(c), nil, "other"); err != ErrDenied || f.gotSecret != "" {
		t.Fatal("unapproved HTTP recipient contacted issuer")
	}
	if _, err = s.AcquireForHTTP(context.Background(), ref(c), nil, "control"); err != nil {
		t.Fatal(err)
	}
	c.HTTPServices[0].Routes[0].Path = "/mutated"
	services := s.HTTPServices()
	if !services[0].Allows("GET", "/api/v1/settings") || services[0].Allows("POST", "/api/v1/settings") || services[0].Allows("POST", "/api/v1/deployment-access-unapproved/x") {
		t.Fatal("HTTP routes or immutable config lost")
	}
	services[0].Routes[0].Path = "/mutated"
	if !s.HTTPServices()[0].Allows("GET", "/api/v1/settings") {
		t.Fatal("returned HTTP config mutates policy")
	}
}

func TestHTTPRecipientRejectsAmbiguousConfiguration(t *testing.T) {
	valid := HTTPService{Name: "control", Origin: "https://control.example", Routes: []HTTPRoute{{Method: "GET", Path: "/api/v1/settings"}}}
	for _, change := range []func(*HTTPService){
		func(s *HTTPService) { s.Name = "CONTROL" },
		func(s *HTTPService) { s.Origin = "https://user:secret@control.example" },
		func(s *HTTPService) { s.Origin = "https://control.example/path/../secret" },
		func(s *HTTPService) { s.Routes = nil },
		func(s *HTTPService) { s.Routes = []HTTPRoute{{Method: "CONNECT", Path: "/api"}} },
		func(s *HTTPService) { s.Routes = []HTTPRoute{{Method: "GET", PathPrefix: "/api"}} },
		func(s *HTTPService) { s.Routes = []HTTPRoute{{Method: "GET", PathPrefix: "/"}} },
		func(s *HTTPService) { s.Routes = []HTTPRoute{{Method: "GET", Path: "/api/../secret"}} },
		func(s *HTTPService) { s.Routes = []HTTPRoute{{Method: "GET", Path: "/api%2fsecret"}} },
		func(s *HTTPService) { s.Routes = []HTTPRoute{{Method: "GET", Path: "/api?url=other"}} },
		func(s *HTTPService) { s.Routes = []HTTPRoute{{Method: "GET", Path: "/api", PathPrefix: "/api/"}} },
	} {
		c := config(t)
		v := valid
		v.Routes = append([]HTTPRoute(nil), valid.Routes...)
		change(&v)
		c.HTTPServices = []HTTPService{v}
		if _, err := New(c, identityTestServer); err != ErrDenied {
			t.Fatal("ambiguous HTTP recipient accepted")
		}
	}
}

func TestHTTPServiceCredentialIsExplicitAndAgentScoped(t *testing.T) {
	c := config(t)
	file := secretFile(t, "upstream-only-one")
	c.HTTPServices = []HTTPService{{Name: "tasks", Origin: "https://tasks.example/external/agent", BearerFile: file, AgentIDs: []string{agentA}, Headers: map[string]string{"X-Client": "fixture"}, Routes: []HTTPRoute{{Method: "GET", PathPrefix: "/api/"}}}}
	service, err := New(c, identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	foreign := Ref{identityTestServer, workspaceA, agentB}
	if len(service.HTTPServicesFor(foreign)) != 0 {
		t.Fatal("foreign agent discovered credential-backed service")
	}
	if _, err = service.AcquireForHTTP(context.Background(), foreign, nil, "tasks"); err != ErrDenied {
		t.Fatal("foreign agent acquired service credential")
	}
	for _, value := range []string{"upstream-only-one", "upstream-only-two"} {
		if err = os.WriteFile(file, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		token, e := service.AcquireForHTTP(context.Background(), ref(c), nil, "tasks")
		if e != nil || token.Bearer() != value || time.Until(token.ExpiresAt) > 30*time.Second {
			t.Fatal("service credential not re-read or lease unbounded", e)
		}
	}
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err = service.AcquireForHTTP(context.Background(), ref(c), nil, "tasks"); err != ErrDenied {
		t.Fatal("unavailable service credential fell back to JWT")
	}
	c.HTTPServices[0].AgentIDs = nil
	if _, err = New(c, identityTestServer); err != ErrDenied {
		t.Fatal("service credential accepted without explicit agent allowlist")
	}
}
