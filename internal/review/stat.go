package review

import "strings"

// Stat counts what a comparison changed: files touched, lines added and lines
// removed, across every repository. It is what a side-by-side view of several
// variations can show without printing each whole patch.
func Stat(c Comparison) (files, added, removed int) {
	for _, repo := range c.Repositories {
		for _, line := range strings.Split(repo.Patch, "\n") {
			switch {
			case strings.HasPrefix(line, "diff --git "):
				files++
			case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			case strings.HasPrefix(line, "+"):
				added++
			case strings.HasPrefix(line, "-"):
				removed++
			}
		}
	}
	return files, added, removed
}
