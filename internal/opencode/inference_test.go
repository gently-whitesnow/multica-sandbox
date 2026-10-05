package opencode

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
)

type changingInference struct {
	calls int
	mode  string
}

func (s *changingInference) Acquire(context.Context, identity.Ref) (inference.Session, error) {
	s.calls++
	if s.mode == "denied" {
		return inference.Session{}, inference.ErrDenied
	}
	target := inference.Target{Gateway: inference.Gateway{URL: "https://inference.example.invalid/v1", Issuer: "inference"}, Model: "demo", Models: map[string]inference.Model{"demo": {Context: 64000, Output: 4096}}}
	if s.calls > 1 {
		switch s.mode {
		case "outage":
			return inference.Session{}, inference.ErrDenied
		case "changed":
			target.Models["demo"] = inference.Model{Context: 64000, Output: 1024}
		}
	}
	return inference.Session{Target: target, Token: identity.AccessToken{ExpiresAt: time.Now().Add(11 * time.Second)}}, nil
}
func TestInferenceDenialBeforeWorkload(t *testing.T) {
	workload := &stubWorkload{}
	a := Adapter{Issuer: &stubIssuer{}, Inference: &changingInference{mode: "denied"}, Authority: &stubAuthority{}, Workloads: workload, Status: stubStatus{}}
	if _, err := a.Start(context.Background(), safeTask()); err == nil {
		t.Fatal("inference denial admitted workload")
	}
	if workload.writes != 0 || workload.runs != 0 {
		t.Fatal("projection preceded inference authorization")
	}
}
func TestInferenceOutageAndChangedGrantStopAttempt(t *testing.T) {
	for _, mode := range []string{"outage", "changed"} {
		t.Run(mode, func(t *testing.T) {
			workload := &stubWorkload{}
			authority := &stubAuthority{}
			a := Adapter{Issuer: &stubIssuer{}, Inference: &changingInference{mode: mode}, Authority: authority, Workloads: workload, Status: stubStatus{}}
			run, err := a.Start(context.Background(), safeTask())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			err = run.Wait(ctx)
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("inference change did not stop attempt", err)
			}
			if err = run.Remove(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !workload.removed || authority.actions[len(authority.actions)-1] != "revoke" {
				t.Fatal("inference failure retained attempt")
			}
		})
	}
}
