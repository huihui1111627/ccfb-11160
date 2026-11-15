package api

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"bankcmp/internal/model"
	"bankcmp/internal/source"
)

// TestSplitStatements handles quoted semicolons and doubled quotes, which is
// what the generated SQL can contain.
func TestSplitStatements(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`INSERT INTO t VALUES ('a;b');`, 1},
		{`INSERT INTO t VALUES ('it''s; here'); DELETE FROM t WHERE id=1;`, 2},
		{`UPDATE t SET a='x' WHERE id=1; UPDATE t SET a='y' WHERE id=2;`, 2},
		{``, 0},
		{`  ;  ;  `, 0},
	}
	for _, c := range cases {
		if got := len(splitStatements(c.in)); got != c.want {
			t.Errorf("%q -> %d statements, want %d", c.in, got, c.want)
		}
	}
}

// TestExecuteBatchIsAtomic applies a batch where the second statement fails and
// confirms the first was rolled back, so a partial repair cannot be left behind.
func TestExecuteBatchIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	db, err := source.Open(context.Background(), source.Config{Driver: model.DriverSQLite, DSN: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(context.Background(), `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = executeSQLBatch(context.Background(), model.DriverSQLite, path, "",
		`INSERT INTO t VALUES (1,'a'); INSERT INTO t VALUES (1,'duplicate');`)
	if err == nil {
		t.Fatal("expected the batch to fail on the duplicate key")
	}
	check, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var n int
	if err := check.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("first statement was not rolled back: %d rows remain", n)
	}
}
