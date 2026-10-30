package opencode

import (
	"context"
	"encoding/json"
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
	env    identity.Env
}

func (s *stubIssuer) AcquireForMCP(_ context.Context, _ identity.Ref, env identity.Env, _ string) (identity.AccessToken, error) {
	s.calls++
	s.env = env
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
	trees   int
	removed bool
	runs    int
	files   map[string][]byte
	envs    chan map[string]string
	// workdir is the requested retained workdir; retained and reused are the answer.
	workdir  execution.Workdir
	retained string
	reused   bool
	// agent answers the adapter's ACP turn; nil waits for cancellation.
	agent *fakeAgent
	// capture answers Capture, which records its argv in captured.
	capture  []byte
	captured []string
	// refuse fails Start like a backend that created nothing.
	refuse error
}

func (s *stubWorkload) Start(_ context.Context, _ string, w execution.Workdir) (execution.ProjectedRun, error) {
	if s.refuse != nil {
		return nil, s.refuse
	}
	s.workdir = w
	return s, nil
}
func (s *stubWorkload) Workdir() (string, bool) { return s.retained, s.reused }
func (s *stubWorkload) Write(_ context.Context, path string, data []byte) error {
	s.writes++
	if s.files != nil {
		s.files[path] = data
	}
	return nil
}
func (s *stubWorkload) WriteTree(_ context.Context, root string, files map[string][]byte) error {
	for name, data := range files {
		if s.files != nil {
			s.files[root+"/"+name] = data
		}
	}
	s.trees++
	return nil
}
func (s *stubWorkload) Execute(context.Context, []string) error {
	s.runs++
	return nil
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
			a := Adapter{Server: "https://multica.example.invalid", Controller: "controller", Issuer: issuer, MCPRelay: NewMCPRelay(), Authority: authority, Workloads: workload, Status: stubStatus{}}
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
	a := Adapter{Issuer: &stubIssuer{}, MCPRelay: NewMCPRelay(), Authority: authority, Workloads: workload, Status: stubStatus{}}
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
	a := Adapter{Issuer: &stubIssuer{}, MCPRelay: NewMCPRelay(), Authority: authority, Workloads: workload, Status: stubStatus{}}
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

func TestBackendRefusalRejectsOnlyTheTask(t *testing.T) {
	refused := &execution.RejectedError{Err: errors.New("Docker address pools are exhausted")}
	authority := &stubAuthority{}
	a := Adapter{Issuer: &stubIssuer{}, MCPRelay: NewMCPRelay(), Authority: authority, Workloads: &stubWorkload{refuse: refused}, Status: stubStatus{}}
	_, err := a.Start(context.Background(), safeTask())
	var rejected *execution.RejectedError
	if !errors.As(err, &rejected) || !strings.Contains(err.Error(), "address pools") || authority.actions[len(authority.actions)-1] != "revoke" {
		t.Fatalf("backend refusal not a clean rejection: %v", err)
	}
	authority.denyRevoke = true
	if _, err = a.Start(context.Background(), safeTask()); errors.As(err, &rejected) {
		t.Fatal("refusal with uncertain revocation reported as safe")
	}
}

func TestUncertainRevocationIsNotSafeRejection(t *testing.T) {
	a := Adapter{Issuer: &stubIssuer{}, MCPRelay: NewMCPRelay(), Authority: &stubAuthority{deny: true, denyRevoke: true}, Workloads: &stubWorkload{}, Status: stubStatus{}}
	_, err := a.Start(context.Background(), safeTask())
	var rejected *execution.RejectedError
	if err == nil || errors.As(err, &rejected) {
		t.Fatal("uncertain authorization cleanup treated as safe rejection")
	}
}

// loaded reports whether the agent was asked to continue a session.
func (s *stubWorkload) loaded() bool {
	_, ok := s.agent.called("session/load")
	return ok
}

// Stream runs the fake agent; envs receives the environment once the prompt arrived.
func (s *stubWorkload) Stream(ctx context.Context, _ []string, env map[string]string, converse func(io.Writer, io.Reader) error) error {
	if s.agent == nil {
		s.agent = &fakeAgent{version: "2.2.2"}
	}
	agent := s.agent
	prompted := make(chan string, 1)
	agent.prompted = prompted
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	served := make(chan struct{})
	go func() { defer close(served); agent.serve(outW, inR) }()
	go func() {
		select {
		case prompt := <-prompted:
			if s.files != nil {
				s.files[sentPrompt] = []byte(prompt)
			}
			if s.envs != nil {
				s.envs <- env
			}
		case <-served:
		}
	}()
	err := converse(inW, outR)
	_ = inW.Close()
	<-served
	return err
}

// sentPrompt keys the prompt the fake agent received in stubWorkload.files.
const sentPrompt = "acp:session/prompt"

func (s *stubWorkload) Capture(_ context.Context, args []string, _ map[string]string) ([]byte, error) {
	s.captured = args
	return s.capture, nil
}
func (s *stubWorkload) Result() execution.Result { return execution.Result{} }

func TestWithoutIdentityOnlyAgentsWithoutMCPRun(t *testing.T) {
	workload := &stubWorkload{}
	a := Adapter{Workloads: workload, Status: stubStatus{}}
	var rejected *execution.RejectedError
	if _, err := a.Start(context.Background(), safeTask()); !errors.As(err, &rejected) || workload.writes != 0 || workload.runs != 0 {
		t.Fatalf("MCP selection without identity not refused: %v", err)
	}
	task := safeTask()
	task.Agent.MCPConfig = nil
	run, err := a.Start(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Remove(context.Background()); err != nil || !workload.removed {
		t.Fatalf("cleanup without authority: %v", err)
	}
}

func TestIssuerReceivesClaimAgentEnv(t *testing.T) {
	issuer := &stubIssuer{}
	a := Adapter{Server: "https://multica.example.invalid", Controller: "controller", Issuer: issuer, MCPRelay: NewMCPRelay(), Authority: &stubAuthority{}, Workloads: &stubWorkload{}, Status: stubStatus{}}
	task := safeTask()
	if err := json.Unmarshal([]byte(`{"IDP_IAM_CLIENT_ID":"client-env","IDP_IAM_CLIENT_SECRET":"env-secret"}`), &task.Agent.CustomEnv); err != nil {
		t.Fatal(err)
	}
	run, err := a.Start(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Remove(context.Background())
	if issuer.env == nil || issuer.env.Allowed([]string{identity.EnvClientID})[identity.EnvClientID] != "client-env" {
		t.Fatal("issuer did not receive the claim's agent env")
	}
}

func TestMCPWithoutAuthority(t *testing.T) {
	issuer, workload := &stubIssuer{}, &stubWorkload{}
	a := Adapter{Server: "https://multica.example.invalid", Controller: "controller", Issuer: issuer, MCPRelay: NewMCPRelay(), Workloads: workload, Status: stubStatus{}}
	run, err := a.Start(context.Background(), safeTask())
	if err != nil || issuer.calls == 0 {
		t.Fatalf("MCP token not issued without authority: %v", err)
	}
	if err = run.Remove(context.Background()); err != nil || !workload.removed {
		t.Fatal("cleanup without authority failed", err)
	}
	a.Issuer = nil
	if _, err = a.Start(context.Background(), safeTask()); err == nil {
		t.Fatal("MCP without identity accepted")
	}
}
