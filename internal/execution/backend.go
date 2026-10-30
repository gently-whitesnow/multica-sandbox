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
	Telemetry                                    Telemetry
}

// Telemetry describes an attempt for controller logs; it never reaches Multica.
// PeakMemory is in bytes, 0 when not measured.
type Telemetry struct {
	Model, Effort                           string
	InputTokens, OutputTokens, CachedTokens int64
	PeakMemory                              int64
}

// Workdir asks for the retained workdir of an issue or chat conversation (ADR 0015,
// 0017); the zero value is a per-attempt workdir. Prior is the claim's prior_work_dir.
type Workdir struct {
	Workspace, Agent, Issue, Chat, Prior string
}

// Conversation keys the retained workdir: the chat session, else the issue.
func (w Workdir) Conversation() string {
	if w.Chat != "" {
		return "chat:" + w.Chat
	}
	return w.Issue
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
	// WriteTree lays down files (relative path to content) under an approved empty root.
	WriteTree(context.Context, string, map[string][]byte) error
	Execute(context.Context, []string) error
	// Stream runs the agent with stdin and stdout connected to the adapter; env passes
	// through the exec client environment, never through argv. Stdin is closed once the
	// callback returns; the agent then has a short grace period to exit.
	Stream(context.Context, []string, map[string]string, func(io.Writer, io.Reader) error) error
	// Capture runs a controller-owned command as the attempt user and returns its bounded stdout.
	Capture(context.Context, []string, map[string]string) ([]byte, error)
	// Workdir names the retained workdir, empty when none, and whether it already existed.
	Workdir() (string, bool)
}

// RejectedError is safe to report as a task failure: no execution resources remain.
type RejectedError struct{ Err error }

func (e *RejectedError) Error() string { return e.Err.Error() }
func (e *RejectedError) Unwrap() error { return e.Err }
