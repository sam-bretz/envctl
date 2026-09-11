package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/guestjob"
	vm "github.com/sam-bretz/envctl/internal/runtime"
)

func simpleSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"summary": map[string]any{"type": "string"}, "accepted": map[string]any{"type": "boolean"}}, "required": []string{"summary", "accepted"}, "additionalProperties": false}
}
func TestRequestUsesGuestScopeAndExplicitResume(t *testing.T) {
	i := Invocation{ID: "attempt_one", Role: "worker", Directory: "/work/envctl/repos/app", Prompt: "Implement the task", Schema: simpleSchema(), TimeoutSeconds: 300, Session: "019edc18-aaa1-755e-b930-980a2e70fede"}
	credential := Credential{APIKey: "a-private-key", Secrets: []string{"a-private-key"}}
	req, err := (Codex{}).Request(i, credential)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(req.Args, " "), credential.APIKey) || req.Env["CODEX_API_KEY"] != credential.APIKey || req.Input != i.Prompt {
		t.Fatal("credentials/prompt escaped stdin or scoped environment")
	}
	if !slices.Contains(req.Args, "resume") || !slices.Contains(req.Args, i.Session) || slices.Contains(req.Args, "--last") {
		t.Fatal("ambiguous session resume")
	}
	i.Directory = "/work/envctl/../../host"
	if _, err = (Codex{}).Request(i, credential); err == nil {
		t.Fatal("host directory accepted")
	}
	i.Directory = "/work/envctl/repos/app"
	i.Role = "other"
	if _, err = (Codex{}).Request(i, credential); err == nil {
		t.Fatal("unscoped role accepted")
	}
}
func TestCredentialsAreReferencesAndErrorsContainNoValues(t *testing.T) {
	t.Setenv("ENVCTL_TEST_CREDENTIAL", "secret-value")
	c, err := CodexCredential(t.TempDir(), "env:ENVCTL_TEST_CREDENTIAL")
	if err != nil || c.APIKey != "secret-value" {
		t.Fatal(err)
	}
	if _, err = CodexCredential("", "secret-value"); err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatal("inline credential accepted or leaked")
	}
	d := t.TempDir()
	if err = os.WriteFile(filepath.Join(d, "auth.json"), []byte(`{"tokens":{"access_token":"access-secret","refresh_token":"refresh-secret","id_token":"id-secret"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = CodexCredential(d, "file:auth.json")
	if err != nil || len(c.Secrets) != 3 {
		t.Fatal("auth redaction inventory incomplete", err)
	}
}
func TestSessionIsExtractedFromStructuredIdentityOnly(t *testing.T) {
	id := "019edc18-aaa1-755e-b930-980a2e70fede"
	if Session("plain text "+id) != "" {
		t.Fatal("prose claimed a session")
	}
	if Session("warning\n"+`{"type":"thread.started","thread_id":"`+id+`"}`) != id {
		t.Fatal("session event ignored")
	}
}

func TestRealCodexWorkerSupervisorAndResume(t *testing.T) {
	if os.Getenv("ENVCTL_AGENT_TEST") != "1" {
		t.Skip("set ENVCTL_AGENT_TEST=1 for real agent/VM acceptance")
	}
	credential, err := CodexCredential("", os.Getenv("ENVCTL_CODEX_CREDENTIAL"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	stateDir := os.Getenv("ENVCTL_AGENT_STATE_DIR")
	if stateDir == "" {
		stateDir = t.TempDir()
	}
	provider := vm.NewLima(stateDir)
	provider.Log = os.Stderr
	runtimeID := os.Getenv("ENVCTL_AGENT_VM")
	if runtimeID == "" {
		runtimeID = "envctl-agenttest-" + time.Now().UTC().Format("20060102150405")
	}
	spec := vm.DefaultSpec(runtimeID)
	spec.MemoryGiB = 2
	t.Cleanup(func() {
		if os.Getenv("ENVCTL_AGENT_KEEP_VM") == "1" && os.Getenv("ENVCTL_AGENT_STATE_DIR") != "" {
			t.Logf("retained acceptance VM %s, ownership state %s", spec.ID, stateDir)
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := provider.Destroy(cleanup, spec.ID); err != nil {
			t.Log(err)
		}
	})
	if _, err = provider.Ensure(ctx, spec); err != nil {
		t.Fatal(err)
	}
	guest := guestjob.Client{Provider: provider, Runtime: spec.ID}
	if err = guest.Install(ctx); err != nil {
		t.Fatal(err)
	}
	exerciseNoImplicitForwarding(t, ctx, guest)
	exerciseGuestRepositories(t, ctx, provider, spec.ID)
	codex := Codex{Guest: guest}
	if err = codex.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if err = codex.Install(ctx); err != nil {
		t.Fatal("pinned install was not repeatable", err)
	}
	nonce := fmt.Sprintf("test_%d", time.Now().UnixNano())
	dir := "/work/envctl/spike/" + nonce
	setup := `set -eu
mkdir -p "$1"
cd "$1"
git init -q
git config user.name 'envctl test'
git config user.email 'envctl-test@example.invalid'
cat > slug.py <<'PY'
def slugify(value):
    raise NotImplementedError
PY
git add slug.py
git commit -qm 'initial fixture'
`
	if err = provider.Exec(ctx, spec.ID, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "sh", "-c", setup, "fixture-setup", dir}}); err != nil {
		t.Fatal(err)
	}
	invoke := func(i Invocation) (json.RawMessage, string) {
		t.Helper()
		i.ID = nonce + "_" + i.ID
		if _, err := codex.Start(ctx, i, credential); err != nil {
			t.Fatal(err)
		}
		var output strings.Builder
		cursor := int64(0)
		for {
			status, err := guest.Poll(ctx, i.ID, cursor)
			if err != nil {
				t.Fatal(err)
			}
			output.WriteString(status.Output)
			cursor = status.Cursor
			if status.State == "completed" {
				// Drain all output pages after a fast completion.
				for status.Output != "" {
					status, err = guest.Poll(ctx, i.ID, cursor)
					if err != nil {
						t.Fatal(err)
					}
					output.WriteString(status.Output)
					cursor = status.Cursor
				}
				result, err := codex.Result(ctx, i.ID)
				if err != nil {
					t.Fatalf("structured result: %v\n%s", err, output.String())
				}
				return result, output.String()
			}
			if status.State != "running" && status.State != "starting" {
				t.Fatalf("agent ended in %s: %s\n%s", status.State, status.Detail, output.String())
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	worker := Invocation{ID: "worker_feature", Role: "worker", Directory: dir, Schema: simpleSchema(), TimeoutSeconds: 300, Model: os.Getenv("ENVCTL_TEST_MODEL"), Prompt: "Implement slugify(value) in slug.py: lowercase ASCII, trim whitespace, replace each sequence of non-alphanumeric characters with one hyphen, and trim edge hyphens. Add test_slug.py at the repository root using unittest, covering Hello, World! -> hello-world, spaces-only -> empty, a__b -> a-b, and already-valid -> already-valid. The required acceptance command is python3 -m unittest discover -v from the repository root, and it must discover and pass these tests. Work only on these fixture files. Return structured summary and accepted=true only if implemented and tested. Do not inspect credentials or harness configuration."}
	workerResult, events := invoke(worker)
	session := Session(events)
	if session == "" {
		t.Fatal("real worker emitted no durable session identity")
	}
	for correction := 0; correction < 3; correction++ {
		var evidence bytes.Buffer
		err = provider.Exec(ctx, spec.ID, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "sh", "-c", `cd "$1" && python3 -m unittest discover -v && python3 -c 'from slug import slugify; assert slugify("Hello, World!")=="hello-world"; assert slugify("   ")==""; assert slugify("a__b")=="a-b"; assert slugify("already-valid")=="already-valid"'`, "independent-check", dir}, Stdout: &evidence, Stderr: &evidence})
		if err == nil {
			break
		}
		t.Logf("independent checks failed: %v\n%s\nWorker result: %s", err, evidence.String(), workerResult)
		if correction == 2 {
			t.Fatalf("independent acceptance checks still fail after correction: %v\n%s\n%s", err, evidence.String(), events)
		}
		repair := worker
		repair.ID = fmt.Sprintf("worker_correction_%d", correction+1)
		repair.Session = session
		repair.Prompt = "Continue this task. The coordinator's independent acceptance command failed. Correct the implementation or test discovery while preserving all original requirements. Required command: python3 -m unittest discover -v from the repository root, which must find test_slug.py. Evidence:\n" + evidence.String()
		workerResult, events = invoke(repair)
	}
	supervisor := Invocation{ID: "supervisor_feature", Role: "supervisor", Directory: dir, Schema: simpleSchema(), TimeoutSeconds: 300, Model: worker.Model, Prompt: "Review slug.py and its tests against this exact requirement: lowercase ASCII, trim whitespace, replace each sequence of non-alphanumeric characters with one hyphen, and trim edge hyphens. Cases: Hello, World! -> hello-world; spaces-only -> empty; a__b -> a-b; already-valid unchanged. Inspect files, execute tests, and return accepted=true only if they meet the requirement; otherwise return accepted=false and explain. Do not change files. Do not inspect credentials or harness configuration."}
	result, _ := invoke(supervisor)
	var review struct {
		Accepted bool   `json:"accepted"`
		Summary  string `json:"summary"`
	}
	if json.Unmarshal(result, &review) != nil || !review.Accepted {
		t.Fatalf("supervisor rejected feature: %s", result)
	}
	resume := worker
	resume.ID = "worker_resume"
	resume.Session = session
	resume.Prompt = "Continue your previous fixture task. Re-run the existing tests without changing files. Return accepted=true only if they still pass, and summarize the feature from your prior context."
	invoke(resume)
	t.Logf("real worker session %s implemented the feature, independent checks passed, separate supervisor accepted, and explicit session continuation completed", session)
}
