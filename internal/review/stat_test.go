package review

import "testing"

func TestStatCountsChangedFilesAndLinesAcrossRepositories(t *testing.T) {
	c := Comparison{Repositories: []RepositoryDiff{
		{Patch: "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,2 +1,3 @@\n keep\n-old\n+new\n+added\n"},
		{Patch: "diff --git a/y.go b/y.go\n--- a/y.go\n+++ b/y.go\n@@ -1 +1 @@\n-gone\n"},
		{Unavailable: "source bundle missing"},
	}}
	// The --- and +++ file headers are not lines of change.
	if files, added, removed := Stat(c); files != 2 || added != 2 || removed != 2 {
		t.Fatalf("got %d files +%d -%d, want 2 files +2 -2", files, added, removed)
	}
}
