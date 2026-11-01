package identity

import (
	"context"
	"os"
	"testing"
)

func pathCredentialService(t *testing.T) HTTPService {
	return HTTPService{Name: "messenger", Origin: "https://messages.example", AgentIDs: []string{agentA},
		PathCredential: &HTTPPathCredential{File: secretFile(t, "123:first-private"), Prefix: "/bot"},
		Routes:         []HTTPRoute{{Method: "POST", Path: "/sendMessage"}}}
}

func TestHTTPPathCredentialIsolationRotationAndImmutablePolicy(t *testing.T) {
	c := config(t)
	c.HTTPServices = []HTTPService{pathCredentialService(t)}
	s, err := New(c, identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	foreign := Ref{identityTestServer, workspaceA, agentB}
	if len(s.HTTPServicesFor(foreign, nil)) != 0 {
		t.Fatal("foreign agent discovered credential service")
	}
	if _, err = s.AcquireForHTTP(context.Background(), foreign, nil, "messenger"); err != ErrDenied {
		t.Fatal("foreign agent acquired path credential")
	}
	file := c.HTTPServices[0].PathCredential.File
	c.HTTPServices[0].PathCredential.Prefix = "/mutated"
	for _, secret := range []string{"123:first-private", "123:rotated-private"} {
		if err = os.WriteFile(file, []byte(secret), 0600); err != nil {
			t.Fatal(err)
		}
		token, e := s.AcquireForHTTP(context.Background(), ref(c), nil, "messenger")
		path, ok := s.HTTPServices()[0].CredentialPath("/sendMessage", token.Bearer())
		if e != nil || !ok || path != "/bot"+secret+"/sendMessage" {
			t.Fatal("path credential rotation or immutable prefix failed")
		}
	}
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireForHTTP(context.Background(), ref(c), nil, "messenger"); err != ErrDenied {
		t.Fatal("missing path credential fell back to JWT")
	}
}

func TestHTTPPathCredentialRejectsAmbiguousAndEscapingPolicy(t *testing.T) {
	for _, change := range []func(*HTTPService){
		func(s *HTTPService) { s.AgentIDs = nil },
		func(s *HTTPService) { s.BearerFile = "/private/also-bearer" },
		func(s *HTTPService) { s.PathCredential.File = "relative" },
		func(s *HTTPService) { s.PathCredential.Prefix = "/api/../bot" },
		func(s *HTTPService) { s.PathCredential.Prefix = "/bot%2f" },
		func(s *HTTPService) { s.PathCredential.Prefix = "/bot?other=" },
		func(s *HTTPService) { s.PathCredential.Prefix = "" },
		func(s *HTTPService) { s.Routes = []HTTPRoute{{Method: "POST", PathPrefix: "/send/"}} },
	} {
		c := config(t)
		service := pathCredentialService(t)
		change(&service)
		c.HTTPServices = []HTTPService{service}
		if _, err := New(c, identityTestServer); err != ErrDenied {
			t.Fatal("ambiguous path credential policy accepted")
		}
	}
	for _, secret := range []string{"..", ".", "../escape", "abc%2fescape", "secret?query", "secret#fragment", "secret\\escape", "secret with space", ""} {
		service := pathCredentialService(t)
		if _, ok := service.CredentialPath("/sendMessage", secret); ok {
			t.Fatal("secret escaped its path segment")
		}
	}
}
