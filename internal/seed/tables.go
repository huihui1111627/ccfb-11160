package seed

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	"bankcmp/internal/source"
)

// tableDef describes one table pair.  The source is the legacy schema
// (amounts and dates as text, GBK text); the target is the migrated schema
// (numeric, date, timestamp, UTF-8).
type tableDef struct {
	name      string
	srcCreate string
	tgtCreate string
	srcRow    func(i int) []any
	tgtRow    func(i int) []any
	// plant mutates the target after the bulk load and returns exactly the
	// differences the comparison must report.
	plant func(ctx context.Context, tgt *source.Database, rnd *rand.Rand, n int) ([]Planted, error)
}

func tableDefs(n int) []tableDef {
	return []tableDef{
		accountsDef(n),
		transactionsDef(n),
		customersDef(n),
	}
}

func rndFor(table string, i int) *rand.Rand {
	h := int64(0)
	for _, c := range table {
		h = h*131 + int64(c)
	}
	return rand.New(rand.NewSource(h*1000003 + int64(i)))
}

var surnames = []string{"张", "王", "李", "赵", "刘", "陈", "杨", "黄", "周", "吴"}
var given = []string{"伟", "芳", "娜", "敏", "静", "磊", "洋", "强", "军", "杰"}

func chineseName(r *rand.Rand) string {
	return surnames[r.Intn(len(surnames))] + given[r.Intn(len(given))] + given[r.Intn(len(given))]
}

func gbk(s string) string {
	out, err := simplifiedchinese.GBK.NewEncoder().String(s)
	if err != nil {
		return s
	}
	return out
}

func accountsBalance(i int, r *rand.Rand) string {
	return fmt.Sprintf("%d.%04d", 1000+i*7, r.Intn(10000))
}

func accountsDef(n int) tableDef {
	const table = "accounts"
	srcCreate := `CREATE TABLE accounts (
		id         INTEGER PRIMARY KEY,
		cust_no    INTEGER NOT NULL,
		acct_type  TEXT NOT NULL,
		balance    TEXT NOT NULL,
		open_date  TEXT NOT NULL,
		status     TEXT NOT NULL,
		branch     TEXT NOT NULL
	)`
	tgtCreate := `CREATE TABLE accounts (
		id         INTEGER PRIMARY KEY,
		cust_no    INTEGER NOT NULL,
		acct_type  TEXT NOT NULL,
		balance    NUMERIC(18,4) NOT NULL,
		open_date  DATE NOT NULL,
		status     TEXT NOT NULL,
		branch     TEXT NOT NULL
	)`
	day := func(r *rand.Rand) string {
		return fmt.Sprintf("2024-%02d-%02d", 1+r.Intn(12), 1+r.Intn(28))
	}
	srcRow := func(i int) []any {
		r := rndFor(table, i)
		return []any{i, 100000 + i%5000, []string{"SAV", "CHK", "LOAN"}[i%3],
			accountsBalance(i, r), day(r), []string{"A", "F", "C"}[r.Intn(3)], fmt.Sprintf("B%03d", i%120)}
	}
	tgtRow := func(i int) []any {
		r := rndFor(table, i)
		return []any{i, 100000 + i%5000, []string{"SAV", "CHK", "LOAN"}[i%3],
			accountsBalance(i, r), day(r), []string{"A", "F", "C"}[r.Intn(3)], fmt.Sprintf("B%03d", i%120)}
	}
	plant := func(ctx context.Context, tgt *source.Database, rnd *rand.Rand, n int) ([]Planted, error) {
		var out []Planted
		lost := pickDistinct(rnd, n, maxInt(3, n/40000))
		for _, id := range lost {
			if err := exec(ctx, tgt, `DELETE FROM accounts WHERE id=?`, id); err != nil {
				return nil, err
			}
			out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "missing"})
		}
		for k := 0; k < maxInt(2, n/60000); k++ {
			id := n + 100000 + k
			if err := exec(ctx, tgt, `INSERT INTO accounts VALUES (?,?,?,?,?,?,?)`,
				id, 100000+id%5000, "SAV", fmt.Sprintf("%d.0000", 1000+id*7),
				"2024-03-01", "A", "B999"); err != nil {
				return nil, err
			}
			out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "extra"})
		}
		ids := pickDistinctExcluding(rnd, n, maxInt(5, n/15000), lost)
		for _, id := range ids {
			r := rndFor(table, id)
			oldBal := accountsBalance(id, r)
			newBal := truncateDecimals(oldBal, 2)
			if newBal == oldBal {
				continue
			}
			if err := exec(ctx, tgt, `UPDATE accounts SET balance=? WHERE id=?`, newBal, id); err != nil {
				return nil, err
			}
			out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "modified", Column: "balance", Old: oldBal, New: newBal})
		}
		ids2 := pickDistinctExcluding(rnd, n, maxInt(3, n/30000), append(append([]int{}, lost...), ids...))
		for _, id := range ids2 {
			var oldDayRaw any
			if err := tgt.DB.QueryRowContext(ctx, `SELECT open_date FROM accounts WHERE id=?`, id).Scan(&oldDayRaw); err != nil {
				return nil, err
			}
			oldDay := formatDay(oldDayRaw)
			newDay, err := shiftDay(oldDay, 1)
			if err != nil {
				return nil, err
			}
			if err := exec(ctx, tgt, `UPDATE accounts SET open_date=? WHERE id=?`, newDay, id); err != nil {
				return nil, err
			}
			out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "modified", Column: "open_date", Old: oldDay, New: newDay})
		}
		return out, nil
	}
	return tableDef{name: table, srcCreate: srcCreate, tgtCreate: tgtCreate,
		srcRow: srcRow, tgtRow: tgtRow, plant: plant}
}

