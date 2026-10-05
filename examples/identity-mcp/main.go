package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: identity-example init|gateway|scenario|health")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "health":
		client := &http.Client{Timeout: time.Second}
		var response *http.Response
		response, err = client.Get("http://localhost:8080/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode != 204 {
				err = fmt.Errorf("unhealthy")
			}
		}
	case "init":
		err = initialize()
	case "gateway":
		err = gateway()
	case "evidence":
		err = printEvidence()
	case "rotation":
		err = rotation()
	case "scenario":
		err = scenario()
	default:
		err = fmt.Errorf("unknown example mode")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func randomSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func initialize() error {
	if _, err := os.Stat("/realm/realm.json"); err == nil {
		for _, name := range []string{"client", "admin"} {
			if _, err := os.Stat("/secrets/" + name); err != nil {
				return fmt.Errorf("incomplete fixture: remove example volumes")
			}
		}
		return nil
	}
	secret, admin := randomSecret(), randomSecret()
	realm := map[string]any{"realm": "sandbox-example", "enabled": true, "accessTokenLifespan": fixtureTTL(), "users": []any{map[string]any{"id": serviceSubject, "username": "service-account-example-agent", "enabled": true, "serviceAccountClientId": "example-agent"}}, "clients": []any{map[string]any{
		"clientId": "example-agent", "secret": secret, "enabled": true, "publicClient": false, "serviceAccountsEnabled": true, "standardFlowEnabled": false, "directAccessGrantsEnabled": false,
		"protocolMappers": []any{map[string]any{"name": "mcp-audience", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper", "config": map[string]string{"included.custom.audience": "sandbox-mcp", "access.token.claim": "true", "id.token.claim": "false"}}},
	}}}
	data, _ := json.Marshal(realm)
	if err := os.WriteFile("/realm/realm.json", data, 0644); err != nil {
		return err
	}
	if err := os.WriteFile("/secrets/client", []byte(secret), 0600); err != nil {
		return err
	}
	return os.WriteFile("/secrets/admin", []byte(admin), 0600)
}
func serve(address string, h http.Handler) error {
	server := &http.Server{Addr: address, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, MaxHeaderBytes: 16 << 10}
	return server.ListenAndServe()
}

func fixtureTTL() int {
	if os.Getenv("ROTATION_FIXTURE") == "1" {
		return 20
	}
	return 120
}
