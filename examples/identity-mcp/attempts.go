package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/attempt"
)

func (g *registry) attempts(secret []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+string(secret))) != 1 {
			w.WriteHeader(403)
			return
		}
		var request attempt.Grant
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		d.DisallowUnknownFields()
		if d.Decode(&request) != nil || request.Version != 1 || request.Controller == "" {
			w.WriteHeader(400)
			return
		}
		g.Lock()
		defer g.Unlock()
		switch request.Action {
		case "revoke", "recover":
			if request.Action == "revoke" {
				g.ended[request.Attempt] = true
			}
			for hash, run := range g.grants {
				if run.Controller == request.Controller && (request.Action == "recover" || run.Attempt == request.Attempt) {
					run.Active = false
					g.grants[hash] = run
					g.ended[run.Attempt] = true
				}
			}
		case "renew":
			existing, exists := g.grants[request.TokenHash]
			if g.ended[request.Attempt] || (request.URL != "http://gateway:8080/mcp" && request.URL != "http://gateway:8080/redirect") || len(request.TokenHash) != 64 || request.Agent != "20000000-0000-4000-8000-000000000001" || request.Attempt == "" || request.Task == "" || request.ExpiresAt <= time.Now().Unix() || request.ExpiresAt > time.Now().Add(15*time.Second).Unix() || (exists && existing.Attempt != request.Attempt) {
				w.WriteHeader(403)
				return
			}
			g.grants[request.TokenHash] = grant{ID: request.Attempt, Controller: request.Controller, Workspace: request.Workspace, Agent: "agent-1", Task: request.Task, Attempt: request.Attempt, Resource: "document", TokenHash: request.TokenHash, Active: true, Until: request.ExpiresAt}
		default:
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(204)
	})
}

func (g *registry) evidence(secret []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+string(secret))) != 1 {
			w.WriteHeader(403)
			return
		}
		g.RLock()
		defer g.RUnlock()
		out := map[string][2]int{"redirects": {g.redirects, g.captures}}
		for key, calls := range g.calls {
			out[key] = [2]int{calls, len(g.hashes[key])}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}
