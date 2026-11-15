package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bankcmp/internal/model"
)

// TestSQLiteDigestUDFReadOnly proves the summary UDF is available on a read
// only connection, which is how the engine opens a production source.
func TestSQLiteDigestUDFReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.db")
	rw, err := Open(context.Background(), Config{Driver: model.DriverSQLite, DSN: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := rw.Exec(context.Background(), `CREATE TABLE d (id INTEGER PRIMARY KEY, b BLOB, s TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := rw.Exec(context.Background(), `INSERT INTO d VALUES (1, X'deadbeef', 'abc')`); err != nil {
		t.Fatal(err)
	}
	rw.Close()

	ro, err := Open(context.Background(), Config{Driver: model.DriverSQLite, DSN: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var blobDigest, textDigest, pgsqlStyle string
	if err := ro.DB.QueryRow(`SELECT bc_sha256(b), bc_sha256_text(s,'utf-8') FROM d WHERE id=1`).
		Scan(&blobDigest, &textDigest); err != nil {
		t.Fatal(err)
	}
	// Known SHA-256 values.
	const wantBlob = "5f78c33274e43fa9de5659265c1d917e25c03722dcb0b8d27db8d5feaa813953"
	const wantText = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if blobDigest != wantBlob {
		t.Errorf("blob digest %s", blobDigest)
	}
	if textDigest != wantText {
		t.Errorf("text digest %s", textDigest)
	}
	if err := ro.DB.QueryRow(`SELECT bc_sha256(b) FROM d`).Scan(&pgsqlStyle); err != nil {
		t.Fatal(err)
	}
	// A read only connection must refuse writes.
	if err := ro.Exec(context.Background(), `INSERT INTO d VALUES (2, X'00', 'z')`); err == nil {
		t.Fatal("read only connection accepted a write")
	}
}

func TestSQLiteDigestGBK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.db")
	db, err := Open(context.Background(), Config{Driver: model.DriverSQLite, DSN: path})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Exec(context.Background(), `CREATE TABLE g (id INTEGER PRIMARY KEY, n TEXT)`); err != nil {
		t.Fatal(err)
	}
	// 张三 in GBK bytes.
	if err := db.Exec(context.Background(), `INSERT INTO g VALUES (1, ?)`, string([]byte{0xd5, 0xc5, 0xc8, 0xfd})); err != nil {
		t.Fatal(err)
	}
	// Digesting with UTF-8 must differ from digesting the raw GBK bytes...
	var raw, decoded string
	if err := db.DB.QueryRow(`SELECT bc_sha256(n), bc_sha256_text(n,'gbk') FROM g`).Scan(&raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if raw == decoded {
		t.Fatal("GBK decoding did not change the digest")
	}
	// ...and the decoded digest must equal the digest of the UTF-8 text.
	var utf8Digest string
	if err := db.DB.QueryRow(`SELECT bc_sha256_text(?, 'utf-8')`, "张三").Scan(&utf8Digest); err != nil {
		t.Fatal(err)
	}
	if decoded != utf8Digest {
		t.Fatalf("GBK digest %s != UTF-8 digest %s", decoded, utf8Digest)
	}
}

func TestPostgresStatementTimeout(t *testing.T) {
	dsn := pgDSNForTest(t)
	db, err := Open(context.Background(), Config{
		Driver: model.DriverPostgres, DSN: dsn, MaxConns: 2, StatementTimeoutMS: 250,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	// A query that sleeps past the timeout must fail rather than hang.
	start := time.Now()
	err = db.Exec(ctx, `SELECT pg_sleep(2)`)
	if err == nil {
		t.Fatal("expected the statement to be cancelled by the timeout")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("timeout did not apply: took %s", time.Since(start))
	}
	// A second query on a freshly checked out connection must also time out.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var ms string
	if err := conn.QueryRowContext(ctx, `SHOW statement_timeout`).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	if ms != "250ms" {
		t.Fatalf("statement_timeout = %q, want 250ms", ms)
	}
}

func pgDSNForTest(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("BANKCMP_TEST_PG")
	if dsn == "" {
		t.Skip("set BANKCMP_TEST_PG to run PostgreSQL integration tests")
	}
	return dsn
}

// TestSQLiteReadOnlySeesCommittedWrites proves a read only pool opened on a
// rollback journal database still sees commits made by another connection.
// Without WAL, a long running read can block writers on some platforms; the
// comparison must at least never silently read a stale snapshot across
// separate queries.
func TestSQLiteReadOnlySeesCommittedWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "w.db")
	w, err := Open(context.Background(), Config{Driver: model.DriverSQLite, DSN: path})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx := context.Background()
	if err := w.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := w.Exec(ctx, `INSERT INTO t VALUES (1,'a')`); err != nil {
		t.Fatal(err)
	}
	ro, err := Open(ctx, Config{Driver: model.DriverSQLite, DSN: path, ReadOnly: true, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var n int
	ro.DB.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n)
	if n != 1 {
		t.Fatalf("initial count %d", n)
	}
	if err := w.Exec(ctx, `INSERT INTO t VALUES (2,'b')`); err != nil {
		t.Fatal(err)
	}
	ro.DB.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n)
	if n != 2 {
		t.Fatalf("read only connection did not see the committed write: %d", n)
	}
	// The write must still be refused on the read only pool.
	if err := ro.Exec(ctx, `INSERT INTO t VALUES (3,'c')`); err == nil {
		t.Fatal("read only pool accepted a write")
	}
}
