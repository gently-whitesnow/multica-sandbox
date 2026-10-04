package multica

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBatchRejectsCrossWorkspaceAndOverclaim(t *testing.T) {
	other := "22222222-2222-4222-8222-222222222222"
	valid := fmt.Sprintf(`{"id":%q,"runtime_id":%q,"workspace_id":%q,"dispatched_at":"2026-10-04T00:00:00Z","start_claim_supported":true}`, testID, testID, testID)
	cross := fmt.Sprintf(`{"id":%q,"runtime_id":%q,"workspace_id":%q,"dispatched_at":"2026-10-04T00:00:00Z","start_claim_supported":true}`, testID, testID, other)
	for _, body := range []string{cross, valid + "," + valid} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"tasks":[%s]}`, body) }))
		api, _ := New(server.URL, "token")
		if _, err := api.ClaimBatch(context.Background(), testID, map[string]string{testID: testID}, 1); err == nil {
			t.Fatal("unsafe batch accepted")
		}
		server.Close()
	}
}
