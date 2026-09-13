// Package doctor checks whether the local machine is ready to use envctl and
// reports an exact one-line fix for every problem it finds. Every external
// command runs through an injectable Runner with a short timeout, so tests
// never depend on docker, git, Lima, gh, or Xcode being present.
package doctor

import (
	"context"
	"runtime"
	"time"
)

// Status is the severity of one check's result.
type Status string

const (
	OK      Status = "ok"
	Warning Status = "warning"
	Failed  Status = "failed"
)

// Result is one check's outcome. Fix is empty when Status is OK.
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Runner executes one external command with a timeout and reports its
// captured stdout, stderr, exit code, and any error starting or running it
// (including a timeout). The real implementation is ExecRunner; tests inject
// a fake keyed on argv.
type Runner func(ctx context.Context, timeout time.Duration, name string, args ...string) (stdout, stderr string, exitCode int, err error)

// Env carries the invocation context every check needs. GOOS defaults to
// runtime.GOOS but is overridable so platform-specific checks are
// deterministic in tests on any host.
type Env struct {
	Run  Runner
	Dir  string
	GOOS string
}

// Report runs every applicable check in a fixed order and reports whether
// any of them Failed. Warnings never affect the returned bool; that is the
// only thing the caller uses to decide the process exit code.
func Report(ctx context.Context, env Env) (results []Result, failed bool) {
	if env.GOOS == "" {
		env.GOOS = runtime.GOOS
	}
	results = append(results, checkDocker(ctx, env)...)
	results = append(results, checkGit(ctx, env))
	results = append(results, checkLima(ctx, env))
	results = append(results, checkGH(ctx, env))

	manifestResult, cfg, rawVersion := checkManifest(ctx, env)
	results = append(results, manifestResult)
	if rawVersion == 2 && cfg != nil {
		results = append(results, checkCredential("agents.worker", cfg.Dir, cfg.Agents.Worker))
		results = append(results, checkCredential("agents.supervisor", cfg.Dir, cfg.Agents.Supervisor))
	}

	results = append(results, checkXcodeCLT(ctx, env)...)

	for _, r := range results {
		if r.Status == Failed {
			failed = true
		}
	}
	return results, failed
}
