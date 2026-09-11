package workflow

import (
	"errors"
	"regexp"
	"strings"
)

// Preview opts one guest service into a host preview. The coordinator
// forwards a host loopback port to that service's guest-loopback binding; no
// other guest port is exposed and provider auto-forwarding stays disabled.
type Preview struct {
	Service string `yaml:"service" json:"service"`
	Port    int    `yaml:"port" json:"port"`
	Path    string `yaml:"path,omitempty" json:"path,omitempty"`
	// Node selects a parallel branch's child runtime. Empty, or a node that
	// has no child runtime, previews the revision runtime.
	Node string `yaml:"node,omitempty" json:"node,omitempty"`
}

var composeService = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

func (p *Preview) validate(d Definition) error {
	if p == nil {
		return nil
	}
	if !composeService.MatchString(p.Service) || p.Port < 1 || p.Port > 65535 {
		return errors.New("preview requires a Compose service name and a container port between 1 and 65535")
	}
	if p.Path != "" && (!strings.HasPrefix(p.Path, "/") || len(p.Path) > 256 || strings.ContainsAny(p.Path, " \t\r\n\x00#")) {
		return errors.New("preview path must be an absolute URL path without spaces or fragments")
	}
	if p.Node != "" {
		if _, ok := d.Nodes[p.Node]; !ok {
			return errors.New("preview node must name a workflow node")
		}
	}
	return nil
}

// RequestPath is the configured request path, defaulting to the root.
func (p Preview) RequestPath() string {
	if p.Path == "" {
		return "/"
	}
	return p.Path
}
