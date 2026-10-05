package main

import (
	"context"
	"fmt"
	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
	"github.com/gently-whitesnow/multica-sandbox/internal/opencode"
	"time"
)

func rotationLease(ctx context.Context, adapter *opencode.Adapter, task multica.Task) error {
	task.ID = "40000000-0000-4000-8000-000000000030"
	token, err := adapter.Issuer.AcquireForMCP(ctx, identity.Ref{Server: adapter.Server, WorkspaceID: task.WorkspaceID, AgentID: task.AgentID}, "http://gateway:8080/mcp")
	if err != nil {
		return err
	}
	grant := attempt.Grant{Controller: adapter.Controller, Attempt: task.AttemptKey(), Server: adapter.Server, Workspace: task.WorkspaceID, Agent: task.AgentID, Task: task.ID, URL: "http://gateway:8080/mcp", TokenHash: attempt.Fingerprint(token.Bearer()), Action: "renew", ExpiresAt: time.Now().Add(time.Second).Unix()}
	if err := adapter.Authority.Apply(ctx, grant); err != nil {
		return err
	}
	time.Sleep(2 * time.Second)
	if !token.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("JWT expired before lease test")
	}
	if err := call(ctx, token.Bearer(), task.WorkspaceID, "document", false); err != nil {
		return err
	}
	grant.ExpiresAt = time.Now().Add(15 * time.Second).Unix()
	if err := adapter.Authority.Apply(ctx, grant); err != nil {
		return err
	}
	if err := call(ctx, token.Bearer(), task.WorkspaceID, "document", true); err != nil {
		return err
	}
	other := grant
	other.Attempt += ":other"
	if adapter.Authority.Apply(ctx, other) == nil {
		return fmt.Errorf("fingerprint moved between attempts")
	}
	if err := adapter.Authority.Apply(ctx, attempt.Grant{Controller: adapter.Controller, Action: "recover"}); err != nil {
		return err
	}
	if err := call(ctx, token.Bearer(), task.WorkspaceID, "document", false); err != nil {
		return err
	}
	if adapter.Authority.Apply(ctx, grant) == nil {
		return fmt.Errorf("ended attempt re-enrolled")
	}
	fmt.Println("PASS bounded lease expiry and recovery deny unexpired JWTs; fingerprint reassignment and ended-attempt re-enrollment refused")
	return nil
}
