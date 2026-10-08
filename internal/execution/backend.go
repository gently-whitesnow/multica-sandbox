package execution

import (
	"context"
	"io"
)

// Result contains retained reporting data, never execution credentials or paths.
// WorkDir names a retained workdir; RetiredSessionID a prior session this run abandoned.
type Result struct {
	Output, SessionID, WorkDir, RetiredSessionID string
	Disposable                                   bool
}

// Workdir asks for the retained workdir of an issue conversation (ADR 0015); the zero
// value is a per-attempt workdir. Prior is the claim's prior_work_dir.
type Workdir struct {
	Workspace, Agent, Issue, Prior string
}

// Backend owns only execution resources, never task scheduling or retries.
type Backend interface {
	Reconcile(context.Context) error
	Start(context.Context, string) (Run, error)
}

type Run interface {
	Wait(context.Context) error
	Remove(context.Context) error
	Result() Result
}

// ProjectedRun accepts bounded adapter-owned files without host directory mounts.
type ProjectedRun interface {
	Run
	Write(context.Context, string, []byte) error
	Execute(context.Context, []string) error
	// Stream passes env through the exec client environment, never through argv.
	Stream(context.Context, []string, map[string]string, func(io.Reader) error) error
	// Capture runs a controller-owned command as the attempt user and returns its bounded stdout.
	Capture(context.Context, []string, map[string]string) ([]byte, error)
	// Workdir names the retained workdir, empty when none, and whether it already existed.
	Workdir() (string, bool)
}

// RejectedError is safe to report as a task failure: no execution resources remain.
type RejectedError struct{ Err error }

func (e *RejectedError) Error() string { return e.Err.Error() }
func (e *RejectedError) Unwrap() error { return e.Err }
