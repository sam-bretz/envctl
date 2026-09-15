package tracker

import (
	"fmt"
	"regexp"
	"strings"
)

var linearIdentifier = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)
var linearURL = regexp.MustCompile(`^https://linear\.app/[^/]+/issue/([A-Z][A-Z0-9]*-[0-9]+)(?:/|$)`)

// ParseRef accepts a bare Linear identifier (ENG-123) or a linear.app issue
// URL and returns the identifier Linear's GraphQL issue(id:) query accepts.
//
// Run.TaskRef stays an opaque, unvalidated string at creation time; parsing
// its Linear-specific shape happens only here, at prober-probe and delivery
// time, so a malformed or non-Linear ref surfaces as a Plan readiness
// failure rather than a run-create rejection, and --task-ref stays
// provider-agnostic for future trackers with different reference formats.
func ParseRef(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if linearIdentifier.MatchString(raw) {
		return raw, nil
	}
	if m := linearURL.FindStringSubmatch(raw); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("task ref %q is not a Linear issue URL or identifier", raw)
}
