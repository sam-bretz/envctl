package dockerx

import "testing"

func TestParsePSHandlesArrayAndNDJSON(t *testing.T) {
	array := `[{"Name":"mg-a-postgres-1","Service":"postgres","State":"running","Health":"healthy","Publishers":[{"URL":"127.0.0.1","TargetPort":5432,"PublishedPort":41000,"Protocol":"tcp"}]}]`
	nd := `{"Name":"mg-a-postgres-1","Service":"postgres","State":"running","Health":"healthy","Publishers":[]}
{"Name":"mg-a-api-1","Service":"api","State":"exited","Health":"","Publishers":[]}`
	a, err := parsePS(array)
	if err != nil || len(a) != 1 || a[0].Publishers[0].PublishedPort != 41000 {
		t.Fatalf("array parse: %v %+v", err, a)
	}
	n, err := parsePS(nd)
	if err != nil || len(n) != 2 || n[1].Service != "api" {
		t.Fatalf("ndjson parse: %v %+v", err, n)
	}
	if e, err := parsePS("  "); err != nil || e != nil {
		t.Fatalf("empty parse: %v %+v", err, e)
	}
}
