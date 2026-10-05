package docker

import (
	"bytes"
	"encoding/json"

	"github.com/gently-whitesnow/multica-sandbox/internal/execution"
)

func agentFailure(data []byte) error {
	for _, line := range bytes.Split(data, []byte("\n")) {
		var event struct {
			Type  string `json:"type"`
			Error struct {
				Name string `json:"name"`
				Data struct {
					Status int `json:"statusCode"`
				} `json:"data"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &event) == nil && event.Type == "error" {
			status := 0
			if event.Error.Name == "APIError" && event.Error.Data.Status >= 400 && event.Error.Data.Status <= 599 {
				status = event.Error.Data.Status
			}
			return &execution.AgentFailure{Status: status}
		}
	}
	return nil
}
