package opencode

import (
	"context"
	"strings"
	"time"
)

// unattended requests receive the same rejection as ACP; another session's
// permissions and forms never belong to this attempt's event consumer.
func (h *httpTurn) unattended(ctx context.Context, event httpEvent) {
	id, session, method, path := event.Data.ID, event.Data.SessionID, "POST", ""
	var body any
	switch event.Type {
	case "permission.asked":
		path = "/permission/" + id + "/reply"
		body = map[string]string{"decision": "reject", "message": "This unattended run cannot ask for permission; continue without that action."}
	case "form.created":
		id, session = event.Data.Form.ID, event.Data.Form.SessionID
		method, path = "DELETE", "/form/"+id
	default:
		return
	}
	if id == "" || strings.ContainsAny(id, "/?#\x00\r\n") {
		return
	}
	h.mu.Lock()
	owned := h.owned[session]
	h.mu.Unlock()
	if !owned {
		return
	}
	reply, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = h.r.httpAPI(reply, method, "/api/session/"+session+path, body, nil)
}
