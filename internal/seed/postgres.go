package seed

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"

	"bankcmp/internal/dialect"
	"bankcmp/internal/model"
	"bankcmp/internal/source"
)

// PostgresPair seeds a PostgreSQL source/target pair.  It is used to prove the
// engine is not SQLite specific: the same planted manifest must be reproduced
// when both sides are PostgreSQL.
func PostgresPair(ctx context.Context, adminDSN, srcName, tgtName string, n int, seedVal int64, manifestPath string, configPath string) error {
	if err := recreateDatabase(adminDSN, srcName); err != nil {
		return err
	}
	if err := recreateDatabase(adminDSN, tgtName); err != nil {
		return err
	}
	src, err := openPG(adminDSN, srcName)
	if err != nil {
		return err
	}
	defer src.Close()
	tgt, err := openPG(adminDSN, tgtName)
	if err != nil {
		return err
	}
	defer tgt.Close()

	man := Manifest{Seed: seedVal, Rows: n}
	// accounts: numeric and date are already typed on both sides; a legacy
	// source stores money as text to exercise the normaliser.
	if err := createPGTable(ctx, src, `CREATE TABLE accounts (
		id bigint PRIMARY KEY, cust_no bigint NOT NULL, acct_type text NOT NULL,
		balance text NOT NULL, open_date text NOT NULL, status text NOT NULL, branch text NOT NULL)`); err != nil {
		return err
	}
	if err := createPGTable(ctx, tgt, `CREATE TABLE accounts (
		id bigint PRIMARY KEY, cust_no bigint NOT NULL, acct_type text NOT NULL,
		balance numeric(18,4) NOT NULL, open_date date NOT NULL, status text NOT NULL, branch text NOT NULL)`); err != nil {
		return err
	}
	if err := copyPG(ctx, src, tgt, "accounts", n); err != nil {
		return err
	}
	planted, err := plantPGAccounts(ctx, tgt, n, seedVal)
	if err != nil {
		return err
	}
	man.Tables = append(man.Tables, TableManifest{Name: "accounts", Rows: n, Planted: planted})

	// transactions: large memo/BLOB compared by digest.
	for _, db := range []*source.Database{src, tgt} {
		if err := createPGTable(ctx, db, `CREATE TABLE transactions (
			id bigint PRIMARY KEY, acct_id bigint NOT NULL, amount numeric(18,2) NOT NULL,
			txn_time timestamp NOT NULL, memo text NOT NULL, receipt bytea NOT NULL)`); err != nil {
			return err
		}
	}
	if err := copyPGTx(ctx, src, tgt, n); err != nil {
		return err
	}
	plantedTx, err := plantPGTx(ctx, tgt, n, seedVal)
	if err != nil {
		return err
	}
	man.Tables = append(man.Tables, TableManifest{Name: "transactions", Rows: n, Planted: plantedTx})

	if err := writeManifestFile(manifestPath, man); err != nil {
		return err
	}
	cfg := fmt.Sprintf(`source:
  driver: postgres
  dsn: %s
target:
  driver: postgres
  dsn: %s
options:
  workers: 2
  chunk_workers: 4
  chunk_rows: 20000
  recheck: true
  settle_ms: 300
tables:
  - name: accounts
    columns:
      - name: balance
        type: decimal
      - name: open_date
        type: date
  - name: transactions
    columns:
      - name: memo
        type: string
        digest: true
      - name: receipt
        type: bytes
        digest: true
`, pgURL(adminDSN, srcName), pgURL(adminDSN, tgtName))
	return os.WriteFile(configPath, []byte(cfg), 0o644)
}

func pgURL(adminDSN, name string) string {
	u, err := url.Parse(adminDSN)
	if err != nil {
		return adminDSN
	}
	u.Path = "/" + name
	return u.String()
}

func recreateDatabase(adminDSN, name string) error {
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return err
	}
	defer admin.Close()
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop %s: %w", name, err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	return nil
}

func openPG(adminDSN, name string) (*source.Database, error) {
	return source.Open(context.Background(), source.Config{
		Driver: model.DriverPostgres, DSN: pgURL(adminDSN, name), MaxConns: 8,
	})
}

func createPGTable(ctx context.Context, db *source.Database, ddl string) error {
	_, err := db.DB.ExecContext(ctx, ddl)
	return err
}

