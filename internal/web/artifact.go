package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	stdhtml "html"
	"html/template"
	"strings"
	"unicode/utf8"

	"github.com/sam-bretz/envctl/internal/workflow"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// Artifacts are agent-written, so Markdown renders without raw HTML and with
// goldmark's filtering of dangerous link targets.
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(codeRenderer{}, 999))),
)

const maxRendered = 2 << 20

type RenderedArtifact struct {
	Kind      string   `json:"kind"` // markdown, json, text or binary
	HTML      string   `json:"html,omitempty"`
	Text      string   `json:"text,omitempty"`
	Images    []string `json:"images,omitempty"`
	Size      int      `json:"size"`
	Truncated bool     `json:"truncated,omitempty"`
}

type artifactPageData struct {
	Name      string
	Run       string
	RunID     string
	Revision  string
	Stage     string
	Attempt   string
	Digest    string
	Size      int64
	MediaType string
	Content   RenderedArtifact
	HTML      template.HTML
	// Screenshots are template.URL because html/template rejects a data: URI
	// in a src attribute and writes #ZgotmplZ instead. These are safe to mark:
	// each one is built here from base64 this package decoded itself and
	// confirmed to start with the PNG magic number.
	Screenshots []template.URL
	Others      []workflow.Artifact
	Related     []artifactLink
}

type artifactLink struct {
	workflow.Artifact
	Stage string
}

type artifactMetadataResult struct {
	Artifact workflow.Artifact
	Run      string
	RunID    string
	Revision string
	Stage    string
	Attempt  string
	Others   []workflow.Artifact
	Related  []artifactLink
}

func artifactMetadata(runs []workflow.Run, digest string) artifactMetadataResult {
	for _, run := range runs {
		for _, rev := range run.Revisions {
			order, _ := rev.Config.Workflow.Order()
			seen := make(map[string]bool, len(order))
			visit := func(stage string) (artifactMetadataResult, bool) {
				cp, ok := rev.Checkpoints[stage]
				if !ok {
					return artifactMetadataResult{}, false
				}
				for _, artifact := range cp.Result.Artifacts {
					if artifact.Digest != digest {
						continue
					}
					others := make([]workflow.Artifact, 0, len(cp.Result.Artifacts)-1)
					for _, sibling := range cp.Result.Artifacts {
						if sibling.Digest != artifact.Digest {
							others = append(others, sibling)
						}
					}
					related := make([]artifactLink, 0)
					for _, relatedStage := range order {
						if relatedStage == stage {
							continue
						}
						relatedCP, exists := rev.Checkpoints[relatedStage]
						if !exists {
							continue
						}
						for _, relatedArtifact := range relatedCP.Result.Artifacts {
							if relatedArtifact.Digest != artifact.Digest {
								related = append(related, artifactLink{Artifact: relatedArtifact, Stage: relatedCP.Node})
							}
						}
					}
					return artifactMetadataResult{Artifact: artifact, Run: run.Name, RunID: run.ID, Revision: rev.ID, Stage: cp.Node, Attempt: cp.Attempt, Others: others, Related: related}, true
				}
				return artifactMetadataResult{}, false
			}
			for _, stage := range order {
				seen[stage] = true
				if result, ok := visit(stage); ok {
					return result
				}
			}
			for stage := range rev.Checkpoints {
				if !seen[stage] {
					if result, ok := visit(stage); ok {
						return result
					}
				}
			}
		}
	}
	return artifactMetadataResult{}
}

