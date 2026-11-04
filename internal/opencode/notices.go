package opencode

import (
	"fmt"
	"log/slog"
	"strings"
)

// Skipped names configuration an attempt left out instead of failing its task.
type Skipped struct{ Name, Reason string }

// Notices tell the agent and the controller log what the attempt left out, by name and
// reason only, never by value.
type Notices struct {
	MCP, Skills []Skipped
	Env         []string
	// GitRelay says Git and forge access goes through the sandbox relay.
	GitRelay bool
}

// maxNoticeItems bounds each notice list in the brief and the log.
const maxNoticeItems = 16

func (n Notices) empty() bool { return len(n.MCP)+len(n.Skills)+len(n.Env) == 0 }

// Section is the brief's Sandbox Notices section, empty when nothing was left out.
func (n Notices) Section() string {
	if n.empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Sandbox Notices\n\n")
	b.WriteString("This sandbox left the following out of your configuration. Do not try to restore it; if the task depends on it, say so in your result.\n\n")
	list := func(items []Skipped, kind, missing string) {
		for i, item := range items {
			if i == maxNoticeItems {
				fmt.Fprintf(&b, "- %d more %ss %s.\n", len(items)-i, kind, missing)
				break
			}
			fmt.Fprintf(&b, "- %s `%s` %s: %s.\n", strings.ToUpper(kind[:1])+kind[1:], item.Name, missing, item.Reason)
		}
	}
	list(n.MCP, "MCP server", "not connected")
	list(n.Skills, "skill", "not installed")
	if len(n.Env) > 0 {
		b.WriteString("- Environment variables not passed (reserved for the sandbox): " + nameList(n.Env) + ".")
		if n.GitRelay {
			b.WriteString(" Git and forge access (`git`, `gh`, `glab`) goes through the sandbox relay, and commit identity comes from its Git configuration.")
		}
		b.WriteString("\n")
	}
	return b.String() + "\n"
}

func nameList(names []string) string {
	shown := names[:min(len(names), maxNoticeItems)]
	list := "`" + strings.Join(shown, "`, `") + "`"
	if more := len(names) - len(shown); more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	return list
}

// Apply adds the section to the brief, or to the prompt of a task without one.
func (n Notices) Apply(prompt, brief []byte) ([]byte, []byte) {
	section := n.Section()
	switch {
	case section == "":
	case brief != nil:
		brief = append([]byte(strings.TrimRight(string(brief), "\n")+"\n\n"), section...)
	default:
		prompt = append([]byte(strings.TrimRight(string(prompt), "\n")+"\n\n"), strings.TrimRight(section, "\n")...)
	}
	return prompt, brief
}

// Log records the notices for operators; log nil discards them.
func (n Notices) Log(log *slog.Logger, task string) {
	if log == nil {
		return
	}
	for _, group := range []struct {
		message string
		items   []Skipped
	}{{"MCP servers not connected", n.MCP}, {"skills not installed", n.Skills}} {
		if len(group.items) == 0 {
			continue
		}
		shown := make([]string, 0, maxNoticeItems)
		for _, item := range group.items[:min(len(group.items), maxNoticeItems)] {
			shown = append(shown, item.Name+": "+item.Reason)
		}
		log.Warn(group.message, "task", task, "count", len(group.items), "items", shown)
	}
	if len(n.Env) > 0 {
		log.Warn("agent environment variables not passed", "task", task, "count", len(n.Env), "names", n.Env[:min(len(n.Env), maxNoticeItems)])
	}
}
