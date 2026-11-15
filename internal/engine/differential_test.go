package engine

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"bankcmp/internal/model"
)

// TestDifferentialAgainstNaiveJoin checks the engine against the obvious but
// slow definition of a difference: a full join in SQL.  Random mutations are
// applied to the target, and the engine's per kind counts must equal what the
// join says, for every seed.  This catches any range/aggregate bug that a
// hand written case would miss.
func TestDifferentialAgainstNaiveJoin(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			runDifferential(t, seed)
		})
	}
}

func runDifferential(t *testing.T, seed int64) {
	t.Helper()
	ctx := context.Background()
	src, tgt := buildPair(t)
	schema := `CREATE TABLE acct (
		id INTEGER PRIMARY KEY, cust_no INTEGER NOT NULL, kind TEXT NOT NULL,
		balance NUMERIC(18,4) NOT NULL, opened TEXT NOT NULL)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	const n = 1200
	for i := 0; i < n; i++ {
		vals := []any{i, 1000 + i%37, []string{"SAV", "CHK"}[i%2],
			fmt.Sprintf("%d.%04d", i*3, i%10000), fmt.Sprintf("2024-%02d-%02d", 1+i%12, 1+i%28)}
		mustExec(t, src, `INSERT INTO acct VALUES (?,?,?,?,?)`, vals...)
		mustExec(t, tgt, `INSERT INTO acct VALUES (?,?,?,?,?)`, vals...)
	}
	rnd := rand.New(rand.NewSource(seed))
	perm := rnd.Perm(n)
	for _, id := range perm[:60] {
		mustExec(t, tgt, `DELETE FROM acct WHERE id=?`, id)
	}
	for _, id := range perm[60:120] {
		mustExec(t, tgt, `INSERT INTO acct VALUES (?,?,?,?,?)`, id+50000, 7, "SAV", "1.0000", "2024-01-01")
	}
	for _, id := range perm[120:220] {
		mustExec(t, tgt, `UPDATE acct SET balance=balance+1 WHERE id=?`, id)
	}

	// Ground truth: read both sides fully and compare row by row.  The two
	// databases are separate files, so this is the naive definition of a
	// difference done in Go rather than SQL.
	var missing, extra, modified int64
	srcSet := map[int64]string{}
	rows, err := src.DB.QueryContext(ctx, `SELECT id, cust_no||'|'||kind||'|'||printf('%.4f',balance)||'|'||opened FROM acct`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		srcSet[id] = v
	}
	rows.Close()
	tgtSet := map[int64]string{}
	rows, err = tgt.DB.QueryContext(ctx, `SELECT id, cust_no||'|'||kind||'|'||printf('%.4f',balance)||'|'||opened FROM acct`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		tgtSet[id] = v
	}
	rows.Close()
	for id, sv := range srcSet {
		tv, ok := tgtSet[id]
		if !ok {
			missing++
		} else if tv != sv {
			modified++
		}
	}
	for id := range tgtSet {
		if _, ok := srcSet[id]; !ok {
			extra++
		}
	}

	tp := planFor(t, src, tgt, "acct")
	res, err := NewRunner(src, tgt, Options{
		ChunkRows: 100, ChunkWorkers: 3, Recheck: false, LeafRows: 32,
		DiffMaxRows: 25, MaxSplitDepth: 30,
	}).RunTable(ctx, tp)
	if err != nil {
		t.Fatal(err)
	}
	var gMissing, gExtra, gModified int64
	for _, d := range res.Diffs {
		switch d.Kind {
		case model.KindMissing:
			gMissing++
		case model.KindExtra:
			gExtra++
		case model.KindModified:
			gModified++
		}
	}
	if gMissing != missing || gExtra != extra || gModified != modified {
		t.Fatalf("engine (%d,%d,%d) != naive join (%d,%d,%d)",
			gMissing, gExtra, gModified, missing, extra, modified)
	}
	if int64(len(res.Diffs)) != missing+extra+modified {
		t.Fatalf("total %d != sum %d", len(res.Diffs), missing+extra+modified)
	}
	// Row identity must match exactly, not just the counts.
	got := map[string]bool{}
	for _, d := range res.Diffs {
		got[fmt.Sprintf("%s/%s", d.Kind, d.PK)] = true
	}
	for id, sv := range srcSet {
		tv, ok := tgtSet[id]
		key := fmt.Sprintf("%s/%d", "missing", id)
		if ok {
			if tv != sv {
				key = fmt.Sprintf("%s/%d", "modified", id)
			} else {
				continue
			}
		}
		if !got[key] {
			t.Fatalf("missing expected difference %s", key)
		}
	}
}
