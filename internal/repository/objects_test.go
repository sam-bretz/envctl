package repository

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func objectCommand(t *testing.T, directory string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), directory, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func objectRepo(t *testing.T, body string) string {
	t.Helper()
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("Git LFS is required for source object acceptance")
	}
	root := sourceRepo(t)
	objectCommand(t, root, "config", "filter.lfs.process", "git-lfs filter-process")
	objectCommand(t, root, "config", "filter.lfs.required", "true")
	objectCommand(t, root, "config", "filter.lfs.clean", "git-lfs clean -- %f")
	objectCommand(t, root, "config", "filter.lfs.smudge", "git-lfs smudge -- %f")
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data.bin"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	objectCommand(t, root, "add", ".")
	objectCommand(t, root, "commit", "-qm", "LFS fixture")
	return root
}
func nestedObjectRepo(t *testing.T) (string, []string) {
	t.Helper()
	leaf := objectRepo(t, "leaf baseline\x00")
	middle := objectRepo(t, "middle baseline\x00")
	objectCommand(t, middle, "-c", "protocol.file.allow=always", "submodule", "add", "--", leaf, "leaf module")
	objectCommand(t, middle, "commit", "-qam", "nested module")
	root := objectRepo(t, "root baseline\x00")
	objectCommand(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "--", middle, "vendor/middle module")
	objectCommand(t, root, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")
	objectCommand(t, root, "commit", "-qam", "middle module")
	for _, directory := range []string{filepath.Join(root, "vendor/middle module"), filepath.Join(root, "vendor/middle module/leaf module")} {
		objectCommand(t, directory, "lfs", "fetch", "origin", "HEAD")
		objectCommand(t, directory, "lfs", "checkout")
	}
	return root, []string{root, middle, leaf}
}
func runObjects(root, pin, filename, operation string) ([]byte, error) {
	cmd := exec.Command("python3", "-c", objectsProgram, operation, root, pin, filename)
	return cmd.CombinedOutput()
}
func objectsOK(t *testing.T, root, pin, filename, operation string) string {
	t.Helper()
	raw, err := runObjects(root, pin, filename, operation)
	if err != nil {
		t.Fatalf("objects %s: %v\n%s", operation, err, raw)
	}
	return strings.TrimSpace(string(raw))
}
func cloneObjectBundle(t *testing.T, bundle, pin string) string {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "checkout")
	objectCommand(t, parent, "clone", "--no-checkout", "--", bundle, root)
	objectCommand(t, root, "config", "user.name", "fixture")
	objectCommand(t, root, "config", "user.email", "fixture@localhost")
	objectCommand(t, root, "checkout", "--detach", pin)
	return root
}
func verifyObjectFiles(t *testing.T, root, leaf string) {
	t.Helper()
	for name, want := range map[string]string{"data.bin": "root baseline\x00", "vendor/middle module/data.bin": "middle baseline\x00", "vendor/middle module/leaf module/data.bin": leaf} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(raw) != want {
			t.Fatalf("%s: object differs: %q %v", name, raw, err)
		}
	}
	if status := objectCommand(t, root, "status", "--porcelain", "--ignore-submodules=none"); status != "" {
		t.Fatal("restored source dirty:", status)
	}
}

