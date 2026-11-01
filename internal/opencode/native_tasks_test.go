package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

func TestNativeTelegramPromptAndLostContinuity(t *testing.T) {
	for _, room := range []string{"p2p", "group", ""} {
		task := chatClaim()
		task.ChatChannelType = "telegram"
		task.ChatType = room
		prompt, brief, err := Prompt(task, true, true, Lost)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{"operating inside a Telegram conversation", "final outcome only", "delivers replies to Telegram as text only", "context could NOT be restored"} {
			if !strings.Contains(string(prompt), s) {
				t.Errorf("missing %q", s)
			}
		}
		if strings.Contains(string(prompt), "read it back with `multica chat history`") || strings.Contains(string(brief), "Multica issue for this run") {
			t.Fatalf("Telegram prompt promises unavailable context: %s", prompt)
		}
		want := "Audience: unknown."
		if room == "p2p" {
			want = "Audience: direct room."
		}
		if room == "group" {
			want = "Audience: group room; not private"
		}
		if !strings.Contains(string(prompt), want) {
			t.Errorf("room %q missing %q", room, want)
		}
	}
	task := chatClaim()
	task.ChatChannelType = "telegram"
	task.ChatChannelDeliversFiles = true
	task.ChatMessageAttachments = []multica.Attachment{{ID: issueID, Filename: "fixture.txt"}}
	prompt, _, err := Prompt(task, true, true, Resumed)
	if err != nil || !strings.Contains(string(prompt), "File upload/download and attachment delivery are unavailable") || strings.Contains(string(prompt), "Multica sends it into the Telegram conversation") || strings.Contains(string(prompt), "multica attachment download") || strings.Contains(string(prompt), "Session Continuity Notice") {
		t.Fatalf("per-turn delivery/resume: %v %s", err, prompt)
	}
}

func TestRunOnlyAutopilotCarriesItsActualInstruction(t *testing.T) {
	task := safeTask()
	task.IssueID = ""
	// Decode the native HTTP claim fields rather than constructing only Go fields.
	if err := json.Unmarshal([]byte(`{"autopilot_run_id":"80000000-0000-4000-8000-000000000001","autopilot_id":"80000000-0000-4000-8000-000000000002","autopilot_title":"Fixture scheduled instruction","autopilot_description":"Read the repository and report the outcome.","autopilot_source":"schedule","autopilot_trigger_payload":{"check":"fixture"}}`), &task); err != nil {
		t.Fatal(err)
	}
	prompt, brief, err := Prompt(task, true, true, Fresh)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"There is no assigned Multica issue", task.AutopilotDescription, `{"check":"fixture"}`, "Trigger source: schedule"} {
		if !strings.Contains(string(prompt), s) {
			t.Errorf("missing %q in prompt", s)
		}
	}
	for _, s := range []string{"final assistant output is captured automatically as the autopilot run result", "Do not run `multica issue get`"} {
		if !strings.Contains(string(brief), s) {
			t.Errorf("missing %q in brief", s)
		}
	}
	if strings.Contains(string(prompt), "Your assigned issue ID") || strings.Contains(string(brief), "Final results MUST be delivered via") {
		t.Fatal("autopilot receives issue workflow")
	}
	// The provider supports the full upstream bounded webhook payload, not the
	// ordinary issue/chat 64KiB bound, and rejects malformed/oversized envelopes.
	task.AutopilotTriggerPayload = json.RawMessage(`{"value":"` + strings.Repeat("x", 70<<10) + `"}`)
	if _, _, err = Prompt(task, true, true, Fresh); err != nil {
		t.Fatalf("valid webhook payload refused: %v", err)
	}
	task.AutopilotTriggerPayload = json.RawMessage(`{broken`)
	if _, _, err = Prompt(task, true, true, Fresh); err == nil {
		t.Fatal("malformed payload accepted")
	}
	task.AutopilotTriggerPayload = nil
	task.IssueID = issueID
	if _, _, err = Prompt(task, true, true, Fresh); err == nil {
		t.Fatal("autopilot/issue confusion accepted")
	}
}

func TestNativePerTurnContextIsNotInRetainedBrief(t *testing.T) {
	task := safeTask()
	task.InitiatorName = "Fixture originator"
	task.ProjectWorkflow = &multica.ProjectWorkflow{Brief: "Current project step: validate"}
	prompt, brief, err := Prompt(task, true, true, Fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "Fixture originator") || !strings.Contains(string(prompt), "Current project step: validate") || strings.Contains(string(brief), "Fixture originator") || strings.Contains(string(brief), "Current project step: validate") {
		t.Fatal("run-scoped context lost or embedded in stable brief")
	}
}

func TestEditedCommentSupersedesResumedInstruction(t *testing.T) {
	task := safeTask()
	task.IssueID = issueID
	task.TriggerCommentID = triggerID
	task.TriggerCommentContent = "Use the corrected instruction"
	task.TriggerCommentEditedAt = "2026-10-31T10:00:00Z"
	task.TriggerCommentRevision = 2
	prompt, _, err := Prompt(task, true, true, Resumed)
	if err != nil || !strings.Contains(string(prompt), "[EDITED COMMENT]") || !strings.Contains(string(prompt), "revision 2") || !strings.Contains(string(prompt), "plans that depended on it, as withdrawn") {
		t.Fatalf("edited context lost: %v %s", err, prompt)
	}
	prompt, _, err = Prompt(task, true, true, Fresh)
	if err != nil || strings.Contains(string(prompt), "[EDITED COMMENT]") {
		t.Fatalf("fresh run misrepresents retained old context: %v", err)
	}
}

func TestIssueWakeupPreservesDeferredInstruction(t *testing.T) {
	task := safeTask()
	task.IssueID = issueID
	task.WakeupID = triggerID
	task.HandoffNote = "Owner access window was approved; continue delivery."
	task.WakeupJoined = "Also check the latest CI outcome."
	prompt, _, err := Prompt(task, true, true, Fresh)
	if err != nil || !strings.Contains(string(prompt), "[WAKEUP]") || !strings.Contains(string(prompt), task.HandoffNote) || !strings.Contains(string(prompt), "trigger reports a fact, not business completion") || !strings.Contains(string(prompt), task.WakeupJoined) {
		t.Fatalf("wakeup instruction lost: %v %s", err, prompt)
	}
}
