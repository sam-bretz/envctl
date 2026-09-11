package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStallWindowDefaultsOverridesAndValidation(t *testing.T) {
	plain, err := Parse([]byte(limitsBase + "workflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if plain.StallSeconds("code") != DefaultStallSeconds {
		t.Fatal("no default stall window")
	}
	raw, _ := json.Marshal(plain.Limits)
	if strings.Contains(string(raw), "stall") {
		t.Fatalf("an unset stall window entered the configuration identity: %s", raw)
	}
	c, err := Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nlimits: {max_attempts: 5, attempt_seconds: 600, stall_seconds: 300}\nworkflow: {template: feature, nodes: {qa: {limits: {stall_seconds: 1800}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.StallSeconds("code") != 300 || c.StallSeconds("qa") != 1800 || c.NodeLimits("qa").StallSeconds != 1800 {
		t.Fatal("stall window overrides not applied", c.StallSeconds("code"), c.StallSeconds("qa"))
	}
	if Digest(c) == Digest(plain) {
		t.Fatal("a stall window did not change the configuration identity")
	}
	for _, bad := range []string{
		"limits: {stall_seconds: 30}\nworkflow: {template: feature}\n",
		"limits: {stall_seconds: 86401}\nworkflow: {template: feature}\n",
		"workflow: {template: feature, nodes: {qa: {limits: {stall_seconds: -1}}}}\n",
		"workflow: {template: feature, nodes: {qa: {limits: {stall_seconds: 59}}}}\n",
	} {
		if _, err := Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\n" + bad)); err == nil {
			t.Fatalf("invalid stall window accepted: %s", bad)
		}
	}
}
