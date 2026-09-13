package agent

import (
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestClaudeUsageUsesResultTotalsAndEstimatesRunningTurns(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"s"}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":2,"cache_creation_input_tokens":100,"cache_read_input_tokens":0,"output_tokens":1}}}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":2,"cache_creation_input_tokens":100,"cache_read_input_tokens":0,"output_tokens":40}}}
{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":900,"output_tokens":5}}}
{"type":"result","subtype":"success","total_cost_usd":0.25,"usage":{"input_tokens":3,"cache_creation_input_tokens":100,"cache_read_input_tokens":900,"output_tokens":700}}
not json
{"type":"assistant","message":{"id":"m3","usage":{"input_tokens":4,"cache_creation_input_tokens":0,"cache_read_input_tokens":1000,"output_tokens":2}}}`
	got := ClaudeUsage(stream)
	want := workflow.Usage{Input: 7, CacheWrite: 100, CacheRead: 1900, Output: 702, CostUSD: 0.25, Estimated: true}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if done := ClaudeUsage(stream[:len(stream)-len(`
{"type":"assistant","message":{"id":"m3","usage":{"input_tokens":4,"cache_creation_input_tokens":0,"cache_read_input_tokens":1000,"output_tokens":2}}}`)]); done.Estimated || done.Tokens() != 1703 {
		t.Fatalf("finished turn: %+v", done)
	}
}

func TestCodexUsageSeparatesCachedInput(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"t"}
{"type":"turn.started"}
{"type":"turn.completed","usage":{"input_tokens":20992,"cached_input_tokens":12160,"cache_write_input_tokens":0,"output_tokens":188,"reasoning_output_tokens":99}}
{"type":"turn.started"}`
	got := CodexUsage(stream)
	if got.Input != 8832 || got.CacheRead != 12160 || got.Output != 188 || got.CostUSD != 0 || !got.Estimated {
		t.Fatalf("got %+v", got)
	}
}

func TestComposingRecognizesTheModelsTurn(t *testing.T) {
	for stream, want := range map[string]bool{
		`{"type":"assistant","message":{"id":"m"}}` + "\n" + `{"type":"user","message":{"content":[{"type":"tool_result"}]}}`: true,
		`{"type":"system","subtype":"thinking_tokens"}`:                                                                       true,
		`{"type":"assistant","message":{"id":"m"}}`:                                                                           false,
		`{"type":"result","subtype":"success"}`:                                                                               false,
		`{"type":"item.completed","item":{"type":"command_execution"}}`:                                                       true,
		`{"type":"item.completed","item":{"type":"agent_message"}}`:                                                           false,
		"": false,
	} {
		if Composing(stream) != want {
			t.Errorf("Composing(%q) = %v", stream, !want)
		}
	}
}
