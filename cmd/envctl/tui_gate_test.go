package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// Scripts and pipelines must never attach the dashboard or start a coordinator.
func TestScriptInvocationsNeverLaunchTheDashboard(t *testing.T) {
	stdin, stdout := os.Stdin, os.Stdout
	t.Cleanup(func() { os.Stdin, os.Stdout = stdin, stdout })
	for _, args := range [][]string{{}, {"ui"}, {"--json"}, {"ui", "--json"}} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdin, os.Stdout = r, w
		state := t.TempDir()
		var out bytes.Buffer
		cmd := root()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append(args, "--state-dir", state))
		err = cmd.Execute()
		os.Stdin, os.Stdout = stdin, stdout
		r.Close()
		w.Close()
		if err != nil || !strings.Contains(out.String(), "Usage:") {
			t.Fatalf("%v: expected help without a terminal, got %v %q", args, err, out.String())
		}
		if entries, _ := os.ReadDir(state); len(entries) != 0 {
			t.Fatalf("%v: a non-interactive invocation touched coordinator state", args)
		}
	}
}
