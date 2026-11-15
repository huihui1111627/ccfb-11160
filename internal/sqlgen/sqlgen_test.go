package sqlgen

import (
	"context"
	"fmt"
	"testing"

	"bankcmp/internal/canon"
	"bankcmp/internal/dialect"
	"bankcmp/internal/model"
	"bankcmp/internal/plan"
	"bankcmp/internal/source"
)

func pair(t *testing.T) (*source.Database, *source.Database, *plan.TablePlan) {
	t.Helper()
	dir := t.TempDir()
	open := func(name string) *source.Database {
		db, err := source.Open(context.Background(), source.Config{Driver: model.DriverSQLite, DSN: dir + "/" + name})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	src, tgt := open("s.db"), open("t.db")
	for _, db := range []*source.Database{src, tgt} {
		if err := db.Exec(context.Background(),
			`CREATE TABLE m (id INTEGER PRIMARY KEY, amount NUMERIC(18,2), note TEXT, bin BLOB)`); err != nil {
			t.Fatal(err)
		}
	}
	ss, _ := src.Introspect(context.Background(), "m")
	ts, _ := tgt.Introspect(context.Background(), "m")
	tp, err := plan.Build(model.TableSpec{Name: "m"}, src, tgt, ss, ts, model.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return src, tgt, tp
}

// rowFor reads one row through the same queries the engine uses, returning a
// complete SourceRow (never a display preview).
func rowFor(t *testing.T, tp *plan.TablePlan, db *source.Database, id int64) SourceRow {
	return rowForTable(t, tp, db, "m", id)
}

func rowForTable(t *testing.T, tp *plan.TablePlan, db *source.Database, table string, id int64) SourceRow {
	t.Helper()
	ctx := context.Background()
	cols := []string{`"id"`}
	for _, c := range tp.Columns {
		cols = append(cols, `"`+c.TargetName+`"`)
	}
	q := "SELECT " + joinStrings(cols) + fmt.Sprintf(` FROM "%s" WHERE "id"=%d`, table, id)
	row := db.DB.QueryRowContext(ctx, q)
	raws := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raws {
		ptrs[i] = &raws[i]
	}
	if err := row.Scan(ptrs...); err != nil {
		t.Fatal(err)
	}
	out := SourceRow{Key: []plan.KeyVal{{Type: model.TypeInt, Int: id}}}
	for i, c := range tp.Columns {
		v, err := c.TgtNorm.Normalize(canon.FromDriver(raws[i+1]))
		if err != nil {
			t.Fatal(err)
		}
		out.Vals = append(out.Vals, v)
	}
	return out
}

func joinStrings(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}

func TestInsertDeleteUpdateRoundTrip(t *testing.T) {
	src, tgt, tp := pair(t)
	ctx := context.Background()
	_ = src.Exec(ctx, `INSERT INTO m VALUES (1, 10.50, 'a', X'01')`)
	_ = src.Exec(ctx, `INSERT INTO m VALUES (2, 20.00, 'b', X'02')`)
	_ = tgt.Exec(ctx, `INSERT INTO m VALUES (2, 20.00, 'B', X'02')`)
	_ = tgt.Exec(ctx, `INSERT INTO m VALUES (3, 30.00, 'c', X'03')`)

	diffs := []model.Diff{
		{TableName: "m", Kind: model.KindMissing, Stability: model.StabilityStable, PK: "1",
			PKValues: []model.KeyValue{{Column: "id", Value: "1", Type: model.TypeInt}}},
		{TableName: "m", Kind: model.KindExtra, Stability: model.StabilityStable, PK: "3",
			PKValues: []model.KeyValue{{Column: "id", Value: "3", Type: model.TypeInt}}},
		{TableName: "m", Kind: model.KindModified, Stability: model.StabilityStable, PK: "2",
			PKValues: []model.KeyValue{{Column: "id", Value: "2", Type: model.TypeInt}}},
	}
	in := Input{
		SourceRows: map[string]SourceRow{"1": rowFor(t, tp, src, 1), "2": rowFor(t, tp, src, 2)},
		TargetRows: map[string]SourceRow{"2": rowFor(t, tp, tgt, 2), "3": rowFor(t, tp, tgt, 3)},
	}
	b, err := Generate(tp, dialect.SQLite{}, diffs, in, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if b.Statements != 3 {
		t.Fatalf("statements=%d sql=%s", b.Statements, b.SQL)
	}
	for _, stmt := range splitOnSemicolon(b.SQL) {
		if err := tgt.Exec(ctx, stmt); err != nil {
			t.Fatalf("apply %q: %v", stmt, err)
		}
	}
	assertCount(t, tgt, `SELECT COUNT(*) FROM m WHERE id=1 AND amount=10.5 AND note='a'`, 1)
	assertCount(t, tgt, `SELECT COUNT(*) FROM m WHERE id=2 AND note='b'`, 1)
	assertCount(t, tgt, `SELECT COUNT(*) FROM m WHERE id=3`, 0)

	for _, stmt := range splitOnSemicolon(b.UndoSQL) {
		if err := tgt.Exec(ctx, stmt); err != nil {
			t.Fatalf("undo %q: %v", stmt, err)
		}
	}
	assertCount(t, tgt, `SELECT COUNT(*) FROM m WHERE id=3`, 1)
	assertCount(t, tgt, `SELECT COUNT(*) FROM m WHERE id=2 AND note='B'`, 1)
}

func assertCount(t *testing.T, db *source.Database, q string, want int) {
	t.Helper()
	var n int
	if err := db.DB.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if n != want {
		t.Fatalf("%s: got %d want %d", q, n, want)
	}
}

func splitOnSemicolon(sql string) []string {
	var out []string
	start := 0
	inString := false
	for i := 0; i < len(sql); i++ {
		switch sql[i] {
		case '\'':
			if inString && i+1 < len(sql) && sql[i+1] == '\'' {
				i++
				continue
			}
			inString = !inString
		case ';':
			if !inString {
				out = append(out, sql[start:i])
				start = i + 1
			}
		}
	}
	return out
}

func TestTransientSkippedByDefault(t *testing.T) {
	_, _, tp := pair(t)
	diffs := []model.Diff{
		{Kind: model.KindMissing, Stability: model.StabilityTransient, PK: "1",
			PKValues: []model.KeyValue{{Column: "id", Value: "1", Type: model.TypeInt}}},
	}
	sel, skipped := DefaultOptions().Prepare(diffs)
	if len(sel) != 0 || skipped != 1 {
		t.Fatalf("transient diff should be skipped: sel=%d skipped=%d", len(sel), skipped)
	}
	b, err := Generate(tp, dialect.SQLite{}, sel, Input{}, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if b.Statements != 0 {
		t.Fatalf("expected no statements, got %d", b.Statements)
	}
}

func TestSensitiveColumnsRefused(t *testing.T) {
	dir := t.TempDir()
	db, err := source.Open(context.Background(), source.Config{Driver: model.DriverSQLite, DSN: dir + "/x.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec(context.Background(), `CREATE TABLE u (id INTEGER PRIMARY KEY, password TEXT)`); err != nil {
		t.Fatal(err)
	}
	ss, _ := db.Introspect(context.Background(), "u")
	tp, err := plan.Build(model.TableSpec{Name: "u"}, db, db, ss, ss, model.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(context.Background(), `INSERT INTO u VALUES (1, 'hunter2')`); err != nil {
		t.Fatal(err)
	}
	row := rowForTable(t, tp, db, "u", 1)
	in := Input{SourceRows: map[string]SourceRow{"1": row}}
	diffs := []model.Diff{{Kind: model.KindMissing, Stability: model.StabilityStable, PK: "1",
		PKValues: []model.KeyValue{{Column: "id", Value: "1", Type: model.TypeInt}}}}
	_, err = Generate(tp, dialect.SQLite{}, diffs, in, DefaultOptions())
	if err == nil {
		t.Fatal("a sensitive column must block SQL generation rather than leak its value")
	}
}

func TestPostgresLiterals(t *testing.T) {
	d := dialect.Postgres{}
	cases := []struct {
		v    canon.Value
		typ  model.LogicalType
		want string
	}{
		{canon.Value{Raw: []byte("123.45")}, model.TypeDecimal, "123.45"},
		{canon.Value{Null: true}, model.TypeDecimal, "NULL"},
		{canon.Value{Raw: []byte("abc")}, model.TypeString, "'abc'"},
		{canon.Value{Raw: []byte("it's")}, model.TypeString, "'it''s'"},
		{canon.Value{Raw: []byte{0xde, 0xad}}, model.TypeBytes, `'\xdead'::bytea`},
		{canon.Value{Raw: []byte("1")}, model.TypeBool, "TRUE"},
	}
	for _, c := range cases {
		if got := d.Literal(c.v, c.typ); got != c.want {
			t.Errorf("Literal(%q, %s) = %s, want %s", c.v.Raw, c.typ, got, c.want)
		}
	}
}
