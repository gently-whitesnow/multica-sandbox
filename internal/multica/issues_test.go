package multica

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func issueID(n int) string { return fmt.Sprintf("40000000-0000-4000-8000-%012d", n) }

func TestClosedIssuesFollowUpstreamGCCheck(t *testing.T) {
	updated := "2026-10-01T00:00:00Z"
	answers := map[string]string{
		issueID(1): `"found":true,"status":"done","category":"done"`,
		issueID(2): `"found":true,"status":"cancelled","category":"closed"`,
		issueID(3): `"found":true,"status":"cancelled"`, // server predating categories
		issueID(4): `"found":true,"status":"in_review","category":"started"`,
		issueID(5): `"found":true,"status":"done","category":"started"`,
		issueID(6): `"found":true,"status":"shipped"`, // custom key without category
		issueID(7): `"found":false`,
		issueID(8): `"found":true,"status":"done","category":"done","updated_at":null`,
	}
	batches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []string `json:"issue_ids"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/daemon/workspaces/"+testID+"/issues/gc-check" || json.NewDecoder(r.Body).Decode(&body) != nil || len(body.IDs) > issueGCBatch {
			t.Errorf("unexpected request %s %s %d", r.Method, r.URL.Path, len(body.IDs))
		}
		batches++
		items := []string{fmt.Sprintf(`{"id":%q,"found":true,"status":"done","category":"done","updated_at":%q}`, issueID(999999), updated)}
		for _, id := range body.IDs {
			answer, ok := answers[id]
			if !ok {
				answer = `"found":true,"status":"todo","category":"unstarted"`
			}
			if !strings.Contains(answer, "updated_at") {
				answer += `,"updated_at":"` + updated + `"`
			}
			items = append(items, fmt.Sprintf(`{"id":%q,%s}`, id, answer))
		}
		fmt.Fprintf(w, `{"issues":[%s]}`, strings.Join(items, ","))
	}))
	defer server.Close()
	api, _ := New(server.URL, "fixture")
	ids := []string{issueID(1), issueID(1)}
	for n := 2; n <= 600; n++ {
		ids = append(ids, issueID(n))
	}
	closed, err := api.ClosedIssues(context.Background(), testID, ids)
	if err != nil || batches != 2 || len(closed) != 3 || closed[issueID(1)].Format("2006-01-02") != "2026-10-01" {
		t.Fatalf("closed=%v batches=%d err=%v", closed, batches, err)
	}
	for _, id := range []string{issueID(1), issueID(2), issueID(3)} {
		if _, ok := closed[id]; !ok {
			t.Errorf("%s not collected", id)
		}
	}
}

func TestClosedIssuesFailSafe(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"issues":[{"id":%q,"found":true,"category":"done","updated_at":"2026-10-01T00:00:00Z"}]}`, issueID(501))
	}))
	defer server.Close()
	api, _ := New(server.URL, "fixture")
	ids := []string{}
	for n := 1; n <= 501; n++ {
		ids = append(ids, issueID(n))
	}
	// A failed batch keeps its issues; later batches still answer.
	closed, err := api.ClosedIssues(context.Background(), testID, ids)
	if err == nil || len(closed) != 1 || closed[issueID(501)].IsZero() {
		t.Fatalf("closed=%v err=%v", closed, err)
	}
	for _, bad := range [][2]string{{"../x", issueID(1)}, {testID, "../x"}} {
		if _, err := api.ClosedIssues(context.Background(), bad[0], []string{bad[1]}); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if calls != 2 {
		t.Fatalf("invalid IDs reached Multica: %d", calls)
	}
}
