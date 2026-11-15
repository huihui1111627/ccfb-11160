package engine

import (
	"context"
	"fmt"
	"testing"

	"bankcmp/internal/model"
	"bankcmp/internal/plan"
	"bankcmp/internal/source"
)

func buildPair(t *testing.T) (src, tgt *source.Database) {
	t.Helper()
	dir := t.TempDir()
	mk := func(name string) *source.Database {
		db, err := source.Open(context.Background(), source.Config{
			Driver: model.DriverSQLite, DSN: dir + "/" + name,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	return mk("src.db"), mk("tgt.db")
}

func mustExec(t *testing.T, db *source.Database, q string, args ...any) {
	t.Helper()
	if err := db.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func TestCompareEqualTables(t *testing.T) {
	src, tgt := buildPair(t)
	srcSchema := `CREATE TABLE accounts (id INTEGER PRIMARY KEY, name TEXT, balance NUMERIC(18,4), updated DATETIME)`
	tgtSchema := `CREATE TABLE accounts (id INTEGER PRIMARY KEY, name TEXT, balance NUMERIC(18,4), updated DATETIME)`
	mustExec(t, src, srcSchema)
	mustExec(t, tgt, tgtSchema)
	for i := 0; i < 500; i++ {
		q := `INSERT INTO accounts VALUES (?,?,?,?)`
		mustExec(t, src, q, i, fmt.Sprintf("name-%d", i), fmt.Sprintf("%d.25", i*10), "2024-01-02 03:04:05")
		mustExec(t, tgt, q, i, fmt.Sprintf("name-%d", i), fmt.Sprintf("%d.2500", i*10), "2024-01-02 03:04:05")
	}
	tp := planFor(t, src, tgt, "accounts")
	res, err := NewRunner(src, tgt, Options{ChunkRows: 100, ChunkWorkers: 2}).RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != model.TableEqual {
		t.Fatalf("expected equal, got %s with %d diffs: %+v", res.Status, len(res.Diffs), res.Diffs[:min(2, len(res.Diffs))])
	}
	if res.SourceRows != 500 || res.TargetRows != 500 {
		t.Fatalf("rows: %d/%d", res.SourceRows, res.TargetRows)
	}
}

func TestCompareDetectsExactDifferences(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER, s TEXT)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	for i := 0; i < 200; i++ {
		q := `INSERT INTO t VALUES (?,?,?)`
		mustExec(t, src, q, i, i, fmt.Sprintf("s%d", i))
		mustExec(t, tgt, q, i, i, fmt.Sprintf("s%d", i))
	}
	for _, id := range []int{5, 77, 150} {
		mustExec(t, tgt, `DELETE FROM t WHERE id=?`, id)
	}
	mustExec(t, tgt, `INSERT INTO t VALUES (1000, 1, 'extra')`)
	mustExec(t, tgt, `INSERT INTO t VALUES (1001, 2, 'extra2')`)
	for _, id := range []int{10, 11, 99, 199} {
		mustExec(t, tgt, `UPDATE t SET v=? WHERE id=?`, 999, id)
	}
	tp := planFor(t, src, tgt, "t")
	res, err := NewRunner(src, tgt, Options{
		ChunkRows: 16, ChunkWorkers: 2, LeafRows: 64, MaxSplitDepth: 20, Recheck: false,
	}).RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]model.DiffKind{}
	for _, d := range res.Diffs {
		got[d.PK] = d.Kind
	}
	want := map[string]model.DiffKind{
		"5": model.KindMissing, "77": model.KindMissing, "150": model.KindMissing,
		"1000": model.KindExtra, "1001": model.KindExtra,
		"10": model.KindModified, "11": model.KindModified, "99": model.KindModified, "199": model.KindModified,
	}
	if len(got) != len(want) {
		t.Fatalf("diff count = %d, want %d\ngot=%v", len(got), len(want), got)
	}
	for pk, kind := range want {
		if got[pk] != kind {
			t.Errorf("pk %s: got %q want %q", pk, got[pk], kind)
		}
	}
	for _, d := range res.Diffs {
		if d.Kind == model.KindModified && len(d.Columns) == 0 {
			t.Errorf("modified row %s has no column details", d.PK)
		}
	}
}

func TestOneSidedRows(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	for i := 0; i < 50; i++ {
		mustExec(t, tgt, `INSERT INTO t VALUES (?,?)`, i, i)
	}
	tp := planFor(t, src, tgt, "t")
	res, err := NewRunner(src, tgt, Options{ChunkRows: 5, LeafRows: 8, MaxSplitDepth: 30}).RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diffs) != 50 {
		t.Fatalf("got %d extras, want 50", len(res.Diffs))
	}
	for _, d := range res.Diffs {
		if d.Kind != model.KindExtra {
			t.Fatalf("kind=%s", d.Kind)
		}
	}
}

func TestGBKSourceComparesEqual(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE c (id INTEGER PRIMARY KEY, name TEXT)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	// GBK bytes for 张三 / 李四
	gbk := func(s string) string {
		m := map[string]string{"张三": "\xd5\xc5\xc8\xfd", "李四": "\xc0\xee\xcb\xc4"}
		return m[s]
	}
	for i, name := range []string{"张三", "李四"} {
		mustExec(t, src, `INSERT INTO c VALUES (?,?)`, i, gbk(name))
		mustExec(t, tgt, `INSERT INTO c VALUES (?,?)`, i, name)
	}
	tp := planForCharset(t, src, tgt, "c", "gbk")
	res, err := NewRunner(src, tgt, Options{ChunkRows: 10}).RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != model.TableEqual {
		t.Fatalf("GBK source should compare equal, got %d diffs: %+v", len(res.Diffs), res.Diffs)
	}
}

func planFor(t *testing.T, src, tgt *source.Database, table string) *plan.TablePlan {
	return planForSpec(t, src, tgt, model.TableSpec{Name: table})
}

func planForCharset(t *testing.T, src, tgt *source.Database, table, cs string) *plan.TablePlan {
	src.Charset = cs
	return planFor(t, src, tgt, table)
}

func planForSpec(t *testing.T, src, tgt *source.Database, spec model.TableSpec) *plan.TablePlan {
	t.Helper()
	ctx := context.Background()
	ss, err := src.Introspect(ctx, spec.Source())
	if err != nil {
		t.Fatal(err)
	}
	ts, err := tgt.Introspect(ctx, spec.Target())
	if err != nil {
		t.Fatal(err)
	}
	tp, err := plan.Build(spec, src, tgt, ss, ts, model.Options{ChunkRows: 100, DiffMaxRows: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return tp
}
