package multica

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkspaceEnvReadsGlobalEnvRedacted(t *testing.T) {
	mode := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/workspaces/"+testID+"/env" || r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		switch mode {
		case "absent":
			w.WriteHeader(http.StatusNotFound)
		case "forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "other":
			fmt.Fprint(w, `{"workspace_id":"22222222-2222-4222-8222-222222222222","global_env":{"K":"v"}}`)
		default:
			fmt.Fprintf(w, `{"workspace_id":%q,"global_env":{"INFERENCE_API_KEY":"sk-secret","OTHER":"x"}}`, testID)
		}
	}))
	defer server.Close()
	api, _ := NewAllowHTTP(server.URL, "token")
	env, ok, err := api.WorkspaceEnv(context.Background(), testID)
	if err != nil || !ok || env.Allowed([]string{"INFERENCE_API_KEY"})["INFERENCE_API_KEY"] != "sk-secret" {
		t.Fatal(ok, err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", env, env, env), "sk-secret") {
		t.Fatal("workspace env formatted")
	}
	mode = "absent"
	if _, ok, err = api.WorkspaceEnv(context.Background(), testID); ok || err != nil {
		t.Fatal("a server without workspace env is not absent", ok, err)
	}
	for _, mode = range []string{"forbidden", "other"} {
		if _, _, err = api.WorkspaceEnv(context.Background(), testID); err == nil {
			t.Fatal(mode, "accepted")
		}
	}
	if _, _, err = api.WorkspaceEnv(context.Background(), "../x"); err == nil {
		t.Fatal("invalid workspace accepted")
	}
}
