package tui

import (
	"fmt"
	"strings"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// runtimeSummary shows the preview (host loopback) apart from service
// endpoints, which are guest-loopback addresses reachable only inside the VM.
func runtimeSummary(rt workflow.RuntimeState) string {
	ready := "not ready"
	if rt.Ready {
		ready = "ready"
	}
	lines := []string{fmt.Sprintf("Runtime %s · %s · %s", rt.ID, rt.State, ready)}
	if rt.PreviewURL != "" {
		lines = append(lines, "Preview (this machine): "+rt.PreviewURL)
	}
	if len(rt.Services) > 0 {
		lines = append(lines, "", "Services (endpoints are inside the VM):")
		for _, s := range rt.Services {
			line := fmt.Sprintf("  %-16s %-18s", s.Name, s.State)
			if s.URL != "" {
				line += " " + s.URL
			}
			lines = append(lines, strings.TrimRight(line, " "))
		}
	}
	return strings.Join(lines, "\n") + "\n\n" + pretty(rt)
}
