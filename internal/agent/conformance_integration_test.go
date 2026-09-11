package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/guestjob"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type harnessProof struct {
	ID, Kind, Runtime, Marker, Session string
	Reconnected, Cancelled, Verified   bool
}

func TestRealHarnessConformance(t *testing.T) {
	if os.Getenv("ENVCTL_HARNESS_CONFORMANCE") != "1" {
		t.Skip("opt-in real harness conformance")
	}
	state, runtimeID := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || runtimeID == "" {
		t.Fatal("select the owned acceptance VM explicitly")
	}
	kind := os.Getenv("ENVCTL_HARNESS_KIND")
	if kind == "" {
		kind = "claude"
	}
	dir := os.Getenv("ENVCTL_HARNESS_RESUME")
	proof := harnessProof{}
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "envctl-harness-conformance-")
		if err != nil {
			t.Fatal(err)
		}
		proof = harnessProof{ID: workflow.ID("harness"), Kind: kind, Runtime: runtimeID, Marker: workflow.ID("marker")}
	} else {
		raw, err := os.ReadFile(filepath.Join(dir, "proof.json"))
		if err != nil || json.Unmarshal(raw, &proof) != nil {
			t.Fatal("cannot resume harness proof", err)
		}
		if proof.Kind != kind || proof.Runtime != runtimeID {
			t.Fatal("resume kind/runtime mismatch")
		}
	}
	save := func() {
		t.Helper()
		raw, _ := json.Marshal(proof)
		f, err := os.CreateTemp(dir, ".proof-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())
		if _, err = f.Write(raw); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err = f.Sync(); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		if err = os.Rename(f.Name(), filepath.Join(dir, "proof.json")); err != nil {
			t.Fatal(err)
		}
	}
	save()
	t.Log("retained conformance state", dir, "kind", kind, "job prefix", proof.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	guest := guestjob.Client{Provider: vm.NewLima(state), Runtime: runtimeID}
	harness, err := Select(kind, guest)
	if err != nil {
		t.Fatal(err)
	}
	if err = guest.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if err = harness.Install(ctx); err != nil {
		t.Fatal("pinned harness installation", err)
	}
	root := "/work/envctl/conformance/" + proof.ID
	if err = guest.Provider.Exec(ctx, runtimeID, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "mkdir", "-p", root + "/first", root + "/second"}}); err != nil {
		t.Fatal(err)
	}
	invocation := func(name, role, prompt string) Invocation {
		return Invocation{ID: proof.ID + "_" + name, Role: role, Directory: root + "/first", Prompt: prompt, Schema: simpleSchema(), TimeoutSeconds: 900}
	}
	start := func(i Invocation) {
		t.Helper()
		s, err := guest.Poll(ctx, i.ID, 0)
		if err != nil {
			t.Fatal("retain job after observation error", err)
		}
		if s.State == "missing" {
			credential, err := ResolveCredential("", kind, os.Getenv("ENVCTL_HARNESS_CREDENTIAL"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = harness.Start(ctx, i, credential); err != nil {
				t.Fatal("retain job after start error", err)
			}
		} else if s.State == "pending" {
			if _, err = guest.Reconcile(ctx, i.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	complete := func(i Invocation) string {
		t.Helper()
		start(i)
		var stream strings.Builder
		var cursor int64
		lastReport := time.Time{}
		for ctx.Err() == nil {
			s, err := guest.Poll(ctx, i.ID, cursor)
			if err != nil {
				t.Fatal("retain job after polling error", err)
			}
			stream.WriteString(s.Output)
			cursor = s.Cursor
			if s.State == "running" && !proof.Reconnected {
				harness, err = Select(kind, guest)
				if err != nil {
					t.Fatal(err)
				}
				proof.Reconnected = true
				save()
				t.Log("recreated adapter with the same running guest job")
			}
			if time.Since(lastReport) > 20*time.Second {
				t.Log(i.ID, s.State)
				lastReport = time.Now()
			}
			if s.Output != "" {
				continue
			}
			if s.State == "completed" {
				raw, err := harness.Result(ctx, i.ID)
				if err != nil {
					t.Fatal(err)
				}
				var result struct {
					Accepted bool   `json:"accepted"`
					Summary  string `json:"summary"`
				}
				if json.Unmarshal(raw, &result) != nil || !result.Accepted || result.Summary == "" {
					t.Fatal("invalid structured conformance result")
				}
				if err = os.WriteFile(filepath.Join(dir, i.ID+"-result.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				session := harness.Session(stream.String())
				if session == "" {
					t.Fatal("missing explicit session identity")
				}
				return session
			}
			if s.State != "running" && s.State != "starting" && s.State != "pending" {
				t.Fatal("retained terminal harness failure", i.ID, s.State)
			}
			if s.State == "pending" {
				if _, err = guest.Reconcile(ctx, i.ID); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(250 * time.Millisecond):
			}
		}
		t.Fatal("conformance observation expired; retain and resume the same state")
		return ""
	}
	first := invocation("worker", "worker", fmt.Sprintf("This is an envctl harness acceptance test. Use the file tools or Bash to write marker.txt containing exactly %s with no newline in the current directory. Remember this marker in this conversation. Verify the file, then return accepted=true and a short summary using the required structured output. Do not create background jobs.", proof.Marker))
	workerSession := complete(first)
	if proof.Session != "" && proof.Session != workerSession {
		t.Fatal("worker session changed")
	}
	proof.Session = workerSession
	save()
	second := invocation("resume", "worker", "Continue this exact conversation from a new working directory. Write resumed.txt in the current directory containing exactly the marker you wrote earlier, with no newline. Recall it from this conversation; do not read files from the earlier directory. Verify this new file, then return accepted=true and a summary as structured output.")
	second.Directory = root + "/second"
	second.Session = proof.Session
	if session := complete(second); session != proof.Session {
		t.Fatal("resume did not continue the explicit session")
	}
	for _, file := range []string{root + "/first/marker.txt", root + "/second/resumed.txt"} {
		var out bytes.Buffer
		// The agent identity owns /work/envctl privately; read as root.
		if err = guest.Provider.Exec(ctx, runtimeID, vm.Command{Args: []string{"sudo", "cat", file}, Stdout: &out}); err != nil || out.String() != proof.Marker {
			t.Fatal("real worker file verification failed", err)
		}
	}
	supervisor := invocation("supervisor", "supervisor", fmt.Sprintf("Independently review these files without changing them: %s/first/marker.txt and %s/second/resumed.txt. Both must contain exactly %s with no newline. Use tools to read and verify both. Return accepted=true only if this is correct, with a short structured summary.", root, root, proof.Marker))
	if session := complete(supervisor); session == proof.Session {
		t.Fatal("supervisor borrowed worker session")
	}
	stopped := invocation("cancel", "worker", "Use Bash to run sleep 600 and wait for it to finish before returning any structured result. This is a coordinator cancellation test.")
	stopped.Session = proof.Session
	if !proof.Cancelled {
		start(stopped)
		var stream strings.Builder
		var cursor int64
		for ctx.Err() == nil {
			s, err := guest.Poll(ctx, stopped.ID, cursor)
			if err != nil {
				t.Fatal("retain cancellation job after polling error", err)
			}
			stream.WriteString(s.Output)
			cursor = s.Cursor
			if s.State == "cancelled" {
				proof.Cancelled = true
				save()
				break
			}
			if s.State == "running" && harness.Session(stream.String()) == proof.Session {
				if _, err = guest.Cancel(ctx, stopped.ID); err != nil {
					t.Fatal("retain cancellation job", err)
				}
				proof.Cancelled = true
				save()
				break
			}
			if s.State != "running" && s.State != "starting" && s.State != "pending" {
				t.Fatal("cancellation fixture ended before cancellation", s.State)
			}
			select {
			case <-ctx.Done():
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	s, err := guest.Poll(ctx, stopped.ID, 0)
	if err != nil || s.State != "cancelled" {
		t.Fatal("cancelled job still active", err)
	}
	recovery := invocation("recover", "worker", "The coordinator deliberately cancelled the previous attempt. Do not run or wait for sleep again. Read the existing marker.txt, verify it, and return accepted=true with a short summary using structured output.")
	recovery.Session = proof.Session
	if session := complete(recovery); session != proof.Session {
		t.Fatal("cancel recovery lost session identity")
	}
	if !proof.Reconnected {
		t.Fatal("live adapter recreation was not observed")
	}
	proof.Verified = true
	save()
	if err = guest.Provider.Exec(ctx, runtimeID, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", root}}); err != nil {
		t.Fatal("conformance fixture cleanup", err)
	}
	t.Log("verified real tool use, independent supervisor, structured results, live reconnect, cross-directory explicit resume, cancellation and recovery; fixture source cleaned")
}
