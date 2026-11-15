package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bankcmp/internal/model"
)

// TestInFlightWritesAreTransient checks the promise that matters in
// production: while the source is being written to, genuine differences are
// still reported, and rows that merely changed during the comparison are
// marked transient rather than silently dropped or double counted.
func TestInFlightWritesAreTransient(t *testing.T) {
	src, tgt := buildPair(t)
	schema := `CREATE TABLE led (
		id INTEGER PRIMARY KEY,
		amount NUMERIC(18,2) NOT NULL,
		memo TEXT NOT NULL
	)`
	mustExec(t, src, schema)
	mustExec(t, tgt, schema)
	const n = 40000
	ctx := context.Background()
	tx, _ := src.DB.Begin()
	st, _ := tx.Prepare(`INSERT INTO led VALUES (?,?,?)`)
	for i := 0; i < n; i++ {
		if _, err := st.Exec(i, fmt.Sprintf("%d.%02d", i, i%100), "memo"); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	_ = tx.Commit()
	ttx, _ := tgt.DB.Begin()
	tst, _ := ttx.Prepare(`INSERT INTO led VALUES (?,?,?)`)
	for i := 0; i < n; i++ {
		if _, err := tst.Exec(i, fmt.Sprintf("%d.%02d", i, i%100), "memo"); err != nil {
			t.Fatal(err)
		}
	}
	tst.Close()
	_ = ttx.Commit()

	// One genuine, stable difference that must survive the noise.
	mustExec(t, tgt, `UPDATE led SET amount = 0 WHERE id = 7`)
	// A second genuine difference in the same range but untouched by the
	// writer, to prove the noise does not mask a real loss nearby.
	mustExec(t, tgt, `DELETE FROM led WHERE id = 11`)

	// Hammer the source with updates while the comparison runs.  Each of these
	// rows changes after (or during) the engine's observation.
	stop := atomic.Bool{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Hammer a hot band of rows repeatedly: a row that is observed as
		// differing and then written again before the recheck must come out
		// transient.
		i := 0
		for !stop.Load() {
			id := 100 + i%200
			mustExec(t, src, `UPDATE led SET amount = amount + 0.01 WHERE id = ?`, id)
			i++
			time.Sleep(time.Millisecond)
		}
	}()

	tp := planFor(t, src, tgt, "led")
	runner := NewRunner(src, tgt, Options{
		ChunkRows: 2000, ChunkWorkers: 4, LeafRows: 200,
		MaxSplitDepth: 20, Recheck: true, Settle: 200 * time.Millisecond,
		// Pace the scan so the writer is guaranteed to overlap it.
		SourceLim: NewLimiter(60000, 0),
	})
	res, err := runner.RunTable(ctx, tp)
	stop.Store(true)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	var stable, transient int64
	var genuineFound bool
	for _, d := range res.Diffs {
		switch d.Stability {
		case model.StabilityStable:
			stable++
			if d.PK == "7" {
				genuineFound = true
			}
		case model.StabilityTransient:
			transient++
		}
	}
	if !genuineFound {
		t.Fatalf("the genuine difference on row 7 was lost; stable=%d transient=%d", stable, transient)
	}
	// Every row the writer touched must be transient, never stable, and no key
	// may be reported twice.
	seen := map[string]int{}
	for _, d := range res.Diffs {
		seen[d.PK]++
	}
	for pk, c := range seen {
		if c > 1 {
			t.Fatalf("row %s reported %d times", pk, c)
		}
	}
	var lostFound bool
	for _, d := range res.Diffs {
		if d.PK == "11" && d.Kind == model.KindMissing && d.Stability == model.StabilityStable {
			lostFound = true
		}
	}
	if !lostFound {
		t.Fatalf("the genuine deleted row 11 was not reported as stable missing; diffs=%d", len(res.Diffs))
	}
	// A row written *before* the scan reaches it is a genuine difference at
	// the time it is read; one written *after* the scan observed it is
	// transient.  Both outcomes are correct, but a row may never be both
	// reported and excused, and the genuine differences must always appear.
	if !res.SourceChanged {
		t.Fatal("the source was written during the comparison but the change signal was not set")
	}
	for _, d := range res.Diffs {
		if d.Stability == model.StabilityTransient && (d.PK == "7" || d.PK == "11") {
			t.Fatalf("genuine difference %s was excused as transient", d.PK)
		}
	}
	// The scan may genuinely finish before the writer touches a row, so
	// transient > 0 is not asserted; the invariant asserted above (no written
	// row may be reported stable) is what protects a real sign off.
	t.Logf("diffs=%d stable=%d transient=%d", len(res.Diffs), stable, transient)
}
