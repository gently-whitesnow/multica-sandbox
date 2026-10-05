package opencode

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
)

type stubIssuer struct {
	calls  int
	denyAt int
}

func (s *stubIssuer) AcquireForMCP(context.Context, identity.Ref, string) (identity.AccessToken, error) {
	s.calls++
	if s.calls == s.denyAt {
		return identity.AccessToken{}, identity.ErrDenied
	}
	return identity.AccessToken{ExpiresAt: time.Now().Add(11 * time.Second)}, nil
}

type stubAuthority struct {
	sync.Mutex
	actions    []string
	deny       bool
	denyRevoke bool
}

func (s *stubAuthority) Apply(_ context.Context, g attempt.Grant) error {
	s.Lock()
	defer s.Unlock()
	s.actions = append(s.actions, g.Action)
	if (s.deny && g.Action == "renew") || (s.denyRevoke && g.Action == "revoke") {
		return attempt.ErrDenied
	}
	return nil
}

type stubStatus struct{}

func (stubStatus) Status(context.Context, string) (string, error) { return "running", nil }

type stubWorkload struct {
	writes  int
	removed bool
	runs    int
}

func (s *stubWorkload) Start(context.Context, string) (execution.ProjectedRun, error) { return s, nil }
func (s *stubWorkload) Write(context.Context, string, []byte) error                   { s.writes++; return nil }
func (s *stubWorkload) Execute(ctx context.Context, args []string) error {
	s.runs++
	if len(args) >= 3 && !strings.HasPrefix(args[2], "exec ") {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}
func (s *stubWorkload) Wait(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
func (s *stubWorkload) Remove(context.Context) error   { s.removed = true; return nil }

func TestRenewalOutageStopsAndRevokes(t *testing.T) {
	for _, source := range []string{"issuer", "authority"} {
		t.Run(source, func(t *testing.T) {
			issuer := &stubIssuer{}
			if source == "issuer" {
				issuer.denyAt = 2
			}
			authority := &stubAuthority{}
			workload := &stubWorkload{}
			a := Adapter{Server: "https://multica.example.invalid", Controller: "controller", Issuer: issuer, Authority: authority, Workloads: workload, Status: stubStatus{}}
			run, err := a.Start(context.Background(), safeTask())
			if err != nil {
				t.Fatal(err)
			}
			// Trigger one of the two independent external dependencies failing on renewal.
			if source == "authority" {
				authority.Lock()
				authority.deny = true
				authority.Unlock()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			if err := run.Wait(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("outage did not stop task: %v", err)
			}
			if err := run.Remove(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !workload.removed || authority.actions[len(authority.actions)-1] != "revoke" {
				t.Fatal("outage failed cleanup/revocation")
			}
		})
	}
}
func TestInitialDenialNeverStartsWorkload(t *testing.T) {
	workload := &stubWorkload{}
	authority := &stubAuthority{deny: true}
	a := Adapter{Issuer: &stubIssuer{}, Authority: authority, Workloads: workload, Status: stubStatus{}}
	if _, err := a.Start(context.Background(), safeTask()); err == nil {
		t.Fatal("denied grant accepted")
	}
	if workload.writes != 0 || workload.runs != 0 || authority.actions[len(authority.actions)-1] != "revoke" {
		t.Fatal("credential projection before authorization")
	}
}
func TestCancellationJoinsRenewalBeforeCleanup(t *testing.T) {
	workload := &stubWorkload{}
	authority := &stubAuthority{}
	a := Adapter{Issuer: &stubIssuer{}, Authority: authority, Workloads: workload, Status: stubStatus{}}
	run, err := a.Start(context.Background(), safeTask())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !workload.removed || authority.actions[len(authority.actions)-1] != "revoke" {
		t.Fatal("cancellation failed cleanup")
	}
}

func TestUncertainRevocationIsNotSafeRejection(t *testing.T) {
	a := Adapter{Issuer: &stubIssuer{}, Authority: &stubAuthority{deny: true, denyRevoke: true}, Workloads: &stubWorkload{}, Status: stubStatus{}}
	_, err := a.Start(context.Background(), safeTask())
	var rejected *execution.RejectedError
	if err == nil || errors.As(err, &rejected) {
		t.Fatal("uncertain authorization cleanup treated as safe rejection")
	}
}

func (s *stubWorkload) Stream(ctx context.Context, args []string, _ func(io.Reader) error) error {
	return s.Execute(ctx, args)
}
func (s *stubWorkload) Result() execution.Result { return execution.Result{} }
