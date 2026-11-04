package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
	"github.com/gently-whitesnow/multica-sandbox/internal/identity"
	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

type stubIssuer struct {
	calls  int
	denyAt int
	env    identity.Env
	// ttl is the issued lifetime, 11 s by default.
	ttl time.Duration
}

func (s *stubIssuer) AcquireForMCP(_ context.Context, _ identity.Ref, env identity.Env, _ string) (identity.AccessToken, error) {
	s.calls++
	s.env = env
	if s.calls == s.denyAt {
		return identity.AccessToken{}, identity.ErrDenied
	}
	ttl := s.ttl
	if ttl == 0 {
		ttl = 11 * time.Second
	}
	// Like a real issuer, every token is distinct.
	return identity.NewAccessToken(fmt.Sprintf("jwt-%d", s.calls), time.Now().Add(ttl)), nil
}

type stubAuthority struct {
	sync.Mutex
	actions    []string
	deny       bool
	denyRevoke bool
}

func (s *stubAuthority) renewals() int {
	s.Lock()
	defer s.Unlock()
	n := 0
	for _, action := range s.actions {
		if action == "renew" {
			n++
		}
	}
	return n
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

// scriptedStatus answers status polls with state, or err when set, and counts them.
type scriptedStatus struct {
	sync.Mutex
	state string
	err   error
	calls int
}

func (s *scriptedStatus) Status(context.Context, string) (string, error) {
	s.Lock()
	defer s.Unlock()
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	return s.state, nil
}
func (s *scriptedStatus) set(state string, err error) {
	s.Lock()
	defer s.Unlock()
	s.state, s.err = state, err
}
func (s *scriptedStatus) polls() int { s.Lock(); defer s.Unlock(); return s.calls }

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
	// capture answers bounded Capture and streamed exports; captured records their argv.
	capture  []byte
	captured []string
	// api answers `opencode api` by request path.
	api func(path string) string
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
func TestStatusGateRidesOutTransientErrorsWithinLease(t *testing.T) {
	unavailable, denied := &multica.HTTPError{Status: 503}, &multica.HTTPError{Status: 403}
	for _, c := range []struct {
		name      string
		confirmed time.Duration
		state     string
		err       error
		ok        bool
	}{
		{"running", -time.Hour, "running", nil, true},
		{"transient", -time.Second, "", unavailable, true},
		{"transient past lease", -attemptLease - time.Second, "", unavailable, false},
		{"transient unconfirmed", 0, "", unavailable, false},
		{"denied", -time.Second, "", denied, false},
		{"missing", -time.Second, "", &multica.HTTPError{Status: 404}, false},
		{"cancelled", -time.Second, "cancelled", nil, false},
	} {
		r := &running{adapter: &Adapter{Status: &scriptedStatus{state: c.state, err: c.err}}, task: safeTask()}
		if c.confirmed != 0 {
			r.confirmed = time.Now().Add(c.confirmed)
		}
		if err := r.confirm(context.Background()); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	status := &scriptedStatus{state: "running"}
	a := Adapter{Issuer: &stubIssuer{}, MCPRelay: NewMCPRelay(), Authority: &stubAuthority{}, Workloads: &stubWorkload{}, Status: status}
	run, err := a.Start(context.Background(), safeTask())
	if err != nil {
		t.Fatal(err)
	}
	defer run.Remove(context.Background())
	status.set("", unavailable)
	short, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	if err := run.Wait(short); !errors.Is(err, context.DeadlineExceeded) || status.polls() < 3 {
		t.Fatalf("transient status error stopped the attempt: %v after %d polls", err, status.polls())
	}
	status.set("", denied)
	wait, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := run.Wait(wait); !errors.Is(err, ErrDenied) {
		t.Fatalf("denied status did not stop the attempt: %v", err)
	}
}

// Once the controller feeds the status, the adapter stops polling and follows it.
func TestFedStatusReplacesThePoll(t *testing.T) {
	status := &scriptedStatus{state: "running"}
	a := Adapter{Issuer: &stubIssuer{}, MCPRelay: NewMCPRelay(), Authority: &stubAuthority{}, Workloads: &stubWorkload{}, Status: status}
	run, err := a.Start(context.Background(), safeTask())
	if err != nil {
		t.Fatal(err)
	}
	defer run.Remove(context.Background())
	fed := run.(interface{ TaskStatus(string, error) })
	for range 25 {
		fed.TaskStatus("running", nil)
		time.Sleep(100 * time.Millisecond)
	}
	if polls := status.polls(); polls != 1 {
		t.Fatalf("adapter polled %d times beside the controller", polls)
	}
	fed.TaskStatus("cancelled", nil)
	wait, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := run.Wait(wait); !errors.Is(err, ErrDenied) {
		t.Fatalf("fed cancellation did not stop renewal: %v", err)
	}
}

// The authority holds a lease until half of it remains or the token changes.
func TestAuthorityRenewsOnlyNearLeaseEndOrOnChange(t *testing.T) {
	authority := &stubAuthority{}
	a := Adapter{Issuer: &stubIssuer{ttl: time.Hour}, Authority: authority, Status: stubStatus{}}
	connections, err := Select(safeTask())
	if err != nil {
		t.Fatal(err)
	}
	r := &running{adapter: &a, task: safeTask(), connections: connections, tokens: map[string]identity.AccessToken{}, leases: map[string]lease{}}
	step := func(want int) {
		t.Helper()
		if err := r.refresh(context.Background()); err != nil || authority.renewals() != want {
			t.Fatalf("renewals=%d want %d: %v", authority.renewals(), want, err)
		}
	}
	step(1)
	step(1)
	r.leases["selected"] = lease{r.leases["selected"].hash, time.Now().Add(attemptLease/2 - time.Second)}
	step(2)
	r.tokens["selected"] = identity.NewAccessToken("rotated", time.Now().Add(time.Hour))
	step(3)
	if r.leases["selected"].hash != attempt.Fingerprint("rotated") {
		t.Fatal("rotated token not registered")
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
func (s *stubWorkload) Stream(ctx context.Context, args []string, env map[string]string, converse func(io.Writer, io.Reader) error) error {
	if len(args) > 1 && args[0] == "opencode" && args[1] == "export" {
		s.captured = args
		return converse(io.Discard, strings.NewReader(string(s.capture)))
	}
	if len(args) > 1 && args[0] == "opencode" && args[1] == "api" {
		return converse(io.Discard, strings.NewReader(s.api(args[len(args)-1])))
	}
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
	return s.capture[:min(len(s.capture), 65536)], nil
}
func (s *stubWorkload) Result() execution.Result { return execution.Result{} }

func TestWithoutRelayOnlyAgentsWithoutMCPRun(t *testing.T) {
	workload := &stubWorkload{}
	a := Adapter{Workloads: workload, Status: stubStatus{}}
	var rejected *execution.RejectedError
	if _, err := a.Start(context.Background(), safeTask()); !errors.As(err, &rejected) || workload.writes != 0 || workload.runs != 0 {
		t.Fatalf("MCP selection without relay not refused: %v", err)
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
	if err := json.Unmarshal([]byte(`{"IAM_CLIENT_ID":"client-env","IAM_CLIENT_SECRET":"env-secret"}`), &task.Agent.CustomEnv); err != nil {
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

func TestAnonymousMCPIssuesNoToken(t *testing.T) {
	issuer, workload, authority := &stubIssuer{}, &stubWorkload{}, &stubAuthority{}
	a := Adapter{Server: "https://multica.example.invalid", Controller: "controller", Issuer: issuer, Authority: authority, MCPRelay: NewMCPRelay(), Workloads: workload, Status: stubStatus{}}
	task := safeTask()
	task.Agent.MCPConfig = json.RawMessage(`{"mcpServers":{"selected":{"url":"https://tools.example.invalid/mcp"}}}`)
	run, err := a.Start(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Remove(context.Background())
	authority.Lock()
	defer authority.Unlock()
	if issuer.calls != 0 || slices.Contains(authority.actions, "renew") {
		t.Fatalf("anonymous MCP issued %d tokens, authority %v", issuer.calls, authority.actions)
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
