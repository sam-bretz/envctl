package feature

import "testing"

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"feat/imported-traffic-pricing":   "feat-imported-traffic-pricing",
		"Fix/All_Environments Unfiltered": "fix-all-environments-unfiltered",
		"worktree-jiggly-waddling-planet": "worktree-jiggly-waddling-planet",
		"///":                             "default",
		"a-very-long-branch-name-that-keeps-going-and-going-forever-and-ever": "a-very-long-branch-name-that-keeps-going",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProjectName(t *testing.T) {
	if got := ProjectName("mg", "feat-x"); got != "mg-feat-x" {
		t.Fatalf("got %q", got)
	}
}
