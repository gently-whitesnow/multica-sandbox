package execution

import "context"

// Backend owns only execution resources, never task scheduling or retries.
type Backend interface {
	Reconcile(context.Context) error
	Start(context.Context, string) (Run, error)
}

type Run interface {
	Wait(context.Context) error
	Remove(context.Context) error
}

// ProjectedRun accepts bounded adapter-owned files without host directory mounts.
type ProjectedRun interface {
	Run
	Write(context.Context, string, []byte) error
	Execute(context.Context, []string) error
}

// RejectedError is safe to report as a task failure: no execution resources remain.
type RejectedError struct{ Err error }

func (e *RejectedError) Error() string { return e.Err.Error() }
func (e *RejectedError) Unwrap() error { return e.Err }
