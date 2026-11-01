package multica

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestClaimTokenNeverFormats(t *testing.T) {
	const token = "mat_0123456789abcdef0123456789abcdef01234567"
	var task Task
	if err := json.Unmarshal([]byte(`{"id":"x","auth_token":"`+token+`"}`), &task); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(task)
	formatted := fmt.Sprintf("%v %+v %#v %s %q", task, task, task, task.AuthToken, task.AuthToken)
	if strings.Contains(string(encoded)+formatted, token) {
		t.Fatal("claim token formatted")
	}
	if got, err := task.TaskToken(); err != nil || got != token {
		t.Fatal("claim token unavailable to the relay")
	}
	for _, bad := range []string{"", "mul_personal", "Bearer mat_x", "mat_x\nInjected: 1"} {
		task.AuthToken = Secret{bad}
		if _, err := task.TaskToken(); err == nil {
			t.Fatalf("non-task token %q accepted", bad)
		}
	}
}
