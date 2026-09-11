package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

const previewBase = "version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"

func TestPreviewConfigurationIsOptInAndStrict(t *testing.T) {
	plain, err := Parse([]byte(previewBase))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plain)
	if err != nil || strings.Contains(string(raw), "preview") {
		t.Fatalf("absent preview serialized into configuration identity: %s", raw)
	}
	c, err := Parse([]byte(previewBase + "preview: {service: web, port: 8080, path: /health}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Preview == nil || c.Preview.Service != "web" || c.Preview.Port != 8080 || c.Preview.RequestPath() != "/health" {
		t.Fatalf("preview not parsed: %+v", c.Preview)
	}
	if Digest(c) == Digest(plain) {
		t.Fatal("preview did not change the configuration identity")
	}
	if (Preview{Service: "web", Port: 1}).RequestPath() != "/" {
		t.Fatal("default preview path is not the root")
	}
	if _, err = Parse([]byte(previewBase + "preview: {service: web, port: 8080, node: code}\n")); err != nil {
		t.Fatal("node-scoped preview rejected", err)
	}
	for _, bad := range []string{
		"{service: web}",
		"{service: web, port: 0}",
		"{service: web, port: 70000}",
		"{service: '-web', port: 80}",
		"{service: web, port: 80, path: health}",
		"{service: web, port: 80, path: '/a b'}",
		"{service: web, port: 80, node: missing}",
		"{service: web, port: 80, host: 0.0.0.0}",
	} {
		if _, err := Parse([]byte(previewBase + "preview: " + bad + "\n")); err == nil {
			t.Fatalf("invalid preview %s accepted", bad)
		}
	}
}
