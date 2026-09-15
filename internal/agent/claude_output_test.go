package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The guest runner gives the wrapper one pipe for stdout and stderr, and
// Claude (Node) makes its inherited stderr non-blocking, which is the same open
// pipe. A large event with a slow reader then filled the pipe and the wrapper
// died with BlockingIOError, failing the attempt. This fake harness does what
// Node does and emits events far larger than a pipe buffer.
const fakeNodeHarness = `import json,os,sys
os.set_blocking(2, False)
big = 'x' * 900000
for _ in range(3):
    print(json.dumps({'type': 'user', 'message': {'content': [{'type': 'tool_result', 'content': big}]}}), flush=True)
print(json.dumps({'type': 'result', 'subtype': 'success', 'is_error': False, 'structured_output': {'summary': 'ok'}}), flush=True)
`

func TestClaudeWrapperSurvivesANonBlockingSharedPipe(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required")
	}
	dir := t.TempDir()
	harness := filepath.Join(dir, "claude.py")
	if err = os.WriteFile(harness, []byte(fakeNodeHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "schema.json"), []byte(`{"type":"object"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-c", claudeProcess, filepath.Join(dir, "schema.json"), filepath.Join(dir, "result.json"), python, harness)
	// One pipe for both streams, as the guest runner does.
	cmd.Stdout, cmd.Stderr = w, w
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := r.Read(buf)
			out.Write(buf[:n])
			if err == io.EOF {
				done <- nil
				return
			}
			if err != nil {
				done <- err
				return
			}
			time.Sleep(time.Millisecond) // a reader slower than the writer
		}
	}()
	waitErr := cmd.Wait()
	<-done
	if waitErr != nil {
		t.Fatalf("wrapper failed (%v): %s", waitErr, tail(out.String(), 400))
	}
	if strings.Contains(out.String(), "BlockingIOError") || strings.Count(out.String(), "\n") < 4 {
		t.Fatalf("events lost or wrapper errored: %s", tail(out.String(), 400))
	}
	raw, err := os.ReadFile(filepath.Join(dir, "result.json"))
	var result map[string]any
	if err != nil || json.Unmarshal(raw, &result) != nil || result["summary"] != "ok" {
		t.Fatalf("structured result not written: %v %s", err, raw)
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