// copyPG copies accounts from the source (all text) to the target (typed),
// using PostgreSQL's own casts so the target holds genuine numeric/date values.
func copyPG(ctx context.Context, src, tgt *source.Database, table string, n int) error {
	tx, err := src.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	st, err := tx.PrepareContext(ctx, `INSERT INTO accounts VALUES ($1,$2,$3,$4,$5,$6,$7)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for i := 0; i < n; i++ {
		r := rndFor(table, i)
		if _, err := st.ExecContext(ctx, i, 5000+i, []string{"SAV", "CHK", "LOAN"}[i%3],
			accountsBalance(i, r),
			fmt.Sprintf("2024-%02d-%02d", 1+r.Intn(12), 1+r.Intn(28)),
			[]string{"A", "F", "C"}[r.Intn(3)], fmt.Sprintf("B%03d", i%120)); err != nil {
			st.Close()
			tx.Rollback()
			return err
		}
	}
	st.Close()
	if err := tx.Commit(); err != nil {
		return err
	}
	// Copy through a typed read/write, applying the same casts a migration ETL
	// would (text money -> numeric, text date -> date).
	return copyViaRows(ctx, src, tgt, n)
}

func copyViaRows(ctx context.Context, src, tgt *source.Database, n int) error {
	if _, err := tgt.DB.ExecContext(ctx, `TRUNCATE accounts`); err != nil {
		return err
	}
	rows, err := src.DB.QueryContext(ctx, `SELECT id, cust_no, acct_type, balance, open_date, status, branch FROM accounts ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	tx, err := tgt.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	st, err := tx.PrepareContext(ctx, `INSERT INTO accounts VALUES ($1,$2,$3,$4::numeric,$5::date,$6,$7)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for rows.Next() {
		var id, custNo int64
		var acctType, balance, openDate, status, branch string
		if err := rows.Scan(&id, &custNo, &acctType, &balance, &openDate, &status, &branch); err != nil {
			st.Close()
			tx.Rollback()
			return err
		}
		if _, err := st.ExecContext(ctx, id, custNo, acctType, balance, openDate, status, branch); err != nil {
			st.Close()
			tx.Rollback()
			return err
		}
	}
	st.Close()
	return tx.Commit()
}

func copyPGTx(ctx context.Context, src, tgt *source.Database, n int) error {
	for _, db := range []*source.Database{src, tgt} {
		tx, err := db.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		st, err := tx.PrepareContext(ctx, `INSERT INTO transactions VALUES ($1,$2,$3,$4,$5,$6)`)
		if err != nil {
			tx.Rollback()
			return err
		}
		for i := 0; i < n; i++ {
			r := rndFor("transactions", i)
			amount := fmt.Sprintf("%d.%02d", 10+i%9999, r.Intn(100))
			txnTime := fmt.Sprintf("2024-%02d-%02d %02d:%02d:%02d",
				1+r.Intn(12), 1+r.Intn(28), r.Intn(24), r.Intn(60), r.Intn(60))
			memo := strings.Repeat(fmt.Sprintf("流水备注%06d ", i), 12)
			receipt := make([]byte, 256)
			r.Read(receipt)
			if _, err := st.ExecContext(ctx, i, 5000+i, amount, txnTime, memo, receipt); err != nil {
				st.Close()
				tx.Rollback()
				return err
			}
		}
		st.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func plantPGAccounts(ctx context.Context, tgt *source.Database, n int, seedVal int64) ([]Planted, error) {
	rnd := newRnd(seedVal + 7)
	var out []Planted
	lost := pickDistinct(rnd, n, maxInt(3, n/40000))
	for _, id := range lost {
		if _, err := tgt.DB.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, id); err != nil {
			return nil, err
		}
		out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "missing"})
	}
	for k := 0; k < maxInt(2, n/60000); k++ {
		id := n + 100000 + k
		if _, err := tgt.DB.ExecContext(ctx, `INSERT INTO accounts VALUES ($1,$2,$3,$4::numeric,$5::date,$6,$7)`,
			id, 100000+id%5000, "SAV", fmt.Sprintf("%d.0000", 1000+id*7), "2024-03-01", "A", "B999"); err != nil {
			return nil, err
		}
		out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "extra"})
	}
	ids := pickDistinctExcluding(rnd, n, maxInt(5, n/15000), lost)
	for _, id := range ids {
		r := rndFor("accounts", id)
		oldBal := accountsBalance(id, r)
		newBal := truncateDecimals(oldBal, 2)
		if newBal == oldBal {
			continue
		}
		if _, err := tgt.DB.ExecContext(ctx, `UPDATE accounts SET balance=$1::numeric WHERE id=$2`, newBal, id); err != nil {
			return nil, err
		}
		out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "modified", Column: "balance", Old: oldBal, New: newBal})
	}
	ids2 := pickDistinctExcluding(rnd, n, maxInt(3, n/30000), append(append([]int{}, lost...), ids...))
	for _, id := range ids2 {
		if _, err := tgt.DB.ExecContext(ctx, `UPDATE accounts SET open_date = open_date + 1 WHERE id=$1`, id); err != nil {
			return nil, err
		}
		out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "modified", Column: "open_date"})
	}
	return out, nil
}

func plantPGTx(ctx context.Context, tgt *source.Database, n int, seedVal int64) ([]Planted, error) {
	rnd := newRnd(seedVal + 11)
	var out []Planted
	start := n * 3 / 4
	end := start + maxInt(2, n/80000)
	for id := start; id < end; id++ {
		if _, err := tgt.DB.ExecContext(ctx, `DELETE FROM transactions WHERE id=$1`, id); err != nil {
			return nil, err
		}
		out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "missing"})
	}
	for _, id := range pickDistinctOffset(rnd, n, maxInt(2, n/50000), n/2) {
		if _, err := tgt.DB.ExecContext(ctx, `UPDATE transactions SET memo = memo || 'X' WHERE id=$1`, id); err != nil {
			return nil, err
		}
		out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "modified", Column: "memo"})
	}
	for _, id := range pickDistinctOffset(rnd, n, maxInt(2, n/60000), n/3) {
		if _, err := tgt.DB.ExecContext(ctx, `UPDATE transactions SET receipt = substring(receipt from 1 for 100) WHERE id=$1`, id); err != nil {
			return nil, err
		}
		out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "modified", Column: "receipt"})
	}
	return out, nil
}

var _ = dialect.Postgres{}