func TestNestedSubmoduleAndLFSSnapshotsRestoreOfflineAndCaptureNewObjects(t *testing.T) {
	root, origins := nestedObjectRepo(t)
	pin := objectCommand(t, root, "rev-parse", "HEAD")
	archive := filepath.Join(t.TempDir(), "baseline.tar")
	objectsOK(t, root, pin, archive, "capture")
	bundle := filepath.Join(t.TempDir(), "main.bundle")
	objectCommand(t, root, "bundle", "create", bundle, "HEAD")
	for _, origin := range origins {
		if err := os.RemoveAll(origin); err != nil {
			t.Fatal(err)
		}
	}
	restored := cloneObjectBundle(t, bundle, pin)
	objectsOK(t, restored, pin, archive, "hydrate")
	verifyObjectFiles(t, restored, "leaf baseline\x00")
	leaf := filepath.Join(restored, "vendor/middle module/leaf module/data.bin")
	if err := os.WriteFile(leaf, []byte("new leaf object\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	if raw, err := runObjects(restored, pin, "readonly", "commit"); err == nil {
		t.Fatal("read-only stage captured submodule edits", string(raw))
	}
	changed := objectsOK(t, restored, pin, "write", "commit")
	if changed == pin {
		t.Fatal("nested submodule change did not update root gitlink history")
	}
	next := filepath.Join(t.TempDir(), "changed.tar")
	objectsOK(t, restored, changed, next, "capture")
	changedBundle := filepath.Join(t.TempDir(), "changed.bundle")
	objectCommand(t, restored, "bundle", "create", changedBundle, "HEAD")
	if err := os.RemoveAll(restored); err != nil {
		t.Fatal(err)
	}
	final := cloneObjectBundle(t, changedBundle, changed)
	objectsOK(t, final, changed, next, "hydrate")
	verifyObjectFiles(t, final, "new leaf object\x00")
	// Hydrating twice must not change committed source or require former remotes.
	objectsOK(t, final, changed, next, "hydrate")
	if objectCommand(t, final, "rev-parse", "HEAD") != changed {
		t.Fatal("replay changed checkpoint")
	}
}

func TestSourceObjectsRejectCorruptionAndMissingCompanion(t *testing.T) {
	root := objectRepo(t, "retained object\x00")
	pin := objectCommand(t, root, "rev-parse", "HEAD")
	archive := filepath.Join(t.TempDir(), "objects.tar")
	objectsOK(t, root, pin, archive, "capture")
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(raw, []byte("retained object\x00"))
	if at < 0 {
		t.Fatal("missing test payload")
	}
	raw[at] = 'X'
	corrupt := filepath.Join(t.TempDir(), "corrupt.tar")
	if err := os.WriteFile(corrupt, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := runObjects(root, pin, corrupt, "hydrate"); err == nil || !strings.Contains(string(out), "checksum") {
		t.Fatal("corrupt LFS payload accepted", string(out), err)
	}
	if out, err := runObjects(root, pin, filepath.Join(t.TempDir(), "missing.tar"), "hydrate"); err == nil {
		t.Fatal("LFS checkpoint silently restored as pointers", string(out))
	}
	if out, err := runObjects(root, strings.Repeat("a", 40), archive, "hydrate"); err == nil {
		t.Fatal("wrong root pin accepted", string(out))
	}
}

func TestResolverPinsNestedModulesAndHydratesTheirLFS(t *testing.T) {
	root, _ := nestedObjectRepo(t)
	// Explicitly permit the fixture's local-only module origins. Production
	// preparation does not enable file transport for arbitrary .gitmodules.
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	source, err := (Resolver{Dir: t.TempDir()}).Prepare(context.Background(), workflow.Repository{ID: "app", URL: root})
	if err != nil {
		t.Fatal(err)
	}
	if !source.LFS || len(source.Submodules) != 2 || source.Submodules["vendor/middle module/leaf module"] == "" {
		t.Fatal("nested module paths or LFS inventory lost")
	}
	verifyObjectFiles(t, source.Checkout, "leaf baseline\x00")
}

func TestSourceHydrationResumesAfterProcessDeath(t *testing.T) {
	root, origins := nestedObjectRepo(t)
	pin := objectCommand(t, root, "rev-parse", "HEAD")
	archive := filepath.Join(t.TempDir(), "objects.tar")
	objectsOK(t, root, pin, archive, "capture")
	bundle := filepath.Join(t.TempDir(), "source.bundle")
	objectCommand(t, root, "bundle", "create", bundle, "HEAD")
	for _, origin := range origins {
		if err := os.RemoveAll(origin); err != nil {
			t.Fatal(err)
		}
	}
	program := filepath.Join(t.TempDir(), "objects.py")
	if err := os.WriteFile(program, []byte(objectsProgram), 0600); err != nil {
		t.Fatal(err)
	}
	for _, point := range []string{"lfs-replace", "module-checkout"} {
		t.Run(point, func(t *testing.T) {
			target := cloneObjectBundle(t, bundle, pin)
			fault := `import importlib.util,os,pathlib,sys
sys.dont_write_bytecode=True
spec=importlib.util.spec_from_file_location('source_objects',sys.argv[1]);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
point=sys.argv[5]
replace=m.os.replace
def cut_replace(a,b):
 replace(a,b)
 if point=='lfs-replace' and pathlib.Path(b).name=='data.bin':os._exit(87)
m.os.replace=cut_replace
git=m.git
def cut_git(root,*args,**kwargs):
 result=git(root,*args,**kwargs)
 if point=='module-checkout' and pathlib.Path(root).name=='module-checkout' and args[0]=='checkout':os._exit(88)
 return result
m.git=cut_git
m.hydrate(pathlib.Path(sys.argv[2]),sys.argv[3],pathlib.Path(sys.argv[4]))`
			cmd := exec.Command("python3", "-c", fault, program, target, pin, archive, point)
			if raw, err := cmd.CombinedOutput(); err == nil {
				t.Fatal("fault did not terminate hydrator", string(raw))
			}
			objectsOK(t, target, pin, archive, "hydrate")
			verifyObjectFiles(t, target, "leaf baseline\x00")
		})
	}
}

func TestCheckpointRetainsNewlyAddedSubmoduleAndLFSFile(t *testing.T) {
	root := sourceRepo(t)
	pin := objectCommand(t, root, "rev-parse", "HEAD")
	module := objectRepo(t, "new module data\x00")
	objectCommand(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "--", module, "new module")
	objectCommand(t, filepath.Join(root, "new module"), "lfs", "fetch", "origin", "HEAD")
	objectCommand(t, filepath.Join(root, "new module"), "lfs", "checkout")
	if err := os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.bin filter=lfs -text\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.bin"), []byte("new root LFS\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	changed := objectsOK(t, root, pin, "write", "commit")
	archive := filepath.Join(t.TempDir(), "objects.tar")
	objectsOK(t, root, changed, archive, "capture")
	bundle := filepath.Join(t.TempDir(), "source.bundle")
	objectCommand(t, root, "bundle", "create", bundle, "HEAD")
	for _, origin := range []string{root, module} {
		if err := os.RemoveAll(origin); err != nil {
			t.Fatal(err)
		}
	}
	target := cloneObjectBundle(t, bundle, changed)
	objectsOK(t, target, changed, archive, "hydrate")
	for name, want := range map[string]string{"new.bin": "new root LFS\x00", "new module/data.bin": "new module data\x00"} {
		raw, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(raw) != want {
			t.Fatal("new source object missing", name, err)
		}
	}
	if status := objectCommand(t, target, "status", "--porcelain", "--ignore-submodules=none"); status != "" {
		t.Fatal("restored new objects are dirty", status)
	}
}
