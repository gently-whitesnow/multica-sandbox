package service

import (
	"context"
	"errors"
	"fmt"
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
		if inaccessible(err) {
			continue
		}
		if err != nil {
			return err
		}
		// Never recover while a cancelled attempt from this workspace is still cleaning up.
		if old := f.known[ws.ID]; f.active[old] != nil {
			continue
		}
		f.known[ws.ID] = rt.ID
		if err := writeRegistry(f.dir, f.known); err != nil {
			return err
		}
		recovered, err := f.api.Recover(ctx, rt.ID)
		if inaccessible(err) {
			continue
		}
		if err != nil {
			return err
		}
		f.ready[ws.ID] = rt.ID
		fmt.Fprintf(f.out, "registered workspace=%s runtime=%s orphaned=%d\n", ws.ID, rt.ID, recovered.Orphaned)
	}
	return nil
}

func (f *fleet) remove(ws, rt string) {
	delete(f.ready, ws)
	delete(f.served, rt)
	if cancel := f.active[rt]; cancel != nil {
		cancel()
	}
	fmt.Fprintf(f.out, "removed workspace=%s runtime=%s\n", ws, rt)
}

func (f *fleet) beat(ctx context.Context) error {
	for ws, rt := range f.ready {
		err := f.api.Heartbeat(ctx, rt)
		if inaccessible(err) {
			f.remove(ws, rt)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
