package workflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRunUsageAddsFinishedLiveAndProbeUsageAcrossRevisions(t *testing.T) {
	c, err := Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	digest := Digest(c)
	r, err := NewRun("demo", "objective", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	old := r.Current()
	old.Attempts = []Attempt{
		{ID: "a1", State: "failed", Usage: &Usage{Input: 10, CacheRead: 1000, Output: 90, CostUSD: 0.5}},
		// Final usage wins over stale live progress.
		{ID: "a2", State: "checkpointed", Usage: &Usage{Output: 100}, Progress: &Progress{Usage: &Usage{Output: 999}}},
	}
	old.ProbeUsage = map[string]Usage{"harness.worker": {CacheWrite: 50, CostUSD: 0.01}}
	next := *old
	next.ID, next.ProbeUsage = "rev_next", nil
	next.Attempts = []Attempt{{ID: "a3", State: "running", Progress: &Progress{Usage: &Usage{Input: 5, Output: 45, Estimated: true}}}}
	r.Revisions = append(r.Revisions, next)
	r.CurrentRevision = next.ID

	u := r.Usage()
	if u.Tokens() != 1300 || u.CacheRead != 1000 || !u.Estimated || u.CostUSD < 0.509 || u.CostUSD > 0.511 {
		t.Fatalf("run usage %+v", u)
	}
	if c.RunTokenLimit() != DefaultRunTokens {
		t.Fatal("default ceiling not applied")
	}
	summary := r.UsageSummary()
	// 1000 cache reads count as 100: 10 + 90 + 100 + 50 + 5 + 45 + 100.
	if got := r.CountedTokens(); got != 400 {
		t.Fatalf("counted tokens %d", got)
	}
	for _, want := range []string{"tokens 400 counted of 20.0M", "1.3K reported", "cache reads 1.0K at 10%", "API-equivalent $0.51", "running jobs estimated"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q lacks %q", summary, want)
		}
	}

	r.Current().Config.Limits.RunTokens = 401
	if over, _ := r.Budget(); over {
		t.Fatal("cache reads counted in full")
	}
	r.Current().Config.Limits.RunTokens = 400
	if over, reason := r.Budget(); !over || !strings.Contains(reason, "counted 400 of its 400 token ceiling") || !strings.Contains(reason, "1.3K tokens reported") {
		t.Fatalf("token ceiling not reached: %v %q", over, reason)
	}
	full := 1.0
	r.Current().Config.Limits.CacheReadWeight, r.Current().Config.Limits.RunTokens = &full, 1300
	if over, _ := r.Budget(); !over || r.CountedTokens() != 1300 {
		t.Fatal("a cache_read_weight of 1 does not count cache reads in full")
	}
	r.Current().Config.Limits.CacheReadWeight = nil
	r.Current().Config.Limits.RunTokens = -1
	r.Current().Config.Limits.RunCostUSD = 0.5
	if over, reason := r.Budget(); !over || !strings.Contains(reason, "limits.run_cost_usd") {
		t.Fatalf("cost ceiling not reached: %v %q", over, reason)
	}
	r.Current().Config.Limits.RunCostUSD = 0
	if over, _ := r.Budget(); over || !strings.Contains(r.UsageSummary(), "no ceiling") {
		t.Fatal("a disabled ceiling still applies")
	}

	// Configurations that do not set the new limits keep their identity.
	if Digest(c) != digest || strings.Contains(string(mustMarshal(t, c)), "run_tokens") || strings.Contains(string(mustMarshal(t, c)), "cache_read_weight") {
		t.Fatal("unset usage limits changed the configuration identity")
	}
	for _, bad := range []string{"run_tokens: -2", "run_cost_usd: -1", "cache_read_weight: -0.1", "cache_read_weight: 1.5"} {
		if _, err := Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\nlimits: {" + bad + "}\n")); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if FormatTokens(950) != "950" || FormatTokens(12400) != "12.4K" || FormatTokens(13_300_000) != "13.3M" {
		t.Fatal("token formatting")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
