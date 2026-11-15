package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"bankcmp/internal/model"
	"bankcmp/internal/plan"
	"bankcmp/internal/source"
)

// Text primary keys with mixed case and punctuation are the classic case a
// broken collation assumption gets wrong.  Every key must be found, including
// ones that sort before/after the sampled boundaries.
func TestTextKeysMixedCase(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE codes (code TEXT PRIMARY KEY, v INTEGER)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	keys := []string{"A1", "a1", "A01", "B", "a-1", "Zz", "中1", "中2", "~", "0", "00"}
	for i, k := range keys {
		mustExec(t, src, `INSERT INTO codes VALUES (?,?)`, k, i)
		mustExec(t, tgt, `INSERT INTO codes VALUES (?,?)`, k, i)
	}
	// Plant: change a1, delete 中1, add a new key that sorts low.
	mustExec(t, tgt, `UPDATE codes SET v=999 WHERE code='a1'`)
	mustExec(t, tgt, `DELETE FROM codes WHERE code='中1'`)
	mustExec(t, tgt, `INSERT INTO codes VALUES ('!new', 7)`)

	tp := planFor(t, src, tgt, "codes")
	res, err := NewRunner(src, tgt, Options{ChunkRows: 2, ChunkWorkers: 2, Recheck: false, LeafRows: 4}).
		RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range res.Diffs {
		got[d.PK] = string(d.Kind)
	}
	want := map[string]string{"a1": "modified", "中1": "missing", "!new": "extra"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("pk %q: got %q want %q", k, got[k], v)
		}
	}
}

// A single row in a huge key space, and a table where both sides are empty.
func TestTinyAndEmptyTables(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE one (id INTEGER PRIMARY KEY, v TEXT)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	tp := planFor(t, src, tgt, "one")
	res, err := NewRunner(src, tgt, Options{ChunkRows: 1000}).RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != model.TableEqual || res.SourceRows != 0 {
		t.Fatalf("empty table: status=%s rows=%d", res.Status, res.SourceRows)
	}
	mustExec(t, src, `INSERT INTO one VALUES (1, 'x')`)
	res, err = NewRunner(src, tgt, Options{ChunkRows: 1000}).RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diffs) != 1 || res.Diffs[0].PK != "1" || res.Diffs[0].Kind != model.KindMissing {
		t.Fatalf("single row: %+v", res.Diffs)
	}
}

// Every row differs: the range must split until each difference is enumerated
// without losing or duplicating any.
func TestEveryRowDiffers(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE allbad (id INTEGER PRIMARY KEY, v TEXT)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	const n = 500
	for i := 0; i < n; i++ {
		mustExec(t, src, `INSERT INTO allbad VALUES (?,?)`, i, "src")
		mustExec(t, tgt, `INSERT INTO allbad VALUES (?,?)`, i, "tgt")
	}
	tp := planFor(t, src, tgt, "allbad")
	res, err := NewRunner(src, tgt, Options{ChunkRows: 50, ChunkWorkers: 2, Recheck: false,
		LeafRows: 10, DiffMaxRows: 20, MaxSplitDepth: 30}).RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diffs) != n {
		t.Fatalf("got %d diffs, want %d", len(res.Diffs), n)
	}
	seen := map[string]bool{}
	for _, d := range res.Diffs {
		if seen[d.PK] {
			t.Fatalf("pk %s reported twice", d.PK)
		}
		seen[d.PK] = true
		if d.Kind != model.KindModified {
			t.Fatalf("pk %s kind %s", d.PK, d.Kind)
		}
	}
}

