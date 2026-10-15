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
	// Identity no longer needs an authority; an unreadable identity file still fails.
	if _, _, err = attemptIdentity(context.Background(), Config{OpenCode: &OpenCodeConfig{IdentityFile: "/nonexistent/identity.json"}}); err == nil {
		t.Fatal("unreadable identity file accepted")
	}
	c := Config{OpenCode: &OpenCodeConfig{Authority: attempt.Config{URL: "https://authority.example.invalid/v1", BearerFile: "/nonexistent/bearer"}}}
	if _, _, err = attemptIdentity(context.Background(), c); err == nil {
		t.Fatal("configured authority must answer recover")
	}
}
