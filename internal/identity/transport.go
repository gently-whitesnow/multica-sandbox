package identity

import (
	"bytes"
	"io"
	"net/http"
	"time"
)

// Each client can contact only operator-configured endpoints. No redirects or proxy ENV.
type transport struct {
	base    http.RoundTripper
	allowed map[string]bool
}

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !t.allowed[r.URL.String()] {
		return nil, ErrDenied
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, ErrDenied
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDocument+1))
	if err != nil || len(body) > maxDocument {
		return nil, ErrDenied
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}
func httpClient(addresses ...string) *http.Client {
	allowed := map[string]bool{}
	for _, address := range addresses {
		allowed[address] = true
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	return &http.Client{Timeout: 10 * time.Second, Transport: transport{base, allowed}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