// Wide numeric keys (bigint and numeric) must split without losing boundary
// rows.
func TestNumericKeys(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE money (id NUMERIC(30,6), v TEXT, PRIMARY KEY (id))`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	vals := []string{"-99999999999999.123456", "-1.000001", "0", "0.000001", "1", "99999999999.5", "1000000000000.000001"}
	for _, v := range vals {
		mustExec(t, src, `INSERT INTO money VALUES (?,?)`, v, v)
		mustExec(t, tgt, `INSERT INTO money VALUES (?,?)`, v, v)
	}
	mustExec(t, tgt, `DELETE FROM money WHERE id='0'`)
	mustExec(t, tgt, `UPDATE money SET v='changed' WHERE id='1'`)
	tp := planFor(t, src, tgt, "money")
	res, err := NewRunner(src, tgt, Options{ChunkRows: 2, ChunkWorkers: 2, Recheck: false, LeafRows: 2}).
		RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range res.Diffs {
		got[d.PK] = string(d.Kind)
	}
	if len(got) != 2 || got["0"] != "missing" || got["1"] != "modified" {
		t.Fatalf("numeric keys: %v", got)
	}
}

// A digest column of NULLs on both sides must compare equal, not error.
func TestDigestNulls(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE dn (id INTEGER PRIMARY KEY, memo TEXT, blob BLOB)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	for i := 0; i < 20; i++ {
		mustExec(t, src, `INSERT INTO dn VALUES (?,NULL,NULL)`, i)
		mustExec(t, tgt, `INSERT INTO dn VALUES (?,NULL,NULL)`, i)
	}
	spec := model.TableSpec{Name: "dn", Columns: []model.ColumnSpec{
		{Name: "memo", Type: model.TypeString, Digest: true},
		{Name: "blob", Type: model.TypeBytes, Digest: true},
	}}
	tp := planForSpec(t, src, tgt, spec)
	res, err := NewRunner(src, tgt, Options{ChunkRows: 5}).RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diffs) != 0 {
		t.Fatalf("NULL digests must compare equal: %+v", res.Diffs)
	}
	// And a NULL vs a value must differ.
	mustExec(t, tgt, `UPDATE dn SET memo='x' WHERE id=3`)
	res, err = NewRunner(src, tgt, Options{ChunkRows: 5, Recheck: false}).RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diffs) != 1 || res.Diffs[0].PK != "3" {
		t.Fatalf("NULL vs value: %+v", res.Diffs)
	}
}

// Password-like columns are masked in the difference preview but still
// reported as differing.
func TestSensitiveMaskedInPreview(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE users (id INTEGER PRIMARY KEY, password TEXT, name TEXT)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	mustExec(t, src, `INSERT INTO users VALUES (1,'hunter2','ann')`)
	mustExec(t, tgt, `INSERT INTO users VALUES (1,'hunter3','ann')`)
	tp := planFor(t, src, tgt, "users")
	res, err := NewRunner(src, tgt, Options{ChunkRows: 10, Recheck: false}).RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diffs) != 1 {
		t.Fatalf("expected one diff, got %d", len(res.Diffs))
	}
	d := res.Diffs[0]
	if d.SourceRow["password"] != "******" || d.TargetRow["password"] != "******" {
		t.Fatalf("password not masked: %+v", d.SourceRow)
	}
	for _, c := range d.Columns {
		if c.Column == "password" && (strings.Contains(c.Source, "hunter") || strings.Contains(c.Target, "hunter")) {
			t.Fatalf("password leaked in column diff: %+v", c)
		}
	}
	if d.Columns[0].Column != "password" {
		t.Fatalf("expected password column diff, got %+v", d.Columns)
	}
}

// A key column that is nullable must be rejected at plan time, because NULL
// ordering differs between engines.
func TestNullableKeyRejected(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE nk (a INTEGER, v TEXT, PRIMARY KEY (a))`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	ss, _ := src.Introspect(context.Background(), "nk")
	ts, _ := tgt.Introspect(context.Background(), "nk")
	ss.Columns[0].Nullable = true
	if _, err := planBuild(t, src, tgt, ss, ts, model.TableSpec{Name: "nk"}); err == nil {
		t.Fatal("nullable key must be rejected")
	}
	_ = fmt.Sprint
}

func planBuild(t *testing.T, src, tgt *source.Database, ss, ts *source.TableSchema, spec model.TableSpec) (*plan.TablePlan, error) {
	t.Helper()
	return plan.Build(spec, src, tgt, ss, ts, model.Options{ChunkRows: 100})
}
