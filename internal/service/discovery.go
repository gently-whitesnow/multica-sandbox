package service

import (
	"context"
	"errors"
	"sort"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func inaccessible(err error) bool {
	var status *multica.HTTPError
	return errors.As(err, &status) && (status.Status == 403 || status.Status == 404)
}

func (f *fleet) sync(ctx context.Context) error {
	workspaces, err := f.api.Workspaces(ctx)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, ws := range workspaces {
		allowed[ws.ID] = true
	}
	for ws, rt := range f.ready {
		if !allowed[ws] {
			f.remove(ws, rt)
		}
	}
	sort.Slice(workspaces, func(i, j int) bool { return workspaces[i].ID < workspaces[j].ID })
	for _, ws := range workspaces {
		if f.ready[ws.ID] != "" {
			continue
		}
		rt, err := f.api.Register(ctx, ws.ID, f.daemon)
		if inaccessible(err) || multica.Transient(err) {
			continue
		}
		if err != nil {
			return err
		}
		// Never recover while a cancelled attempt from this workspace is still cleaning up.
		if old := f.known[ws.ID]; f.perRuntime[old] > 0 {
			continue
		}
		f.known[ws.ID] = rt.ID
		if err := writeRegistry(f.dir, f.known); err != nil {
			return err
		}
		// A transient recovery failure leaves the workspace unready until the next sync.
		recovered, err := f.api.Recover(ctx, rt.ID)
		if inaccessible(err) || multica.Transient(err) {
			continue
		}
		if err != nil {
			return err
		}
		f.ready[ws.ID] = rt.ID
		f.log.Info("workspace registered", "workspace", ws.ID, "runtime", rt.ID, "orphaned", recovered.Orphaned)
	}
	return nil
}

func (f *fleet) remove(ws, rt string) {
	delete(f.ready, ws)
	delete(f.served, rt)
	// Every attempt of the runtime ends; slots are released as they complete.
	for _, attempt := range f.active {
		if attempt.runtime == rt {
			attempt.cancel()
		}
	}
	f.log.Info("workspace removed", "workspace", ws, "runtime", rt)
}

// beat is the only heartbeat of fleet runtimes and answers their pending requests;
// attempts do not heartbeat (controller.Probe.Execute).
func (f *fleet) beat(ctx context.Context) error {
	for ws, rt := range f.ready {
		if err := f.beatRuntime(ctx, ws, rt); err != nil && !multica.Transient(err) {
			return err
		}
	}
	return nil
}

// beatRuntime heartbeats one runtime; a revoked runtime is removed.
func (f *fleet) beatRuntime(ctx context.Context, ws, rt string) error {
	err := f.api.Heartbeat(ctx, rt)
	var unanswered *multica.UnansweredError
	switch {
	case errors.As(err, &unanswered):
		f.log.Warn("runtime request unanswered", "runtime", rt, "error", err.Error())
		return nil
	case inaccessible(err):
		f.remove(ws, rt)
		return nil
	}
	return err
}
