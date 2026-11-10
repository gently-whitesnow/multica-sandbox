// Package faultproxy is a test fixture: a reverse proxy to a disposable Multica
// server that loses, holds or refuses selected requests on demand, so tests see
// the same ambiguous outcomes a real network gives.
package faultproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"sync"
)

// Control is the proxy's own path; everything else is forwarded.
const Control = "/__faults"

const (
	// Lose forwards the request, then closes the connection without an answer:
	// the server committed, the client cannot know.
	Lose = "lose"
	// Hold keeps the request unforwarded until the rules are cleared, then
	// answers 503: the server never saw it.
	Hold = "hold"
	// Refuse answers the rule's status without forwarding.
	Refuse = "refuse"
	// Count only counts and forwards.
	Count = "count"
)

type rule struct {
	Action string `json:"action"`
	Match  string `json:"match"`
	Left   int    `json:"left"`
	Status int    `json:"status,omitempty"`
	Hits   int    `json:"hits"`
	// Held counts requests currently blocked by a Hold rule.
	Held    int `json:"held"`
	pattern *regexp.Regexp
	release chan struct{}
}

type proxy struct {
	mu      sync.Mutex
	rules   []*rule
	forward *httputil.ReverseProxy
}

// Handler proxies to upstream. POST Control?action=&match=&count=&status= arms a
// rule for count POST requests whose path matches the regular expression; GET
// lists rules with their hits; DELETE clears them and releases held requests.
func Handler(upstream *url.URL) http.Handler {
	return &proxy{forward: httputil.NewSingleHostReverseProxy(upstream)}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == Control {
		p.control(w, r)
		return
	}
	matched := p.match(r)
	if matched == nil {
		p.forward.ServeHTTP(w, r)
		return
	}
	switch matched.Action {
	case Lose:
		p.forward.ServeHTTP(httptest.NewRecorder(), r)
		drop(w)
	case Hold:
		select {
		case <-matched.release:
		case <-r.Context().Done():
		}
		p.mu.Lock()
		matched.Held--
		p.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	case Refuse:
		w.WriteHeader(matched.Status)
	default:
		p.forward.ServeHTTP(w, r)
	}
}

func drop(w http.ResponseWriter) {
	if hijacker, ok := w.(http.Hijacker); ok {
		if conn, _, err := hijacker.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

func (p *proxy) match(r *http.Request) *rule {
	if r.Method != http.MethodPost {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, candidate := range p.rules {
		if candidate.Left == 0 || !candidate.pattern.MatchString(r.URL.Path) {
			continue
		}
		candidate.Hits++
		if candidate.Action != Count {
			candidate.Left--
		}
		if candidate.Action == Hold {
			candidate.Held++
		}
		return candidate
	}
	return nil
}

func (p *proxy) control(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch r.Method {
	case http.MethodPost:
		q := r.URL.Query()
		pattern, err := regexp.Compile(q.Get("match"))
		count, countErr := strconv.Atoi(q.Get("count"))
		status, _ := strconv.Atoi(q.Get("status"))
		action := q.Get("action")
		known := action == Lose || action == Hold || action == Count || action == Refuse && status >= 400 && status <= 599
		if err != nil || countErr != nil || count < 1 || !known || q.Get("match") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.rules = append(p.rules, &rule{Action: action, Match: q.Get("match"), Left: count, Status: status, pattern: pattern, release: make(chan struct{})})
	case http.MethodDelete:
		for _, armed := range p.rules {
			close(armed.release)
		}
		p.rules = nil
	case http.MethodGet:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(p.rules)
}
