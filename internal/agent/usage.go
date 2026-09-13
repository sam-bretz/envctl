package agent

import (
	"encoding/json"
	"strings"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func (c Codex) Usage(stream string) workflow.Usage  { return CodexUsage(stream) }
func (c Claude) Usage(stream string) workflow.Usage { return ClaudeUsage(stream) }

// ClaudeUsage reads Claude Code stream-json. A finished turn's result event
// carries exact totals and an API-list-price cost. Until it arrives, input and
// cache counts of each API response are known but output is only partial, so
// the running total is marked estimated.
func ClaudeUsage(stream string) workflow.Usage {
	type counts struct {
		Input      int64 `json:"input_tokens"`
		CacheWrite int64 `json:"cache_creation_input_tokens"`
		CacheRead  int64 `json:"cache_read_input_tokens"`
		Output     int64 `json:"output_tokens"`
	}
	var final workflow.Usage
	var live workflow.Usage
	pending := false
	responses := map[string]counts{}
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event struct {
			Type    string   `json:"type"`
			Usage   *counts  `json:"usage"`
			Cost    *float64 `json:"total_cost_usd"`
			Message *struct {
				ID    string  `json:"id"`
				Usage *counts `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		switch {
		case event.Type == "assistant" && event.Message != nil && event.Message.Usage != nil:
			// Streamed responses repeat an ID with growing output counts.
			prior := responses[event.Message.ID]
			u := *event.Message.Usage
			if u.Output < prior.Output {
				u.Output = prior.Output
			}
			responses[event.Message.ID] = u
			pending = true
		case event.Type == "result" && event.Usage != nil:
			final = final.Add(workflow.Usage{Input: event.Usage.Input, CacheWrite: event.Usage.CacheWrite, CacheRead: event.Usage.CacheRead, Output: event.Usage.Output})
			if event.Cost != nil {
				final.CostUSD += *event.Cost
			}
			// The result covers every response of its turn.
			responses = map[string]counts{}
			pending = false
		}
	}
	for _, u := range responses {
		live = live.Add(workflow.Usage{Input: u.Input, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead, Output: u.Output})
	}
	if pending {
		live.Estimated = true
	}
	return final.Add(live)
}

// CodexUsage reads Codex exec --json turn.completed events. Codex counts
// cached input inside input_tokens and reports no cost; a turn in progress has
// no usage event yet.
func CodexUsage(stream string) workflow.Usage {
	var total workflow.Usage
	started := 0
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Usage *struct {
				Input      int64 `json:"input_tokens"`
				Cached     int64 `json:"cached_input_tokens"`
				CacheWrite int64 `json:"cache_write_input_tokens"`
				Output     int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		switch event.Type {
		case "turn.started":
			started++
		case "turn.completed":
			started--
			if u := event.Usage; u != nil {
				total = total.Add(workflow.Usage{Input: max(0, u.Input-u.Cached), CacheRead: u.Cached, CacheWrite: u.CacheWrite, Output: u.Output})
			}
		}
	}
	if started > 0 {
		total.Estimated = true
	}
	return total
}
