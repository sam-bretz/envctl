package tui

import (
	"errors"
	"os/exec"
	"runtime"
)

// OpenInBrowser hands a URL to the platform's opener without waiting for the
// browser, so the dashboard stays responsive.
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
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
