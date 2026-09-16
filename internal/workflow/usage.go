package workflow

import "fmt"

// DefaultRunTokens is the counted-token ceiling for a run that does not set
// limits.run_tokens. Complete feature runs on envctl itself counted 4M to
// 13M once cache reads were weighted, so this fits a large feature with room
// for retries and still stops a runaway run.
const DefaultRunTokens int64 = 20_000_000

// DefaultCacheReadWeight counts a cache-read token as a tenth of a token.
// Agents re-read their whole conversation from the prompt cache on every tool
// call, so cache reads dominate what they report, but they cost about a tenth
// of normal input.
const DefaultCacheReadWeight = 0.1

// Usage is the model usage agents reported. Tokens are summed across jobs.
// CostUSD is the harness's own API-list-price estimate and is zero when the
// harness does not report one (Codex).
type Usage struct {
	Input      int64   `json:"input,omitempty"`
	CacheWrite int64   `json:"cache_write,omitempty"`
	CacheRead  int64   `json:"cache_read,omitempty"`
	Output     int64   `json:"output,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	// Estimated marks usage that includes jobs still running, whose final
	// output counts are not reported yet.
	Estimated bool `json:"estimated,omitempty"`
}

// Tokens is every token the harness reported, including cache reads.
func (u Usage) Tokens() int64 { return u.Input + u.CacheWrite + u.CacheRead + u.Output }

// Counted is the usage that counts toward the token ceiling: cache reads at
// weight, everything else in full.
func (u Usage) Counted(cacheReadWeight float64) int64 {
	return u.Input + u.CacheWrite + u.Output + int64(float64(u.CacheRead)*cacheReadWeight+0.5)
}

func (u Usage) IsZero() bool { return u == Usage{} }

func (u Usage) Add(v Usage) Usage {
	return Usage{Input: u.Input + v.Input, CacheWrite: u.CacheWrite + v.CacheWrite, CacheRead: u.CacheRead + v.CacheRead, Output: u.Output + v.Output, CostUSD: u.CostUSD + v.CostUSD, Estimated: u.Estimated || v.Estimated}
}

// RunTokenLimit is the effective token ceiling; 0 means no ceiling.
func (c Config) RunTokenLimit() int64 {
	switch {
	case c.Limits.RunTokens < 0:
		return 0
	case c.Limits.RunTokens == 0:
		return DefaultRunTokens
	}
	return c.Limits.RunTokens
}

// CacheReadWeight is the effective share of a cache-read token that counts.
func (c Config) CacheReadWeight() float64 {
	if c.Limits.CacheReadWeight == nil {
		return DefaultCacheReadWeight
	}
	return *c.Limits.CacheReadWeight
}

// CountedTokens is the run's usage as the token ceiling counts it.
func (r *Run) CountedTokens() int64 {
	return r.Usage().Counted(r.Current().Config.CacheReadWeight())
}

// Usage sums agent usage across every revision: finished attempts, live
// progress of running ones, and readiness probes.
func (r *Run) Usage() Usage {
	// One sum, defined per revision, so a run's total and the per-variation
	// figures a comparison shows can never disagree.
	var total Usage
	for i := range r.Revisions {
		total = total.Add(r.Revisions[i].Usage())
	}
	return total
}

// Budget reports whether the run has reached a ceiling of its current
// revision's configuration, and why.
func (r *Run) Budget() (exceeded bool, reason string) {
	u, c := r.Usage(), r.Current().Config
	if limit := c.RunTokenLimit(); limit > 0 && u.Counted(c.CacheReadWeight()) >= limit {
		return true, fmt.Sprintf("run counted %s of its %s token ceiling (limits.run_tokens; %s tokens reported, cache reads at %s)", FormatTokens(u.Counted(c.CacheReadWeight())), FormatTokens(limit), FormatTokens(u.Tokens()), weightText(c.CacheReadWeight()))
	}
	if c.Limits.RunCostUSD > 0 && u.CostUSD >= c.Limits.RunCostUSD {
		return true, fmt.Sprintf("run used $%.2f of its $%.2f cost ceiling (limits.run_cost_usd)", u.CostUSD, c.Limits.RunCostUSD)
	}
	return false, ""
}

// FormatTokens renders a token count compactly: 950, 12.4K, 13.3M.
func FormatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

// UsageFraction is the share of the token ceiling used, 0 when unlimited.
func (r *Run) UsageFraction() float64 {
	limit := r.Current().Config.RunTokenLimit()
	if limit <= 0 {
		return 0
	}
	return float64(r.CountedTokens()) / float64(limit)
}

func weightText(w float64) string { return fmt.Sprintf("%.0f%%", 100*w) }

// UsageSummary is a one-line account of the run's usage against its ceiling.
func (r *Run) UsageSummary() string {
	u, c := r.Usage(), r.Current().Config
	w := c.CacheReadWeight()
	var line string
	if limit := c.RunTokenLimit(); limit > 0 {
		line = fmt.Sprintf("tokens %s counted of %s (%.0f%%)", FormatTokens(u.Counted(w)), FormatTokens(limit), 100*r.UsageFraction())
	} else {
		line = fmt.Sprintf("tokens %s counted (no ceiling)", FormatTokens(u.Counted(w)))
	}
	line += " · " + FormatTokens(u.Tokens()) + " reported"
	if u.CacheRead > 0 {
		line += fmt.Sprintf(" · cache reads %s at %s", FormatTokens(u.CacheRead), weightText(w))
	}
	if u.CostUSD > 0 {
		line += fmt.Sprintf(" · API-equivalent $%.2f", u.CostUSD)
		if c.Limits.RunCostUSD > 0 {
			line += fmt.Sprintf(" of $%.2f", c.Limits.RunCostUSD)
		}
	}
	if u.Estimated {
		line += " · running jobs estimated"
	}
	return line
}
