package opencode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// exportUsage reads the entire export without Capture's 64 KiB truncation. It
// decodes one message at a time so retained history does not accumulate in memory.
// OpenCode may print a banner before the JSON; bound that preamble to 64 KiB.
func exportUsage(stdout io.Reader, since int64) (multica.Usage, error) {
	r := bufio.NewReader(stdout)
	for n := 0; ; n++ {
		b, err := r.ReadByte()
		if err != nil || n >= 65536 {
			return multica.Usage{}, fmt.Errorf("opencode export has no JSON object")
		}
		if b == '{' {
			_ = r.UnreadByte()
			break
		}
	}
	d := json.NewDecoder(r)
	_, _ = d.Token() // The opening object was found above.
	var total multica.Usage
	found := false
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return multica.Usage{}, err
		}
		if key == "messages" {
			var usage multica.Usage
			usage, err = exportMessages(d, since)
			total.Input += usage.Input
			total.Output += usage.Output
			total.CacheRead += usage.CacheRead
			total.CacheWrite += usage.CacheWrite
			found = true
		} else {
			err = skipExportValue(d)
		}
		if err != nil {
			return multica.Usage{}, err
		}
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') || !found {
		return multica.Usage{}, fmt.Errorf("invalid opencode export")
	}
	// Drain trailing output so the export process can exit without filling stdout.
	_, err := io.Copy(io.Discard, r)
	return total, err
}

func exportMessages(d *json.Decoder, since int64) (multica.Usage, error) {
	if token, err := d.Token(); err != nil || token != json.Delim('[') {
		return multica.Usage{}, fmt.Errorf("opencode export messages must be an array")
	}
	var total multica.Usage
	for d.More() {
		var message struct {
			Info struct {
				Role   string
				Time   struct{ Created int64 }
				Tokens *struct {
					Input, Output, Reasoning int64
					Cache                    struct{ Read, Write int64 }
				}
			}
		}
		if err := d.Decode(&message); err != nil {
			return multica.Usage{}, err
		}
		if t := message.Info.Tokens; message.Info.Role == "assistant" && message.Info.Time.Created >= since && t != nil {
			total.Input += max(t.Input, 0)
			total.Output += max(t.Output, 0) + max(t.Reasoning, 0)
			total.CacheRead += max(t.Cache.Read, 0)
			total.CacheWrite += max(t.Cache.Write, 0)
		}
	}
	_, err := d.Token()
	return total, err
}

// skipExportValue discards metadata without retaining its object or array.
func skipExportValue(d *json.Decoder) error {
	depth := 0
	for {
		token, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}
