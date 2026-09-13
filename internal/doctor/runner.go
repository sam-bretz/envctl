package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// ExecRunner is the production Runner: it runs name with args under a
// context bounded by timeout and captures its output. A deadline exceeded
// while the process is running is reported as a distinct timeout error
// regardless of the process's own exit state; a normal non-zero exit passes
// its code through with a nil error.
func ExecRunner(ctx context.Context, timeout time.Duration, name string, args ...string) (stdout, stderr string, exitCode int, err error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, name, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	// A killed child can leave grandchildren holding the stdout/stderr pipes
	// open (e.g. a shell that forked instead of exec'ing); WaitDelay bounds
	// how long Wait keeps reading before it force-closes them, so a hung
	// tool's timeout is honored in wall-clock time too.
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	stdout, stderr = outBuf.String(), errBuf.String()

	if runCtx.Err() == context.DeadlineExceeded {
		return stdout, stderr, -1, fmt.Errorf("%s did not respond within %s", name, timeout)
	}
	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return stdout, stderr, exitErr.ExitCode(), nil
	}
	return stdout, stderr, -1, runErr
}
