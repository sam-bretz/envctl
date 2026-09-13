package localexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	vm "github.com/sam-bretz/envctl/internal/runtime"
)

// localPython runs the guest's output-document reader on this host, mapping
// the guest scratch path into a temporary directory.
type localPython struct {
	vm.Provider
	root string
	args []string
}

func (p *localPython) Exec(ctx context.Context, _ string, c vm.Command) error {
	p.args = c.Args
	at := slices.Index(c.Args, "python3")
	if at < 0 || !slices.Contains(c.Args, "envctl-agent") {
		return nil
	}
	args := slices.Clone(c.Args[at+1:])
	args[2] = filepath.Join(p.root, strings.TrimPrefix(args[2], "/"))
	cmd := exec.CommandContext(ctx, "python3", args...)
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	return cmd.Run()
}

func TestOutputDocumentsAreReadAsTheAgentWithoutFollowingLinks(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required to run the guest reader")
	}
	b, a := fixture(t)
	p := &localPython{root: t.TempDir()}
	b.Provider = p
	ctx := context.Background()
	dir := filepath.Join(p.root, strings.TrimPrefix(outputsDirectory(a), "/"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if got, err := b.readOutput(ctx, a, "design"); err != nil || got != "" {
		t.Fatalf("missing document: %q %v", got, err)
	}
	if !slices.Contains(p.args, "envctl-agent") {
		t.Fatal("documents must be read as the agent user")
	}

	body := "# Design\n\nThe real content.\n"
	if err := os.WriteFile(filepath.Join(dir, "design.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := b.readOutput(ctx, a, "design"); err != nil || got != body {
		t.Fatalf("document not read: %q %v", got, err)
	}

	secret := filepath.Join(p.root, "secret")
	if err := os.WriteFile(secret, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "plan.md")); err != nil {
		t.Fatal(err)
	}
	if got, err := b.readOutput(ctx, a, "plan"); err == nil || strings.Contains(got, "not yours") {
		t.Fatalf("a symlinked document was followed: %q %v", got, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "huge.md"), make([]byte, maxOutputDocument+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.readOutput(ctx, a, "huge"); err == nil {
		t.Fatal("an oversized document was accepted")
	}
	if _, err := b.readOutput(ctx, a, "../escape"); err == nil {
		t.Fatal("an unsafe document name was accepted")
	}
}
