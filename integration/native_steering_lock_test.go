//go:build upstream

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// Replay the deletion transaction's actual pinned queries, with a gate between
// comment and supplement locks. The claim runs through the patched HTTP server.
func nativeSteeringRoutingLocks(t *testing.T, api *multica.Client) {
	if os.Getenv("VERIFY_STEERING_NATIVE_PATCH") != "1" {
		t.Skip("requires disposable native patches")
	}
	for n, mode := range []string{"create", "bind"} {
		t.Run(mode, func(t *testing.T) {
			s := supplementAdmissionFixture(t, api, 91+n, "opencode", true, true)
			comment := routingLockInput(t, s, mode)
			tx := nativeDeleteTransaction(t)
			tx.send(t, "BEGIN; SET LOCAL deadlock_timeout='100ms'; SET LOCAL statement_timeout='5s';\n"+nativeDeleteSQL(t, "comment", "LockCommentForDelete", comment)+"\nSELECT 'comment-locked';")
			tx.until(t, "comment-locked")
			claimed := nativeClaimConcurrent(t, s)
			var result *claimHTTPResult
			eventually(t, "claim committed or waiting on the locked comment", func() bool {
				select {
				case got := <-claimed:
					result = &got
					return true
				default:
				}
				return sql(t, `SELECT count(*) FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE '%name: ClaimNextTaskSupplement%';`) != "0"
			})
			tx.send(t, nativeDeleteSQL(t, "supplement", "LockTaskSupplementsByComment", comment)+"\nSELECT 'receipts-locked';")
			tx.until(t, "receipts-locked")
			if result == nil {
				select {
				case got := <-claimed:
					result = &got
				case <-time.After(5 * time.Second):
					t.Fatal("claim still blocked behind the delete transaction")
				}
			}
			var message struct {
				CommentID string `json:"comment_id"`
			}
			if result.err != nil || result.status != 200 || json.Unmarshal(result.body, &message) != nil || message.CommentID != comment {
				t.Fatalf("claim/delete lock race: status=%d error=%v body=%s", result.status, result.err, result.body)
			}
			// The delete path now sees an in-flight receipt and rolls back with 409.
			tx.send(t, "ROLLBACK; SELECT 'released';")
			tx.until(t, "released")
			s.request("DELETE", "/api/comments/"+comment, "", 409)
			s.receipt(comment, "delivering||1")
			s.finish()
			t.Log("comment-first delete transaction and HTTP claim completed without deadlock; delete returned 409, claim returned the original input")
		})
	}
}

func routingLockInput(t *testing.T, s *supplements, mode string) string {
	t.Helper()
	if mode == "create" {
		return s.create(1, "Direct steering lock fixture", 201)
	}
	body := fmt.Sprintf(`{"content":"[@Fixture](mention://agent/%s) Bound steering lock fixture","steer_task_ids":[%q]}`, s.task.AgentID, s.task.ID)
	var comment struct {
		ID       string
		Outcomes []struct{ Status string } `json:"trigger_outcomes"`
	}
	response := s.request("POST", "/api/issues/"+s.task.IssueID+"/comments", body, 201)
	if json.Unmarshal([]byte(response), &comment) != nil || len(comment.Outcomes) != 1 || comment.Outcomes[0].Status != "steered" {
		t.Fatalf("comment was not bound: %s", response)
	}
	return comment.ID
}

func nativeDeleteSQL(t *testing.T, file, name, comment string) string {
	t.Helper()
	source := os.Getenv("MULTICA_TEST_UPSTREAM")
	if source == "" {
		t.Fatal("disposable patched source required")
	}
	data, err := os.ReadFile(filepath.Join(source, "server/pkg/db/queries", file+".sql"))
	if err != nil {
		t.Fatal(err)
	}
	_, query, found := strings.Cut(string(data), "-- name: "+name+" ")
	if !found {
		t.Fatal("native deletion query missing", name)
	}
	_, query, _ = strings.Cut(query, "\n")
	query, _, _ = strings.Cut(query, "\n-- name:")
	return strings.NewReplacer("@comment_id", "'"+comment+"'::uuid", "@id", "'"+comment+"'::uuid", "@workspace_id", "'"+workspace+"'::uuid").Replace(query)
}

type claimHTTPResult struct {
	status int
	body   []byte
	err    error
}

func nativeClaimConcurrent(t *testing.T, s *supplements) <-chan claimHTTPResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "POST", os.Getenv("MULTICA_TEST_URL")+"/api/daemon/tasks/"+s.task.ID+"/supplements/claim", strings.NewReader(`{"capabilities":["task-supplement-author-v1"]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token(t))
	req.Header.Set("Content-Type", "application/json")
	result := make(chan claimHTTPResult, 1)
	go func() {
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			result <- claimHTTPResult{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		result <- claimHTTPResult{response.StatusCode, body, err}
	}()
	return result
}

type deleteTransaction struct {
	input io.WriteCloser
	lines <-chan string
}

func nativeDeleteTransaction(t *testing.T) *deleteTransaction {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", os.Getenv("MULTICA_TEST_DB_CONTAINER"), "psql", "-U", "contract", "-d", "contract", "-At", "-v", "ON_ERROR_STOP=1", "-v", "VERBOSITY=verbose")
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_, _ = io.WriteString(input, "ROLLBACK;\n\\q\n")
		_ = input.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("delete transaction: %v: %s", err, stderr.String())
			}
		case <-time.After(3 * time.Second):
			cancel()
			<-done
		}
		cancel()
	})
	return &deleteTransaction{input, lines}
}

func (tx *deleteTransaction) send(t *testing.T, query string) {
	t.Helper()
	if _, err := io.WriteString(tx.input, query+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (tx *deleteTransaction) until(t *testing.T, marker string) {
	t.Helper()
	timeout := time.NewTimer(6 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case line, open := <-tx.lines:
			if !open {
				t.Fatal("delete transaction ended before", marker)
			}
			if line == marker {
				return
			}
		case <-timeout.C:
			t.Fatal("delete transaction blocked before", marker)
		}
	}
}
