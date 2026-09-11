package agent

import (
	"slices"
	"testing"
)

func TestCodexActivityFromExecJSONEvents(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"01a08f59-7073-7a83-b959-867ca896ce48"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"reasoning","text":"private chain"}}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"Inspecting the calculator.\nSecond line"}}
{"type":"item.started","item":{"id":"item_2","type":"command_execution","command":"/bin/bash -lc pytest","aggregated_output":"","exit_code":null,"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_2","type":"command_execution","command":"/bin/bash -lc pytest","aggregated_output":"1 passed","exit_code":0,"status":"completed"}}
{"type":"item.started","item":{"id":"item_3","type":"command_execution","command":"sleep 600","exit_code":null,"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_4","type":"file_change","changes":[{"path":"calc.py","kind":"update"}]}}
{"type":"item.completed","item":{"id":"item_5","type":"todo_list"}}
not json
{"type":"future.event","payload":{}}
{"type":"turn.failed","error":{"message":"usage limit"}}
{"type":"error","message":"stream closed"}`
	want := []string{"agent: Inspecting the calculator. …", "ran (exit 0): /bin/bash -lc pytest", "running: sleep 600", "edited: calc.py", "todo_list", "failed: usage limit", "error: stream closed"}
	if got := CodexActivity(stream); !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
	if got := (Codex{}).Activity(""); len(got) != 0 {
		t.Fatal("empty stream produced activity")
	}
}

func TestClaudeActivityFromStreamJSONEvents(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"fa193c86-0274-4b04-a263-34401088c191"}
{"type":"rate_limit_event"}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"private","signature":"x"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"I'll write the marker."}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"printf x > marker.txt\necho done"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/work/envctl/a.txt"}},{"type":"tool_use","id":"t3","name":"TodoWrite","input":{"todos":[]}}]}}
{"type":"assistant","message":{"content":"unexpected string content"}}
{"type":"result","subtype":"success","is_error":false,"result":"ok"}
{"type":"result","subtype":"success","is_error":true,"result":"Failed to authenticate"}`
	want := []string{"agent: I'll write the marker.", "tool Bash: printf x > marker.txt …", "tool Read: /work/envctl/a.txt", "tool TodoWrite", "finished: success", "error: Failed to authenticate"}
	if got := ClaudeActivity(stream); !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestOutputActivityKeepsNewestNonEmptyLines(t *testing.T) {
	got := OutputActivity("one\n\ntwo\nthree\n", 2)
	if !slices.Equal(got, []string{"two", "three"}) {
		t.Fatalf("got %q", got)
	}
}
