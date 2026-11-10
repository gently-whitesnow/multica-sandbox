package opencode

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/gently-whitesnow/multica-sandbox/internal/repo"
)

// markClean is deliberately conservative. Native jobs have no complete public
// inspection API; an observed background or foreign execution forbids DB reuse.
func (h *httpTurn) markClean(ctx context.Context) {
	h.mu.Lock()
	unsafe := h.unsafeResume
	owned := make([]string, 0, len(h.owned))
	for session := range h.owned {
		owned = append(owned, session)
	}
	h.mu.Unlock()
	if unsafe {
		return
	}
	var active struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if h.r.httpAPI(ctx, "GET", "/api/session/active", nil, &active) != nil || active.Data == nil || len(active.Data) != 0 {
		return
	}
	for _, session := range owned {
		var inbox struct {
			Data []json.RawMessage `json:"data"`
		}
		if h.r.httpAPI(ctx, "GET", "/api/session/"+session+"/inbox", nil, &inbox) != nil || inbox.Data == nil || len(inbox.Data) != 0 {
			return
		}
	}
	var shells struct {
		Data []json.RawMessage `json:"data"`
	}
	if h.r.httpAPI(ctx, "GET", "/api/shell?directory="+url.QueryEscape(repo.WorkDir), nil, &shells) != nil || shells.Data == nil || len(shells.Data) != 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.unsafeResume && !h.busy && h.terminal && len(h.shells) == 0 {
		h.r.nativeClean = true
	}
}
