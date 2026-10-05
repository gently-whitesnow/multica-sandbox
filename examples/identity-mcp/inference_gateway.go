package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const inferenceSubject = "30000000-0000-4000-8000-000000000002"

func (g *registry) inferenceAuth(v *oidc.IDTokenVerifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.Lock()
		g.inferenceRequests++
		g.Unlock()
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parsed, err := v.Verify(r.Context(), bearer)
		if err != nil {
			w.WriteHeader(403)
			return
		}
		var claims struct {
			Client string `json:"azp"`
			Type   string `json:"typ"`
		}
		if parsed.Claims(&claims) != nil || claims.Client != "example-inference" || claims.Type != "Bearer" || parsed.Subject != inferenceSubject {
			w.WriteHeader(403)
			return
		}
		hash := fingerprint(bearer)
		g.Lock()
		defer g.Unlock()
		grant, ok := g.grants[hash]
		if !ok || !grant.Active || grant.Until <= time.Now().Unix() || grant.URL != "http://litellm:4000/v1" {
			w.WriteHeader(403)
			return
		}
		var selection struct {
			Model  string `json:"model"`
			Effort string `json:"reasoning_effort"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&selection) != nil {
			w.WriteHeader(400)
			return
		}
		g.calls["selection:"+grant.Attempt+"|"+selection.Model+"|"+selection.Effort]++
		key := "inference:" + grant.Attempt
		g.calls[key]++
		if g.hashes[key] == nil {
			g.hashes[key] = map[string]bool{}
		}
		g.hashes[key][hash] = true
		_ = json.NewEncoder(w).Encode(map[string]any{"workspace": grant.Workspace, "agent": grant.Agent, "models": []string{"fixture", "fixture-new", "fixture-limited"}})
	})
}
func fixtureInference(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("INFERENCE_FIXTURE") == "1" {
		key, err := os.ReadFile("/secrets/upstream")
		if err != nil || r.Header.Get("Authorization") != "Bearer "+string(key) {
			w.WriteHeader(403)
			return
		}
	}
	mockInference(w, r)
}
