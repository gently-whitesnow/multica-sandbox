package identity

import (
	"context"
	"testing"
)

func TestHTTPIdentityIsOptionalForAgentWithoutIAMEnvironment(t *testing.T) {
	fixture := newTokenFixture(t)
	config := fixture.config(t)
	config.AgentEnv, config.Bindings = true, nil
	config.HTTPServices = []HTTPService{
		{Name: "control", Origin: "https://control.example", Routes: []HTTPRoute{{Method: "GET", Path: "/settings"}}},
		{Name: "tasks", Origin: "https://tasks.example", BearerFile: secretFile(t, "service-secret"), AgentIDs: []string{agentA}, Routes: []HTTPRoute{{Method: "GET", Path: "/tasks"}}},
		pathCredentialService(t),
	}
	service, err := New(config, identityTestServer)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []Env{nil, agentEnv{"USER_SETTING": "ordinary"}} {
		services := service.HTTPServicesFor(ref(config), env)
		if len(services) != 2 || services[0].Name != "messenger" || services[1].Name != "tasks" {
			t.Fatal("agent without IAM must retain only its explicit static services")
		}
	}
	for _, env := range []Env{
		agentEnv{EnvClientID: "client-a", EnvClientSecret: "env-secret"},
		agentEnv{EnvClientID: "client-a"},
		agentEnv{EnvClientSecret: "env-secret"},
		agentEnv{EnvClientID: "", EnvClientSecret: ""},
	} {
		services := service.HTTPServicesFor(ref(config), env)
		if len(services) != 3 || services[0].Name != "control" {
			t.Fatal("configured or incomplete IAM must keep the identity helper selected")
		}
	}
	if fixture.gotSecret != "" {
		t.Fatal("service selection must not probe the issuer")
	}
	if _, err = service.AcquireForHTTP(context.Background(), ref(config), agentEnv{EnvClientID: "client-a"}, "control"); err != ErrDenied || fixture.gotSecret != "" {
		t.Fatal("partial IAM pair must fail instead of silently hiding the helper")
	}
	if _, err = service.AcquireForMCP(context.Background(), ref(config), nil, "https://tools.example/mcp"); err != ErrDenied || fixture.gotSecret != "" {
		t.Fatal("explicit IAM MCP must still refuse missing credentials")
	}
	if _, err = service.AcquireForHTTP(context.Background(), ref(config), agentEnv{EnvClientID: "client-a", EnvClientSecret: "env-secret"}, "control"); err != nil || fixture.gotSecret != "env-secret" {
		t.Fatal("complete IAM pair did not activate HTTP identity", err)
	}
}

func TestHTTPIdentityKeepsConfiguredResolversWithoutAgentEnvironment(t *testing.T) {
	for _, external := range []bool{false, true} {
		config := config(t)
		config.AgentEnv = true
		config.HTTPServices = []HTTPService{{Name: "control", Origin: "https://control.example", Routes: []HTTPRoute{{Method: "GET", Path: "/settings"}}}}
		if external {
			config.Bindings = nil
			config.External = &ExternalConfig{URL: "https://resolver.example/identity", BearerFile: "/run/secrets/resolver"}
		}
		service, err := New(config, identityTestServer)
		if err != nil {
			t.Fatal(err)
		}
		if services := service.HTTPServicesFor(ref(config), nil); len(services) != 1 || services[0].Name != "control" {
			t.Fatal("configured resolver lost its HTTP identity service")
		}
	}
}
