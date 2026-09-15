package web

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// Artifacts are agent-written, so Markdown renders without raw HTML and with
// goldmark's filtering of dangerous link targets.
var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

const maxRendered = 2 << 20

type RenderedArtifact struct {
	Kind      string   `json:"kind"` // markdown, json, text or binary
	HTML      string   `json:"html,omitempty"`
	Text      string   `json:"text,omitempty"`
	Images    []string `json:"images,omitempty"`
	Size      int      `json:"size"`
	Truncated bool     `json:"truncated,omitempty"`
}

func renderArtifact(raw []byte, mediaType string) RenderedArtifact {
	out := RenderedArtifact{Size: len(raw)}
	if len(raw) > maxRendered {
		raw, out.Truncated = raw[:maxRendered], true
	}
	var value any
	switch {
	case json.Unmarshal(raw, &value) == nil:
		out.Kind = "json"
		value = extractScreenshots(value, &out.Images)
		pretty, _ := json.MarshalIndent(value, "", "  ")
		out.Text = string(pretty)
	case !utf8.Valid(raw):
		out.Kind = "binary"
	case strings.Contains(mediaType, "markdown") || looksLikeMarkdown(raw):
		var html bytes.Buffer
		if err := markdown.Convert(raw, &html); err == nil {
			out.Kind, out.HTML = "markdown", html.String()
		} else {
			out.Kind, out.Text = "text", string(raw)
		}
	default:
		out.Kind, out.Text = "text", string(raw)
	}
	return out
}

func looksLikeMarkdown(raw []byte) bool {
	s := string(raw)
	return strings.HasPrefix(s, "# ") || strings.Contains(s, "\n## ") || strings.Contains(s, "\n```")
}

// extractScreenshots moves browser screenshots out of the JSON text into
// images the page can show.
func extractScreenshots(v any, images *[]string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, item := range x {
			if s, ok := item.(string); ok && k == "screenshot_png" {
				*images = append(*images, "data:image/png;base64,"+s)
				x[k] = "[screenshot shown above]"
				continue
			}
			x[k] = extractScreenshots(item, images)
		}
	case []any:
		for i := range x {
			x[i] = extractScreenshots(x[i], images)
		}
	}
	return v
}
