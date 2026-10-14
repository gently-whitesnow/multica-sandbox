package service

import (
	"context"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
)

func TestAttemptIdentityIsOptional(t *testing.T) {
	issuer, authority, err := attemptIdentity(context.Background(), Config{OpenCode: &OpenCodeConfig{}})
	if err != nil || issuer != nil || authority != nil {
		t.Fatalf("absent identity and authority: %v %v %v", issuer, authority, err)
	}
	if _, _, err = attemptIdentity(context.Background(), Config{OpenCode: &OpenCodeConfig{IdentityFile: "/run/identity.json"}}); err == nil {
		t.Fatal("identity without authority accepted")
	}
	c := Config{OpenCode: &OpenCodeConfig{Authority: attempt.Config{URL: "https://authority.example.invalid/v1", BearerFile: "/nonexistent/bearer"}}}
	if _, _, err = attemptIdentity(context.Background(), c); err == nil {
		t.Fatal("configured authority must answer recover")
	}
}
