package localexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRealLocalPlanningAndCoordinatorRecovery(t *testing.T) {
	if os.Getenv("ENVCTL_LOCAL_EXEC_TEST") != "1" {
		t.Skip("requires explicitly selected owned acceptance VM and real Codex connection")
	}
	runRealPlanning(t, false)
}

func TestRealPlanDiscoversAdditionalReadinessRequirement(t *testing.T) {
	if os.Getenv("ENVCTL_PLAN_DISCOVERY_TEST") != "1" {
		t.Skip("opt-in real structured Plan discovery")
	}
	runRealPlanning(t, true)
}

func runRealPlanning(t *testing.T, discover bool) {
	t.Helper()
	state, name := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || name == "" {
		t.Fatal("explicit owned acceptance VM required")
	}
	source := t.TempDir()
	compose := `services:
  app:
    image: busybox:1.37.0
    command: [sh, -c, 'mkdir -p /www; echo ready > /www/index.html; httpd -f -p 8080 -h /www']
    healthcheck:
      test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:8080/']
      interval: 1s
      timeout: 1s
      retries: 10
`
	files := map[string]string{"compose.yaml": compose, "numbers.py": "def add(a, b):\n    return a - b\n", "test_numbers.py": "import unittest\nfrom numbers import add\nclass AddTest(unittest.TestCase):\n    def test_add(self): self.assertEqual(add(2, 3), 5)\n"}
	for path, body := range files {
		if err := os.WriteFile(filepath.Join(source, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "commit", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = source
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", raw, err)
		}
	}
	c, err := workflow.Parse([]byte(fmt.Sprintf("version: 2\nproject: fixture\nrepositories: [{id: app, url: %q}]\nruntime: {memory_gib: 2}\nstack: {files: [compose.yaml]}\nworkflow:\n  template: feature\n  nodes:\n    qa:\n      checks: [{name: arithmetic, command: [python3, -m, unittest, discover, -v]}]\n", source)))
	if err != nil {
		t.Fatal(err)
	}
	c.Dir = source
	objective := "Fix add(a,b) to perform addition. Preserve the API, verify 2+3=5 with unittest, and produce a reviewed PR."
	if discover {
		objective += " QA additionally requires a connection to an isolated arithmetic reference service, identified by capability arithmetic.fixture. Include that dependency for the qa node in the structured Plan requirements; it is intentionally not configured yet. Do not simulate or waive this connection. The coordinator will resolve it through an invocation plugin after planning."
	}
	r, err := workflow.NewRun("planning integration", objective, "test", c, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	r.Current().State = "preparing"
	r.Current().Runtime = workflow.RuntimeState{ID: name, Provider: "lima", Location: "local", State: "preparing"}
	dir := filepath.Join(os.TempDir(), "envctl-local-exec-"+r.ID)
	resume := os.Getenv("ENVCTL_LOCAL_EXEC_RESUME")
	if discover {
		resume = os.Getenv("ENVCTL_PLAN_DISCOVERY_RESUME")
	}
	if resume != "" {
		dir = resume
	}
	t.Log("durable acceptance state:", dir)
	store, err := runstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if resume != "" {
		runs, e := store.List(ctx)
		if e != nil || len(runs) != 1 {
			t.Fatal("resume requires exactly one retained acceptance run", e)
		}
		r = &runs[0]
		if r.Current().Runtime.ID != name {
			t.Fatal("resume VM does not match retained run")
		}
	} else if _, err = store.Create(ctx, "create", "real fixture", r); err != nil {
		t.Fatal(err)
	}
	b := New(store)
	b.Provider = vm.NewLima(state)
	e := &engine.Engine{Store: store, Backend: b}
	restarted := false
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		a := engine.Assignment{Run: *r, Revision: *r.Current()}
		if p, err := b.currentStack(a); err == nil {
			if err = b.stack(a).Down(clean, p, true); err != nil {
				t.Errorf("stack cleanup: %v", err)
			}
		}
	})
	last := ""
	for ctx.Err() == nil {
		if err = e.Reconcile(ctx, r.ID, r.CurrentRevision); err != nil {
			t.Fatal(err)
		}
		r, err = store.Get(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		summary := fmt.Sprintf("%s checkpoints=%d attempts=%d", r.Current().State, len(r.Current().Checkpoints), len(r.Current().Attempts))
		if r.Current().Recovery != nil {
			summary += " recovery=" + r.Current().Recovery.Detail
		}
		if summary != last {
			t.Log(summary)
			last = summary
		}
		for _, attempt := range r.Current().Attempts {
			if attempt.Node != "task" && attempt.Node != "plan" {
				t.Fatal("downstream stage admitted with unavailable publication binding")
			}
			if attempt.Session != "" && !restarted {
				if err = store.Close(); err != nil {
					t.Fatal(err)
				}
				store, err = runstore.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				b = New(store)
				b.Provider = vm.NewLima(state)
				e = &engine.Engine{Store: store, Backend: b}
				restarted = true
				t.Log("recreated coordinator and backend during live agent execution")
			}
			if attempt.Node == "plan" && attempt.Result != nil {
				if _, ok := r.Current().Checkpoints["task"]; !ok || !restarted {
					t.Fatal("Task/recovery acceptance missing")
				}
				if _, ok := r.Current().Checkpoints["plan"]; ok {
					t.Fatal("Plan checkpoint bypassed missing publication")
				}
				problems := strings.Join(r.Current().ReadinessProblems(time.Now(), r.Current().Requirements()), "\n")
				if !strings.Contains(problems, "publication.pr") {
					t.Fatalf("missing capability not visible: %s", problems)
				}
				if discover {
					found := false
					for _, requirement := range r.Current().DiscoveredRequirements {
						if requirement.Capability == "arithmetic.fixture" && len(requirement.Nodes) == 1 && requirement.Nodes[0] == "qa" && requirement.Reason != "" {
							found = true
						}
					}
					if !found || !strings.Contains(problems, "arithmetic.fixture") {
						t.Fatal("real Plan did not add its discovered capability to executable admission")
					}
					t.Log("real worker and supervisor produced a structured additional requirement; durable inventory and failed admission verified")
				}
				t.Log("real Task checkpoint, worker/supervisor sessions, coordinator recovery, and Plan admission hold verified")
				return
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatal("real local planning acceptance timed out:", ctx.Err())
}
