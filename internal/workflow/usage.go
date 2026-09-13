package workflow

import "fmt"

// DefaultRunTokens is the token ceiling for a run that does not set
// limits.run_tokens: about two complete six-stage feature runs.
const DefaultRunTokens int64 = 40_000_000

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

// Usage sums agent usage across every revision: finished attempts, live
// progress of running ones, and readiness probes.
func (r *Run) Usage() Usage {
	var total Usage
	for i := range r.Revisions {
		v := &r.Revisions[i]
		for _, u := range v.ProbeUsage {
			total = total.Add(u)
		}
		for _, a := range v.Attempts {
			switch {
			case a.Usage != nil:
				total = total.Add(*a.Usage)
			case a.Progress != nil && a.Progress.Usage != nil:
				total = total.Add(*a.Progress.Usage)
			}
		}
	}
	return total
}

// Budget reports whether the run has reached a ceiling of its current
// revision's configuration, and why.
func (r *Run) Budget() (exceeded bool, reason string) {
	u, c := r.Usage(), r.Current().Config
	if limit := c.RunTokenLimit(); limit > 0 && u.Tokens() >= limit {
		return true, fmt.Sprintf("run used %s of its %s token ceiling (limits.run_tokens)", FormatTokens(u.Tokens()), FormatTokens(limit))
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
	return float64(r.Usage().Tokens()) / float64(limit)
}

// UsageSummary is a one-line account of the run's usage against its ceiling.
func (r *Run) UsageSummary() string {
	u, c := r.Usage(), r.Current().Config
	line := "tokens " + FormatTokens(u.Tokens())
	if limit := c.RunTokenLimit(); limit > 0 {
		line += fmt.Sprintf(" of %s (%.0f%%)", FormatTokens(limit), 100*r.UsageFraction())
	} else {
		line += " (no ceiling)"
	}
	if u.CacheRead > 0 {
		line += " · cache reads " + FormatTokens(u.CacheRead)
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
