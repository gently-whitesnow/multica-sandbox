package multica

import (
	"context"
	"time"
)

// Message and Usage mirror the pinned daemon HTTP reporting contract.
type Message struct {
	Seq       int            `json:"seq"`
	Type      string         `json:"type"`
	CallID    string         `json:"call_id,omitempty"`
	Tool      string         `json:"tool,omitempty"`
	Content   string         `json:"content,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	Output    string         `json:"output,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	// OutputTruncated follows the upstream 8 KiB tool-output preview.
	OutputTruncated *bool `json:"output_truncated,omitempty"`
}
type Usage struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Input      int64  `json:"input_tokens"`
	Output     int64  `json:"output_tokens"`
	CacheRead  int64  `json:"cache_read_tokens"`
	CacheWrite int64  `json:"cache_write_tokens"`
}

func (c *Client) ReportMessages(ctx context.Context, id string, messages []Message) error {
	return c.taskPost(ctx, id, "messages", map[string]any{"messages": messages})
}
func (c *Client) ReportUsage(ctx context.Context, id string, usage Usage) error {
	return c.taskPost(ctx, id, "usage", map[string]any{"usage": []Usage{usage}})
}

// ReportSession pins the session and its retained workdir mid-flight, as upstream PinTaskSession.
func (c *Client) ReportSession(ctx context.Context, id, session, workDir string) error {
	body := map[string]string{"session_id": session}
	if workDir != "" {
		body["work_dir"] = workDir
	}
	return c.taskPost(ctx, id, "session", body)
}
