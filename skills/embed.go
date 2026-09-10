// Package skills embeds the agent skill so the binary can install it into a
// repository without a network fetch. The directory layout also matches what
// `npx skills add <owner>/envctl` expects.
package skills

import "embed"

// FS holds skills/<name>/*.
//
//go:embed envctl/*.md
var FS embed.FS

// Name is the skill directory name.
const Name = "envctl"
