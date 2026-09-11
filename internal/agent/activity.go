package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Activity lines summarize a harness's native event stream for live display.
// Unknown or malformed events are skipped: progress is advisory, never evidence.

func (c Codex) Activity(stream string) []string  { return CodexActivity(stream) }
func (c Claude) Activity(stream string) []string { return ClaudeActivity(stream) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

// CodexActivity reads `codex exec --json` events.
func CodexActivity(stream string) []string {
	var lines []string
	started := map[string]int{}
	for _, raw := range strings.Split(stream, "\n") {
		var e struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
			Item struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Text     string `json:"text"`
				Command  string `json:"command"`
				ExitCode *int   `json:"exit_code"`
				Changes  []struct {
					Path string `json:"path"`
				} `json:"changes"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(raw), &e) != nil {
			continue
		}
		switch e.Type {
		case "item.started", "item.completed":
			line := ""
			switch e.Item.Type {
			case "agent_message":
				line = "agent: " + firstLine(e.Item.Text)
			case "command_execution":
				line = "running: " + firstLine(e.Item.Command)
				if e.Type == "item.completed" && e.Item.ExitCode != nil {
					line = fmt.Sprintf("ran (exit %d): %s", *e.Item.ExitCode, firstLine(e.Item.Command))
				}
			case "file_change":
				var paths []string
				for _, change := range e.Item.Changes {
					paths = append(paths, change.Path)
				}
				line = "edited: " + strings.Join(paths, ", ")
			case "reasoning", "":
				continue
			default:
				line = e.Item.Type
			}
			if i, ok := started[e.Item.ID]; ok && e.Item.ID != "" {
				lines[i] = line
				continue
			}
			if e.Type == "item.started" && e.Item.ID != "" {
				started[e.Item.ID] = len(lines)
			}
			lines = append(lines, line)
		case "turn.failed":
			lines = append(lines, "failed: "+firstLine(e.Error.Message))
		case "error":
			lines = append(lines, "error: "+firstLine(e.Message))
		}
	}
	return lines
}

// ClaudeActivity reads Claude Code `--output-format stream-json` events.
func ClaudeActivity(stream string) []string {
	var lines []string
	for _, raw := range strings.Split(stream, "\n") {
		var e struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(raw), &e) != nil {
			continue
		}
		switch e.Type {
		case "assistant":
			var blocks []struct {
				Type  string         `json:"type"`
				Text  string         `json:"text"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			}
			if json.Unmarshal(e.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if line := firstLine(b.Text); line != "" {
						lines = append(lines, "agent: "+line)
					}
				case "tool_use":
					line := "tool " + b.Name
					for _, key := range []string{"command", "file_path", "pattern", "path"} {
						if value, ok := b.Input[key].(string); ok && value != "" {
							line += ": " + firstLine(value)
							break
						}
					}
					lines = append(lines, line)
				}
			}
		case "result":
			if e.IsError {
				lines = append(lines, "error: "+firstLine(e.Result))
			} else {
				lines = append(lines, "finished: "+e.Subtype)
			}
		}
	}
	return lines
}

// OutputActivity summarizes plain command output, such as a verification check.
func OutputActivity(output string, n int) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
