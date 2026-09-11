package repository

import "testing"

func TestGuestAssignmentPathsAreRevisionScoped(t *testing.T) {
	g := Guest{Revision: "rev_example"}
	a, err := g.WorktreeDir("attempt_left", "app")
	if err != nil {
		t.Fatal(err)
	}
	b, err := g.WorktreeDir("attempt_right", "app")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("writers share a worktree")
	}
	other := Guest{Revision: "rev_other"}
	c, err := other.WorktreeDir("attempt_left", "app")
	if err != nil || c == a {
		t.Fatal("revisions share mutable files", err)
	}
	for _, invalid := range []string{"../escape", "/host", "attempt;echo bad", "a/b", ""} {
		if _, err = g.WorktreeDir(invalid, "app"); err == nil {
			t.Fatalf("accepted invalid assignment: %q", invalid)
		}
		if _, err = g.SourceDir(invalid); err == nil {
			t.Fatalf("accepted invalid repository: %q", invalid)
		}
		if _, err = (Guest{Revision: invalid}).SourceDir("app"); err == nil {
			t.Fatalf("accepted invalid revision: %q", invalid)
		}
	}
}