func renderArtifact(raw []byte, mediaType string) RenderedArtifact {
	out := RenderedArtifact{Size: len(raw)}
	if len(raw) > maxRendered {
		raw, out.Truncated = raw[:maxRendered], true
	}
	var value any
	isJSON := json.Unmarshal(raw, &value) == nil
	switch {
	case strings.Contains(mediaType, "markdown") || looksLikeMarkdown(raw):
		var html bytes.Buffer
		if err := markdown.Convert(raw, &html); err == nil {
			out.Kind, out.HTML = "markdown", html.String()
		} else {
			out.Kind, out.Text = "text", string(raw)
		}
	case isJSON:
		out.Kind = "json"
		value = extractScreenshots(value, &out.Images)
		pretty, _ := json.MarshalIndent(value, "", "  ")
		out.Text = string(pretty)
	case !utf8.Valid(raw):
		out.Kind = "binary"
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
				decoded, err := base64.StdEncoding.DecodeString(s)
				if err == nil && len(decoded) >= 8 && bytes.Equal(decoded[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
					*images = append(*images, "data:image/png;base64,"+s)
					delete(x, k)
				}
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

// codeRenderer adds colors to fenced blocks without asking the browser to
// execute a renderer or fetch a highlighter. The lexer is intentionally small:
// artifact readability benefits from highlighting common identifiers and
// literals, while every token is still escaped before it becomes HTML.
type codeRenderer struct{}

func (codeRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindCodeBlock, renderCodeBlock)
	reg.Register(ast.KindFencedCodeBlock, renderFencedCodeBlock)
}

func renderCodeBlock(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		_, _ = w.WriteString("<pre><code>")
		_, _ = w.WriteString(highlightCode(node.Text(source), ""))
	} else {
		_, _ = w.WriteString("</code></pre>\n")
	}
	return ast.WalkContinue, nil
}

func renderFencedCodeBlock(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.FencedCodeBlock)
	if entering {
		language := string(n.Language(source))
		_, _ = w.WriteString("<pre><code")
		if language != "" {
			_, _ = w.WriteString(` class="language-`)
			_, _ = w.WriteString(stdhtml.EscapeString(language))
			_ = w.WriteByte('"')
		}
		_ = w.WriteByte('>')
		_, _ = w.WriteString(highlightCode(n.Text(source), language))
	} else {
		_, _ = w.WriteString("</code></pre>\n")
	}
	return ast.WalkContinue, nil
}

var keywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"def": true, "defer": true, "else": true, "enum": true, "fallthrough": true,
	"for": true, "func": true, "go": true, "if": true, "import": true,
	"in": true, "interface": true, "let": true, "map": true, "package": true,
	"range": true, "return": true, "select": true, "struct": true, "switch": true,
	"type": true, "var": true, "while": true, "class": true, "function": true,
	"public": true, "private": true, "new": true, "try": true, "catch": true,
}

func highlightCode(raw []byte, language string) string {
	var out strings.Builder
	for i := 0; i < len(raw); {
		start := i
		class := ""
		switch {
		case raw[i] == '#' || (raw[i] == '/' && i+1 < len(raw) && raw[i+1] == '/'):
			class = "comment"
			for i < len(raw) && raw[i] != '\n' {
				i++
			}
		case raw[i] == '\'' || raw[i] == '"' || raw[i] == '`':
			quote := raw[i]
			class = "string"
			i++
			for i < len(raw) {
				if raw[i] == '\\' && quote != '`' && i+1 < len(raw) {
					i += 2
					continue
				}
				i++
				if raw[i-1] == quote {
					break
				}
			}
		case raw[i] >= '0' && raw[i] <= '9':
			class = "number"
			for i < len(raw) && ((raw[i] >= '0' && raw[i] <= '9') || raw[i] == '.' || raw[i] == 'x' || raw[i] == 'A' || raw[i] == 'B' || raw[i] == 'C' || raw[i] == 'D' || raw[i] == 'E' || raw[i] == 'F' || raw[i] == 'a' || raw[i] == 'b' || raw[i] == 'c' || raw[i] == 'd' || raw[i] == 'e' || raw[i] == 'f') {
				i++
			}
		case (raw[i] >= 'A' && raw[i] <= 'Z') || (raw[i] >= 'a' && raw[i] <= 'z') || raw[i] == '_':
			for i < len(raw) && ((raw[i] >= 'A' && raw[i] <= 'Z') || (raw[i] >= 'a' && raw[i] <= 'z') || (raw[i] >= '0' && raw[i] <= '9') || raw[i] == '_') {
				i++
			}
			if keywords[string(raw[start:i])] {
				class = "keyword"
			}
		default:
			i++
		}
		token := stdhtml.EscapeString(string(raw[start:i]))
		if class != "" {
			out.WriteString(`<span class="tok-`)
			out.WriteString(class)
			out.WriteString(`">`)
		}
		out.WriteString(token)
		if class != "" {
			out.WriteString("</span>")
		}
	}
	return out.String()
}
