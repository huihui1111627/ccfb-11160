package engine

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"bankcmp/internal/model"
	"bankcmp/internal/plan"
	"bankcmp/internal/source"
)

// pgDSNFromEnv returns the administrative DSN used for tests, or "" when no
// PostgreSQL server is configured.  Tests skip themselves in that case so the
// suite runs anywhere.
func pgDSNFromEnv(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("BANKCMP_TEST_PG")
	if dsn == "" {
		t.Skip("set BANKCMP_TEST_PG to run PostgreSQL integration tests")
	}
	return dsn
}

// newPGDatabase creates a scratch database and returns a Database for it.
func newPGDatabase(t *testing.T, adminDSN, name string) *source.Database {
	t.Helper()
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + name); err != nil {
		t.Fatalf("drop %s: %v", name, err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := source.Open(context.Background(), source.Config{
		Driver: model.DriverPostgres, DSN: u.String(), MaxConns: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestPostgresToSQLiteTypes(t *testing.T) {
	admin := pgDSNFromEnv(t)
	name := fmt.Sprintf("bankcmp_t_%d", time.Now().UnixNano()%1e9)
	pg := newPGDatabase(t, admin, name)

	dir := t.TempDir()
	lite, err := source.Open(context.Background(), source.Config{
		Driver: model.DriverSQLite, DSN: dir + "/lite.db",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lite.Close()

	// Target (new) schema: typed columns, char padding, timestamptz.
	if err := pg.Exec(context.Background(), `CREATE TABLE cust (
		id bigint PRIMARY KEY,
		cust_no bigint NOT NULL,
		name varchar(64) NOT NULL,
		code char(6) NOT NULL,
		balance numeric(18,4) NOT NULL,
		opened date NOT NULL,
		last_seen timestamptz NOT NULL,
		active boolean NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	// Source (legacy) schema: everything textual, GBK not needed here.
	if err := lite.Exec(context.Background(), `CREATE TABLE cust (
		id INTEGER PRIMARY KEY,
		cust_no INTEGER NOT NULL,
		name TEXT NOT NULL,
		code TEXT NOT NULL,
		balance TEXT NOT NULL,
		opened TEXT NOT NULL,
		last_seen TEXT NOT NULL,
		active TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("客户%d", i)
		bal := fmt.Sprintf("%d.%04d", 100+i, i%10000)
		day := fmt.Sprintf("2024-%02d-%02d", 1+i%12, 1+i%28)
		ts := fmt.Sprintf("%s %02d:%02d:%02d", day, i%24, i%60, i%60)
		if err := lite.Exec(context.Background(), `INSERT INTO cust VALUES (?,?,?,?,?,?,?,?)`,
			i, 5000+i, name, "0001", bal, day, ts, "1"); err != nil {
			t.Fatal(err)
		}
		if err := pg.Exec(context.Background(), `INSERT INTO cust VALUES ($1,$2,$3,$4,$5::numeric,$6::date,$7::timestamptz,$8::boolean)`,
			i, 5000+i, name, "0001", bal, day, ts, true); err != nil {
			t.Fatal(err)
		}
	}
	// Planted: one lost row, one changed amount, one changed timestamp.
	for _, q := range []string{
		`DELETE FROM cust WHERE id=7`,
		`UPDATE cust SET balance = balance - 0.01 WHERE id=120`,
		`UPDATE cust SET last_seen = last_seen + interval '1 second' WHERE id=121`,
	} {
		if err := pg.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}

	spec := model.TableSpec{Name: "cust", Columns: []model.ColumnSpec{
		{Name: "balance", Type: model.TypeDecimal},
		{Name: "opened", Type: model.TypeDate},
		{Name: "last_seen", Type: model.TypeDateTime},
		{Name: "active", Type: model.TypeBool},
	}}
	res := compareAcross(t, lite, pg, spec)
	if len(res.Diffs) != 3 {
		t.Fatalf("expected 3 diffs, got %d: %+v", len(res.Diffs), res.Diffs)
	}
	got := map[string]model.DiffKind{}
	for _, d := range res.Diffs {
		got[d.PK] = d.Kind
	}
	want := map[string]model.DiffKind{"7": model.KindMissing, "120": model.KindModified, "121": model.KindModified}
	for pk, kind := range want {
		if got[pk] != kind {
			t.Errorf("pk %s: got %q want %q", pk, got[pk], kind)
		}
	}
}

func TestPostgresDigestLargeColumns(t *testing.T) {
	admin := pgDSNFromEnv(t)
	base := time.Now().UnixNano() % 1e9
	srcName := fmt.Sprintf("bankcmp_dig_s_%d", base)
	tgtName := fmt.Sprintf("bankcmp_dig_t_%d", base)
	src := newPGDatabase(t, admin, srcName)
	tgt := newPGDatabase(t, admin, tgtName)
	for _, db := range []*source.Database{src, tgt} {
		if err := db.Exec(context.Background(), `CREATE TABLE docs (
			id bigint PRIMARY KEY,
			body text NOT NULL,
			blob bytea NOT NULL
		)`); err != nil {
			t.Fatal(err)
		}
	}
	body := strings.Repeat("这是很长的备注内容 ", 400)
	bin := make([]byte, 2048)
	for i := range bin {
		bin[i] = byte(i)
	}
	for i := 0; i < 50; i++ {
		for _, db := range []*source.Database{src, tgt} {
			if err := db.Exec(context.Background(), `INSERT INTO docs VALUES ($1,$2,$3)`, i, body, bin); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tgt.Exec(context.Background(), `UPDATE docs SET body = body || 'x' WHERE id=10`); err != nil {
		t.Fatal(err)
	}
	if err := tgt.Exec(context.Background(), `UPDATE docs SET blob = substring(blob from 1 for 1000) WHERE id=11`); err != nil {
		t.Fatal(err)
	}
	spec := model.TableSpec{Name: "docs", Columns: []model.ColumnSpec{
		{Name: "body", Type: model.TypeString, Digest: true},
		{Name: "blob", Type: model.TypeBytes, Digest: true},
	}}
	tp := planForSpec(t, src, tgt, spec)
	res, err := NewRunner(src, tgt, Options{ChunkRows: 10, Recheck: false}).RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diffs) != 2 {
		t.Fatalf("expected 2 digest differences, got %d: %+v", len(res.Diffs), res.Diffs)
	}
	pks := map[string]bool{}
	for _, d := range res.Diffs {
		pks[d.PK] = true
	}
	if !pks["10"] || !pks["11"] {
		t.Fatalf("wrong digest differences: %v", pks)
	}
	for _, d := range res.Diffs {
		for _, c := range d.Columns {
			if len(c.Source) > 200 || len(c.Target) > 200 {
				t.Fatalf("digest column preview should be short, got %d bytes", len(c.Source))
			}
		}
	}
}

// compareAcross resolves a plan between two databases and runs it.
func compareAcross(t *testing.T, src, tgt *source.Database, spec model.TableSpec) *TableResult {
	t.Helper()
	tp := planForSpec(t, src, tgt, spec)
	res, err := NewRunner(src, tgt, Options{ChunkRows: 25, ChunkWorkers: 2, Recheck: false}).
		RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestUniqueIndexUsedAsKey: a table with no primary key but two separate
// single column unique indexes must use exactly one of them as the key, not a
// concatenation that is not unique.
func TestUniqueIndexUsedAsKey(t *testing.T) {
	admin := pgDSNFromEnv(t)
	name := fmt.Sprintf("bankcmp_uidx_%d", time.Now().UnixNano()%1e9)
	pg := newPGDatabase(t, admin, name)
	if err := pg.Exec(context.Background(), `CREATE TABLE k (
		a bigint NOT NULL, b bigint NOT NULL, v text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := pg.Exec(context.Background(), `CREATE UNIQUE INDEX k_a ON k(a)`); err != nil {
		t.Fatal(err)
	}
	if err := pg.Exec(context.Background(), `CREATE UNIQUE INDEX k_b ON k(b)`); err != nil {
		t.Fatal(err)
	}
	if err := pg.Exec(context.Background(), `INSERT INTO k VALUES (1,10,'x'),(2,20,'y')`); err != nil {
		t.Fatal(err)
	}
	sch, err := pg.Introspect(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(sch.PrimaryKey) != 1 {
		t.Fatalf("expected exactly one key column, got %v", sch.PrimaryKey)
	}
}

// TestNoKeyRejected: a table with no usable key must be refused, not compared
// with an unstable whole-row key.
func TestNoKeyRejected(t *testing.T) {
	admin := pgDSNFromEnv(t)
	name := fmt.Sprintf("bankcmp_nokey_%d", time.Now().UnixNano()%1e9)
	pg := newPGDatabase(t, admin, name)
	if err := pg.Exec(context.Background(), `CREATE TABLE n (a bigint, b text)`); err != nil {
		t.Fatal(err)
	}
	ss, _ := pg.Introspect(context.Background(), "n")
	_, err := planBuildPkg(pg, pg, ss, ss, model.TableSpec{Name: "n"})
	if err == nil {
		t.Fatal("a keyless table must be rejected")
	}
}

func planBuildPkg(src, tgt *source.Database, ss, ts *source.TableSchema, spec model.TableSpec) (*plan.TablePlan, error) {
	return plan.Build(spec, src, tgt, ss, ts, model.Options{})
}

// TestDecimalKeysAcrossEngines: a numeric key with fractional values must be
// ordered identically on a SQLite text column and a PostgreSQL numeric column,
// and every row must land in exactly one range.
func TestDecimalKeysAcrossEngines(t *testing.T) {
	admin := pgDSNFromEnv(t)
	name := fmt.Sprintf("bankcmp_deckey_%d", time.Now().UnixNano()%1e9)
	pg := newPGDatabase(t, admin, name)
	if err := pg.Exec(context.Background(), `CREATE TABLE m (id numeric(30,6) PRIMARY KEY, v text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lite, err := source.Open(context.Background(), source.Config{Driver: model.DriverSQLite, DSN: dir + "/m.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer lite.Close()
	if err := lite.Exec(context.Background(), `CREATE TABLE m (id TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	ids := []string{
		"-999999.999999", "-1.500000", "-0.000001", "0.000000", "0.000001",
		"1.000000", "1.500000", "9.999999", "10.000000", "999999.999999",
	}
	for _, id := range ids {
		if err := lite.Exec(context.Background(), `INSERT INTO m VALUES (?,?)`, id, "v"+id); err != nil {
			t.Fatal(err)
		}
		if err := pg.Exec(context.Background(), `INSERT INTO m VALUES ($1::numeric,$2)`, id, "v"+id); err != nil {
			t.Fatal(err)
		}
	}
	if err := pg.Exec(context.Background(), `DELETE FROM m WHERE id='-0.000001'::numeric`); err != nil {
		t.Fatal(err)
	}
	if err := pg.Exec(context.Background(), `UPDATE m SET v='changed' WHERE id='10.000000'::numeric`); err != nil {
		t.Fatal(err)
	}
	spec := model.TableSpec{Name: "m", Columns: []model.ColumnSpec{{Name: "id", Type: model.TypeDecimal}}}
	res := compareAcross(t, lite, pg, spec)
	if len(res.Diffs) != 2 {
		t.Fatalf("expected 2 diffs, got %d: %+v", len(res.Diffs), res.Diffs)
	}
	got := map[string]model.DiffKind{}
	for _, d := range res.Diffs {
		got[d.PK] = d.Kind
	}
	if got["-0.000001"] != model.KindMissing || got["10"] != model.KindModified {
		t.Fatalf("decimal key diffs: %v", got)
	}
}