func transactionsDef(n int) tableDef {
	const table = "transactions"
	srcCreate := `CREATE TABLE transactions (
		id        INTEGER PRIMARY KEY,
		acct_id   INTEGER NOT NULL,
		amount    TEXT NOT NULL,
		txn_time  TEXT NOT NULL,
		dr_cr     TEXT NOT NULL,
		remark    TEXT,
		memo      TEXT,
		receipt   BLOB
	)`
	tgtCreate := `CREATE TABLE transactions (
		id        INTEGER PRIMARY KEY,
		acct_id   INTEGER NOT NULL,
		amount    NUMERIC(18,2) NOT NULL,
		txn_time  TIMESTAMP NOT NULL,
		dr_cr     TEXT NOT NULL,
		remark    TEXT,
		memo      TEXT,
		receipt   BLOB
	)`
	row := func(i int) []any {
		r := rndFor(table, i)
		amount := fmt.Sprintf("%d.%02d", 10+i%9999, r.Intn(100))
		txnTime := fmt.Sprintf("2024-%02d-%02d %02d:%02d:%02d",
			1+r.Intn(12), 1+r.Intn(28), r.Intn(24), r.Intn(60), r.Intn(60))
		remark := fmt.Sprintf("转账-%d", i)
		memo := strings.Repeat(fmt.Sprintf("流水备注%06d ", i), 12)
		receipt := make([]byte, 256)
		r.Read(receipt)
		return []any{i, 100000 + i%5000, amount, txnTime, []string{"D", "C"}[i%2], remark, memo, receipt}
	}
	plant := func(ctx context.Context, tgt *source.Database, rnd *rand.Rand, n int) ([]Planted, error) {
		var out []Planted
		// A contiguous block of transactions never made it across: the
		// migration stopped at a chunk boundary.  This is exactly the kind of
		// loss a "sample a few tables and count" check misses.
		start := n * 3 / 4
		end := start + maxInt(2, n/80000)
		for id := start; id < end; id++ {
			if err := exec(ctx, tgt, `DELETE FROM transactions WHERE id=?`, id); err != nil {
				return nil, err
			}
			out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "missing"})
		}
		// Amounts lost their cents.
		ids := pickDistinct(rnd, n/2, maxInt(4, n/25000))
		for _, id := range ids {
			var oldAmt string
			if err := tgt.DB.QueryRowContext(ctx, `SELECT amount FROM transactions WHERE id=?`, id).Scan(&oldAmt); err != nil {
				return nil, err
			}
			newAmt := truncateDecimals(oldAmt, 0)
			if newAmt == oldAmt {
				continue
			}
			if err := exec(ctx, tgt, `UPDATE transactions SET amount=? WHERE id=?`, newAmt, id); err != nil {
				return nil, err
			}
			out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "modified", Column: "amount", Old: oldAmt, New: newAmt})
		}
		// A large memo corrupted mid-field: only the digest catches it.
		for _, id := range pickDistinctOffset(rnd, n, maxInt(2, n/50000), n/2) {
			if err := exec(ctx, tgt, `UPDATE transactions SET memo = memo || 'X' WHERE id=?`, id); err != nil {
				return nil, err
			}
			out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "modified", Column: "memo"})
		}
		// A receipt blob truncated.
		for _, id := range pickDistinctOffset(rnd, n, maxInt(2, n/60000), n/3) {
			if err := exec(ctx, tgt, `UPDATE transactions SET receipt = substr(receipt, 1, 100) WHERE id=?`, id); err != nil {
				return nil, err
			}
			out = append(out, Planted{PK: fmt.Sprintf("%d", id), Kind: "modified", Column: "receipt"})
		}
		return out, nil
	}
	return tableDef{name: table, srcCreate: srcCreate, tgtCreate: tgtCreate,
		srcRow: row, tgtRow: row, plant: plant}
}

