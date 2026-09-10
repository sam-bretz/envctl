package ports

import (
	"path/filepath"
	"testing"
)

func TestAllocateStableAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ports.json")
	r, err := Open(path, 47000, 47010)
	if err != nil {
		t.Fatal(err)
	}
	k := Key{Project: "mg-a", Service: "postgres", Target: 5432}
	p1, err := r.Get(k)
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := r.Get(k)
	if p1 != p2 {
		t.Fatalf("allocation not stable: %d vs %d", p1, p2)
	}
	other, _ := r.Get(Key{Project: "mg-b", Service: "postgres", Target: 5432})
	if other == p1 {
		t.Fatalf("two projects share port %d", p1)
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(path, 47000, 47010)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r2.Get(k); got != p1 {
		t.Fatalf("persisted allocation lost: %d vs %d", got, p1)
	}
	r2.Release("mg-a")
	if _, ok := r2.Alloc[k.String()]; ok {
		t.Fatal("release did not drop allocation")
	}
	if _, ok := r2.Alloc["mg-b/postgres/5432"]; !ok {
		t.Fatal("release dropped another project's allocation")
	}
}
