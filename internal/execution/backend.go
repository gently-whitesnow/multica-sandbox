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
