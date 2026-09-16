package tui

import (
	"errors"
	"os/exec"
	"runtime"
)

// OpenInBrowser hands a URL to the platform's opener. Waiting for the opener
// command to report failure is necessary so callers can show a terminal
// fallback when an SSH session has no browser integration.
func OpenInBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux", "freebsd", "openbsd", "netbsd":
		cmd = exec.Command("xdg-open", url)
	default:
		return errors.New("no browser opener for " + runtime.GOOS)
	}
	return cmd.Run()
}
