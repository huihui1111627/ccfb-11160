package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"bankcmp/internal/model"
)

// TestRecheckClassifiesStableAndTransient proves the rule used to separate a
// genuine difference from a row that changed while the comparison was running:
// only a row whose observed content changed afterwards is transient.
func TestRecheckClassifiesStableAndTransient(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE t (id INTEGER PRIMARY KEY, v INTEGER, s TEXT)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	for i := 0; i < 20; i++ {
		mustExec(t, src, `INSERT INTO t VALUES (?,?,?)`, i, i, fmt.Sprintf("s%d", i))
		mustExec(t, tgt, `INSERT INTO t VALUES (?,?,?)`, i, i, fmt.Sprintf("s%d", i))
	}
	// Genuine differences: target lost row 1, gained row 100, row 2 has a
	// different value.  None of these rows are touched afterwards.
	mustExec(t, tgt, `DELETE FROM t WHERE id=1`)
	mustExec(t, tgt, `INSERT INTO t VALUES (100, 1, 'x')`)
	mustExec(t, tgt, `UPDATE t SET v=999 WHERE id=2`)

	tp := planFor(t, src, tgt, "t")
	runner := NewRunner(src, tgt, Options{ChunkRows: 4, Recheck: false})
	res, err := runner.RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diffs) != 3 {
		t.Fatalf("expected 3 diffs, got %d", len(res.Diffs))
	}
	// Now simulate a source write racing the comparison: row 5 is the "lost"
	// row on the target, but the source changes it after the observation, so
	// the stored difference is stale and must be classified transient.
	mustExec(t, tgt, `DELETE FROM t WHERE id=5`)
	// Re-run against the new target state so the diff carries a fresh
	// fingerprint, then mutate the source.
	res2, err := runner.RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	var bpk string
	for _, d := range res2.Diffs {
		if d.Kind == model.KindMissing {
			bpk = d.PK
			break
		}
	}
	if bpk == "" {
		t.Fatal("no missing diff found")
	}
	mustExec(t, src, `UPDATE t SET v=v+1000 WHERE id=?`, bpk)
	changed, err := runner.recheck(context.Background(), tp, res2.Diffs)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("expected 1 transient row, got %d", changed)
	}
	byPK := map[string]model.Diff{}
	for _, d := range res2.Diffs {
		byPK[d.PK] = d
	}
	if byPK[bpk].Stability != model.StabilityTransient {
		t.Fatalf("changed source row should be transient, got %s", byPK[bpk].Stability)
	}
	if got := byPK["2"]; got.Stability != model.StabilityStable {
		t.Fatalf("untouched modified row should stay stable, got %s", got.Stability)
	}
}

// TestLimiterPacesRows verifies the rate limiter never exceeds the configured
// budget by more than the small allowed burst.
func TestLimiterPacesRows(t *testing.T) {
	l := NewLimiter(2000, 0)
	start := time.Now()
	if err := l.Wait(context.Background(), 2000); err != nil {
		t.Fatal(err)
	}
	// Second batch of 2000 rows must wait about a second.
	if err := l.Wait(context.Background(), 2000); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < 700*time.Millisecond {
		t.Fatalf("limiter too fast: %s for 4000 rows at 2000/s", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("limiter too slow: %s", elapsed)
	}
}

func TestLimiterCancel(t *testing.T) {
	l := NewLimiter(10, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := l.Wait(ctx, 1000)
	if err == nil {
		t.Fatal("expected context error")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancel took too long")
	}
}

// TestExtraRowTransientOnlyWhenItChanged proves the other direction: a target
// row that was present when the comparison ran, and is still present and
// unchanged afterwards, stays a stable extra even though the source is busy.
func TestExtraRowNotMaskedByUnrelatedWrites(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	for i := 0; i < 10; i++ {
		mustExec(t, src, `INSERT INTO t VALUES (?,?)`, i, "a")
		mustExec(t, tgt, `INSERT INTO t VALUES (?,?)`, i, "a")
	}
	mustExec(t, tgt, `INSERT INTO t VALUES (100,'extra')`)
	tp := planFor(t, src, tgt, "t")
	runner := NewRunner(src, tgt, Options{ChunkRows: 3, Recheck: false})
	res, err := runner.RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	// Unrelated source writes must not affect the extra row's classification.
	mustExec(t, src, `UPDATE t SET v='b' WHERE id=2`)
	mustExec(t, src, `INSERT INTO t VALUES (200,'x')`)
	if _, err := runner.recheck(context.Background(), tp, res.Diffs); err != nil {
		t.Fatal(err)
	}
	for _, d := range res.Diffs {
		if d.PK == "100" && d.Stability != model.StabilityStable {
			t.Fatalf("unchanged extra row marked %s: %s", d.Stability, d.Note)
		}
	}
}

// TestSourceRegainsExtraKey: an extra row wholesale disappears from the target
// during the comparison, which means nothing was actually wrong; it must be
// transient.
func TestExtraRowDeletedFromTargetIsTransient(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	for i := 0; i < 5; i++ {
		mustExec(t, src, `INSERT INTO t VALUES (?,?)`, i, "a")
		mustExec(t, tgt, `INSERT INTO t VALUES (?,?)`, i, "a")
	}
	mustExec(t, tgt, `INSERT INTO t VALUES (100,'extra')`)
	tp := planFor(t, src, tgt, "t")
	runner := NewRunner(src, tgt, Options{ChunkRows: 3, Recheck: false})
	res, err := runner.RunTable(context.Background(), tp)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, tgt, `DELETE FROM t WHERE id=100`)
	if _, err := runner.recheck(context.Background(), tp, res.Diffs); err != nil {
		t.Fatal(err)
	}
	if len(res.Diffs) != 1 || res.Diffs[0].Stability != model.StabilityTransient {
		t.Fatalf("expected transient, got %+v", res.Diffs)
	}
}
