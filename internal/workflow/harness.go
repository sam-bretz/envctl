package workflow

const CodexHarnessVersion = "0.154.0"
const ClaudeHarnessVersion = "2.1.268"

func DefaultHarnessVersion(kind string) string {
	switch kind {
	case "codex":
		return CodexHarnessVersion
	case "claude":
		return ClaudeHarnessVersion
	default:
		return ""
	}
}
