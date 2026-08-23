package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Condensing a backend's output stream into one short line per action is the
// difference between "PLANNING (12m, no output)" and a running commentary of
// what the agent is reading and writing. The orchestrator does not interpret
// these lines — they go to the console and to the job's progress events — so
// the parser is deliberately forgiving: anything it does not recognise is
// passed through as text, and anything that is pure protocol is dropped.

// maxProgressLine bounds one condensed line. Agent prose is paragraphs; a
// progress line is a status bar.
const maxProgressLine = 160

// CondenseLine turns one line of backend output into a short human-readable
// progress line, or "" when the line carries nothing worth showing (protocol
// envelopes, tool results, the final result object — the caller already
// records exit code and token counts from that one).
func CondenseLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if !strings.HasPrefix(line, "{") {
		return truncateLine(line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return truncateLine(line)
	}
	switch str(m["type"]) {
	case "result", "system", "user":
		// "user" carries tool_result echoes: high volume, no information a
		// watching human can act on.
		return ""
	}
	msg, _ := m["message"].(map[string]any)
	content := contentOf(m, msg)
	if content == nil {
		// Some backends put a bare "text"/"content" string at the top level.
		for _, k := range []string{"text", "content", "delta", "summary"} {
			if s := str(m[k]); s != "" {
				return truncateLine(s)
			}
		}
		return ""
	}
	var parts []string
	for _, raw := range content {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch str(block["type"]) {
		case "text":
			if s := firstLine(str(block["text"])); s != "" {
				parts = append(parts, s)
			}
		case "tool_use":
			parts = append(parts, describeTool(str(block["name"]), block["input"]))
		case "thinking":
			// Reasoning is the agent's own draft, not an action it took.
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return truncateLine(strings.Join(parts, " | "))
}

func contentOf(m, msg map[string]any) []any {
	if msg != nil {
		if c, ok := msg["content"].([]any); ok {
			return c
		}
	}
	if c, ok := m["content"].([]any); ok {
		return c
	}
	return nil
}

// describeTool renders a tool call as "Tool arg" using whichever input field
// names the thing being acted on. The field varies by tool and by backend, so
// the list is ordered by how specific the field is, not by how common.
func describeTool(name string, input any) string {
	if name == "" {
		name = "tool"
	}
	in, ok := input.(map[string]any)
	if !ok {
		return name
	}
	for _, k := range []string{"file_path", "path", "notebook_path", "command", "pattern", "query", "url", "prompt", "description"} {
		if v := str(in[k]); v != "" {
			return fmt.Sprintf("%s %s", name, firstLine(v))
		}
	}
	return name
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// truncateLine bounds a progress line by runes, not bytes: a path or a
// message with any non-ASCII in it must not be cut mid-character.
func truncateLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\t", " "))
	r := []rune(s)
	if len(r) <= maxProgressLine {
		return s
	}
	return string(r[:maxProgressLine-1]) + "…"
}
