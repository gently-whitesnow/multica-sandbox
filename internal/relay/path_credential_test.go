package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type pathErrorTransport struct{}

func (pathErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: errors.New("transport included private-path-secret")}
}

func TestPathCredentialTransportFailureNeverRecordsSecretURL(t *testing.T) {
	lease := Lease{Origin: &url.URL{Scheme: "https", Host: "messages.example"}, Path: "/botprivate-path-secret/send", WithholdErrors: true, Context: context.Background(), Release: func() {}}
	var recorded string
	var outcome error
	proxy, err := New(fixedLease{lease}, Policy{Allow: func(string) bool { return true }, Limit: 1024,
		Record: func(req *http.Request, _ string, _ int, err error, _ time.Duration) {
			recorded, outcome = req.URL.String(), err
		}})
	if err != nil {
		t.Fatal(err)
	}
	proxy.proxy.Transport = pathErrorTransport{}
	request := httptest.NewRequest("POST", "/http/messenger/send", nil)
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	response := recorder.Result()
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 502 || strings.Contains(recorded+outcome.Error()+string(body), "private-path-secret") || !errors.Is(outcome, ErrUpstream) || recorded != "/http/messenger/send" {
		t.Fatal("transport failure exposed a secret-bearing URL")
	}
}
