package verify

import "testing"

func TestCheckExact(t *testing.T) {
	m := &Manifest{Tables: []TableManifest{{Name: "t", Planted: []Planted{
		{PK: "1", Kind: "missing"}, {PK: "2", Kind: "modified", Column: "a"},
	}}}}
	ok := Check(m, []Result{{Table: "t", PK: "1", Kind: "missing"}, {Table: "t", PK: "2", Kind: "modified", Columns: []string{"a"}}})
	if !ok.OK {
		t.Fatalf("expected ok: %s", ok.String())
	}
	bad := Check(m, []Result{{Table: "t", PK: "1", Kind: "missing"}, {Table: "t", PK: "3", Kind: "extra"}})
	if bad.OK {
		t.Fatal("expected failure")
	}
	if len(bad.Missing) != 1 || len(bad.Unexpected) != 1 {
		t.Fatalf("missing=%v unexpected=%v", bad.Missing, bad.Unexpected)
	}
}
