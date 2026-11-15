package plan_test

import (
	"context"
	"strings"
	"testing"

	"bankcmp/internal/model"
	"bankcmp/internal/plan"
	"bankcmp/internal/source"
)

func openDB(t *testing.T) *source.Database {
	t.Helper()
	db, err := source.Open(context.Background(), source.Config{
		Driver: model.DriverSQLite, DSN: t.TempDir() + "/x.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func build(t *testing.T, db *source.Database, table string, spec model.TableSpec) (*plan.TablePlan, error) {
	t.Helper()
	ss, err := db.Introspect(context.Background(), table)
	if err != nil {
		t.Fatal(err)
	}
	return plan.Build(spec, db, db, ss, ss, model.Options{ChunkRows: 100})
}

func TestKeyColumnNormalisationRejected(t *testing.T) {
	db := openDB(t)
	if err := db.Exec(context.Background(), `CREATE TABLE t (code TEXT PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	spec := model.TableSpec{Name: "t", Columns: []model.ColumnSpec{
		{Name: "code", CaseFold: true},
	}}
	_, err := build(t, db, "t", spec)
	if err == nil || !strings.Contains(err.Error(), "cannot use value normalisation") {
		t.Fatalf("expected rejection, got %v", err)
	}
}

func TestIncompatibleKeyOrderingRejected(t *testing.T) {
	db := openDB(t)
	if err := db.Exec(context.Background(), `CREATE TABLE t (k INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	ss, _ := db.Introspect(context.Background(), "t")
	// Pretend one side stores the key as text: the byte ordering of "10" < "9"
	// differs from the numeric order, so the plan must refuse.
	ts := *ss
	ts.Columns = append([]source.Column{}, ss.Columns...)
	ts.Columns[0].Logical = model.TypeString
	_, err := plan.Build(model.TableSpec{Name: "t"}, db, db, ss, &ts, model.Options{})
	if err == nil {
		t.Fatal("int vs text key must be rejected")
	}
}

func TestNullableKeyRejectedAtPlanTime(t *testing.T) {
	db := openDB(t)
	if err := db.Exec(context.Background(), `CREATE TABLE t (k INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	ss, _ := db.Introspect(context.Background(), "t")
	ss.Columns[0].Nullable = true
	_, err := plan.Build(model.TableSpec{Name: "t"}, db, db, ss, ss, model.Options{})
	if err == nil || !strings.Contains(err.Error(), "nullable") {
		t.Fatalf("expected nullable rejection, got %v", err)
	}
}

func TestMissingColumnRejected(t *testing.T) {
	db := openDB(t)
	if err := db.Exec(context.Background(), `CREATE TABLE t (k INTEGER PRIMARY KEY, a TEXT, b TEXT)`); err != nil {
		t.Fatal(err)
	}
	ss, _ := db.Introspect(context.Background(), "t")
	ts := *ss
	// Target lost column b.
	var cols []source.Column
	for _, c := range ss.Columns {
		if c.Name != "b" {
			cols = append(cols, c)
		}
	}
	ts.Columns = cols
	_, err := plan.Build(model.TableSpec{Name: "t"}, db, db, ss, &ts, model.Options{})
	if err == nil || !strings.Contains(err.Error(), "exists on source but not on target") {
		t.Fatalf("expected missing-column rejection, got %v", err)
	}
}

func TestColumnModeListed(t *testing.T) {
	db := openDB(t)
	if err := db.Exec(context.Background(), `CREATE TABLE t (k INTEGER PRIMARY KEY, a TEXT, b TEXT, c TEXT)`); err != nil {
		t.Fatal(err)
	}
	spec := model.TableSpec{Name: "t", ColumnMode: "listed", Columns: []model.ColumnSpec{{Name: "a"}}}
	tp, err := build(t, db, "t", spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(tp.Columns) != 1 || tp.Columns[0].SourceName != "a" {
		t.Fatalf("listed mode: %+v", tp.Columns)
	}
	// Default mode compares everything.
	tp2, err := build(t, db, "t", model.TableSpec{Name: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tp2.Columns) != 3 {
		t.Fatalf("all mode: %d columns", len(tp2.Columns))
	}
}

func TestDigestOnlyForStringAndBytes(t *testing.T) {
	db := openDB(t)
	if err := db.Exec(context.Background(), `CREATE TABLE t (k INTEGER PRIMARY KEY, n NUMERIC, s TEXT)`); err != nil {
		t.Fatal(err)
	}
	spec := model.TableSpec{Name: "t", Columns: []model.ColumnSpec{{Name: "n", Digest: true}}}
	if _, err := build(t, db, "t", spec); err == nil {
		t.Fatal("digest on a numeric column must be rejected")
	}
}

func TestDateAndDateTimeWiden(t *testing.T) {
	db := openDB(t)
	if err := db.Exec(context.Background(), `CREATE TABLE t (k INTEGER PRIMARY KEY, d DATE)`); err != nil {
		t.Fatal(err)
	}
	ss, _ := db.Introspect(context.Background(), "t")
	ts := *ss
	ts.Columns = append([]source.Column{}, ss.Columns...)
	for i := range ts.Columns {
		if ts.Columns[i].Name == "d" {
			ts.Columns[i].Logical = model.TypeDateTime
		}
	}
	tp, err := plan.Build(model.TableSpec{Name: "t"}, db, db, ss, &ts, model.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range tp.Columns {
		if c.SourceName == "d" {
			if c.SrcNorm.Opt.Type != model.TypeDateTime {
				t.Fatalf("date+datetime should widen to datetime, got %s", c.SrcNorm.Opt.Type)
			}
		}
	}
}