func customersDef(n int) tableDef {
	const table = "customers"
	create := `CREATE TABLE customers (
		branch_id INTEGER NOT NULL,
		cust_no   INTEGER NOT NULL,
		name      TEXT NOT NULL,
		phone     TEXT,
		created   %s,
		PRIMARY KEY (branch_id, cust_no)
	)`
	srcCreate := fmt.Sprintf(create, "TEXT")
	tgtCreate := fmt.Sprintf(create, "DATE")
	custNo := func(i int) int { return 900000 + i }
	srcRow := func(i int) []any {
		r := rndFor(table, i)
		return []any{i % 97, custNo(i), gbk(chineseName(r)),
			fmt.Sprintf("138%08d", i%100000000),
			fmt.Sprintf("2023-%02d-%02d", 1+r.Intn(12), 1+r.Intn(28))}
	}
	tgtRow := func(i int) []any {
		r := rndFor(table, i)
		return []any{i % 97, custNo(i), chineseName(r),
			fmt.Sprintf("138%08d", i%100000000),
			fmt.Sprintf("2023-%02d-%02d", 1+r.Intn(12), 1+r.Intn(28))}
	}
	plant := func(ctx context.Context, tgt *source.Database, rnd *rand.Rand, n int) ([]Planted, error) {
		var out []Planted
		for _, i := range pickDistinct(rnd, n, maxInt(2, n/60000)) {
			branch, cust := i%97, custNo(i)
			if err := exec(ctx, tgt, `UPDATE customers SET phone=? WHERE branch_id=? AND cust_no=?`,
				"13900000000", branch, cust); err != nil {
				return nil, err
			}
			out = append(out, Planted{PK: fmt.Sprintf("%d|%d", branch, cust), Kind: "modified", Column: "phone"})
		}
		i := n - 1
		if err := exec(ctx, tgt, `DELETE FROM customers WHERE branch_id=? AND cust_no=?`, i%97, custNo(i)); err != nil {
			return nil, err
		}
		out = append(out, Planted{PK: fmt.Sprintf("%d|%d", i%97, custNo(i)), Kind: "missing"})
		return out, nil
	}
	return tableDef{name: table, srcCreate: srcCreate, tgtCreate: tgtCreate,
		srcRow: srcRow, tgtRow: tgtRow, plant: plant}
}

func exec(ctx context.Context, db *source.Database, q string, args ...any) error {
	_, err := db.DB.ExecContext(ctx, q, args...)
	return err
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func pickDistinct(r *rand.Rand, n, k int) []int {
	return pickDistinctExcluding(r, n, k, nil)
}

func pickDistinctExcluding(r *rand.Rand, n, k int, exclude []int) []int {
	ex := make(map[int]bool, len(exclude))
	for _, v := range exclude {
		ex[v] = true
	}
	seen := map[int]bool{}
	var out []int
	for tries := 0; len(out) < k && tries < k*100+1000; tries++ {
		v := r.Intn(n)
		if ex[v] || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func pickDistinctOffset(r *rand.Rand, n, k, min int) []int {
	seen := map[int]bool{}
	var out []int
	for tries := 0; len(out) < k && tries < k*100+1000; tries++ {
		v := min + r.Intn(n-min)
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// truncateDecimals keeps only `places` fractional digits.  It returns the
// input unchanged when the truncation would not change the numeric value (for
// example 1.2300 -> 1.23), so that a planted difference is always genuine.
func truncateDecimals(s string, places int) string {
	i := strings.Index(s, ".")
	if i < 0 {
		return s
	}
	frac := s[i+1:]
	if len(frac) <= places {
		return s
	}
	for _, c := range frac[places:] {
		if c != '0' {
			if places == 0 {
				return s[:i]
			}
			return s[:i+1+places]
		}
	}
	return s
}

// formatDay renders a value read back from a DATE column.  The SQLite driver
// returns time.Time for DATE declared columns, so the display form has to be
// recovered before re-writing it.
func formatDay(v any) string {
	switch x := v.(type) {
	case time.Time:
		return x.Format("2006-01-02")
	case string:
		if len(x) >= 10 {
			return x[:10]
		}
		return x
	case []byte:
		return formatDay(string(x))
	default:
		return fmt.Sprint(x)
	}
}

func shiftDay(day string, days int) (string, error) {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return "", err
	}
	return t.AddDate(0, 0, days).Format("2006-01-02"), nil
}

func newRnd(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }
