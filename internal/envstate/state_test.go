package envstate

import (
	"os"
	"testing"
)

func TestUpsertListRemove(t *testing.T) {
	root := t.TempDir()
	if _, err := Load(root, "a"); !os.IsNotExist(err) {
		t.Fatalf("expected not-exist, got %v", err)
	}
	s, err := Upsert(root, "a", "local", func(s *State) { s.Branch = "feat/a" })
	if err != nil {
		t.Fatal(err)
	}
	if s.CreatedAt.IsZero() || s.Branch != "feat/a" {
		t.Fatalf("bad state %+v", s)
	}
	created := s.CreatedAt
	s2, err := Upsert(root, "a", "local", func(s *State) { s.Kept = true })
	if err != nil {
		t.Fatal(err)
	}
	if !s2.CreatedAt.Equal(created) || s2.Branch != "feat/a" || !s2.Kept {
		t.Fatalf("upsert lost fields: %+v", s2)
	}
	if _, err := Upsert(root, "b", "local", nil); err != nil {
		t.Fatal(err)
	}
	all, err := List(root)
	if err != nil || len(all) != 2 || all[0].Name != "a" || all[1].Name != "b" {
		t.Fatalf("list: %v %+v", err, all)
	}
	if err := Remove(root, "a"); err != nil {
		t.Fatal(err)
	}
	if all, _ := List(root); len(all) != 1 {
		t.Fatalf("remove did not drop a: %+v", all)
	}
}
