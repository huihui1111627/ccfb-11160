package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bankcmp/internal/dialect"
	"bankcmp/internal/model"
	"bankcmp/internal/plan"
	"bankcmp/internal/source"
)

// Options are the engine level knobs.
type Options struct {
	Workers       int
	ChunkWorkers  int
	ChunkRows     int
	DiffMaxRows   int
	LeafRows      int
	MaxSplitDepth int
	Recheck       bool
	Settle        time.Duration
	Progress      func(Progress)
	SourceLim     *Limiter
	TargetLim     *Limiter
}

// Runner compares tables between two databases.
type Runner struct {
	Src       *source.Database
	Tgt       *source.Database
	Opts      Options
	SourceLim *Limiter
	TargetLim *Limiter
}

// NewRunner builds a runner with sane defaults filled in.
func NewRunner(src, tgt *source.Database, opts Options) *Runner {
	if opts.ChunkWorkers <= 0 {
		opts.ChunkWorkers = 1
	}
	if opts.ChunkRows <= 0 {
		opts.ChunkRows = 20000
	}
	if opts.DiffMaxRows <= 0 {
		opts.DiffMaxRows = 50000
	}
	if opts.LeafRows <= 0 {
		opts.LeafRows = 4096
	}
	if opts.MaxSplitDepth <= 0 {
		opts.MaxSplitDepth = 12
	}
	if opts.Settle <= 0 {
		opts.Settle = 300 * time.Millisecond
	}
	return &Runner{Src: src, Tgt: tgt, Opts: opts,
		SourceLim: opts.SourceLim, TargetLim: opts.TargetLim}
}

// TableResult is the outcome of comparing one table.
type TableResult struct {
	TableName  string
	Status     model.TableStatus
	SourceRows int64
	TargetRows int64
	Chunks     []model.ChunkResult
	Diffs      []model.Diff
	Stable     int64
	Transient  int64
	Error      string
	Duration   time.Duration
	// ChangeBefore/After are source change tokens around the compare.
	ChangeBefore string
	ChangeAfter  string
	// SourceChanged reports whether the source was written to while the table
	// was being compared.  When false, the difference list for this table was
	// produced against a quiescent source and needs no stability caveat.
	SourceChanged bool
	// TargetChanged is the same signal for the target database.
	TargetChanged bool
	// ChangeBeforeTarget is internal: the target token captured at start.
	ChangeBeforeTarget string
}

// RunTable compares one table.  Diffs are buffered in the result (bounded by
// MaxDiffStore at the caller); stability is classified by a recheck pass when
// enabled.
func (r *Runner) RunTable(ctx context.Context, tp *plan.TablePlan) (*TableResult, error) {
	start := time.Now()
	res := &TableResult{TableName: tp.Name, Status: model.TableRunning}
	defer func() { res.Duration = time.Since(start) }()

	// Capture a source change token before sampling so a row that is written
	// during the compare can be told apart from a real difference.
	res.ChangeBefore, _ = r.changeToken(ctx)
	res.ChangeBeforeTarget, _ = r.changeTokenSide(ctx, plan.SideTarget)

	pconn, err := r.Src.Conn(ctx)
	if err != nil {
		return res, err
	}
	chunks, err := planChunks(ctx, pconn, tp, tp.ChunkRows)
	pconn.Close()
	if err != nil {
		return res, fmt.Errorf("plan chunks: %w", err)
	}

	var mu sync.Mutex
	var srcRows, tgtRows int64
	var chunkResults []model.ChunkResult
	var diffs []model.Diff
	var scanned int64
	var failed atomic.Bool
	var firstErr error
	setErr := func(e error) {
		if e == nil {
			return
		}
		mu.Lock()
		if firstErr == nil {
			firstErr = e
		}
		mu.Unlock()
		failed.Store(true)
	}

	jobs := make(chan Chunk, len(chunks))
	var wg sync.WaitGroup
	workerN := r.Opts.ChunkWorkers
	if workerN > len(chunks)+1 {
		workerN = len(chunks) + 1
	}
	for i := 0; i < workerN; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ch := range jobs {
				if ctx.Err() != nil || failed.Load() {
					continue
				}
				out, err := r.compareChunk(ctx, tp, ch)
				if err != nil {
					if ctx.Err() != nil {
						continue
					}
					setErr(fmt.Errorf("chunk %d: %w", ch.Seq, err))
					continue
				}
				mu.Lock()
				srcRows += out.srcRows
				tgtRows += out.tgtRows
				scanned += out.scanned
				chunkResults = append(chunkResults, out.chunks...)
				diffs = append(diffs, out.diffs...)
				mu.Unlock()
				if r.Opts.Progress != nil {
					mu.Lock()
					p := Progress{ChunksTotal: len(chunks), Diffs: int64(len(diffs)), RowsScanned: scanned, Phase: "compare"}
					for _, c := range chunkResults {
						if c.Depth == 0 {
							p.ChunksDone++
							if !c.Matched {
								p.ChunksDirty++
							}
						}
					}
					mu.Unlock()
					r.Opts.Progress(p)
				}
			}
		}()
	}
	for _, ch := range chunks {
		jobs <- ch
	}
	close(jobs)
	wg.Wait()

	sort.SliceStable(chunkResults, func(i, j int) bool {
		if chunkResults[i].Depth != chunkResults[j].Depth {
			return chunkResults[i].Depth < chunkResults[j].Depth
		}
		return chunkResults[i].Seq < chunkResults[j].Seq
	})
	res.SourceRows, res.TargetRows = srcRows, tgtRows
	res.Chunks, res.Diffs = chunkResults, diffs
	res.ChangeAfter, _ = r.changeToken(ctx)
	res.SourceChanged = tokensDiffer(res.ChangeBefore, res.ChangeAfter)
	if tok, err := r.changeTokenSide(ctx, plan.SideTarget); err == nil {
		res.TargetChanged = tokensDiffer(res.ChangeBeforeTarget, tok)
	}

	if firstErr != nil {
		res.Status = model.TableFailed
		res.Error = firstErr.Error()
		return res, firstErr
	}
	if ctx.Err() != nil {
		res.Status = model.TableCanceled
		return res, ctx.Err()
	}

	if r.Opts.Recheck && len(diffs) > 0 {
		changed, err := r.recheck(ctx, tp, res.Diffs)
		if err != nil && ctx.Err() == nil {
			// A recheck failure must not lose the difference list; report the
			// diffs as pending and note the problem.
			res.Error = "recheck: " + err.Error()
		}
		_ = changed
	}
	res.Stable, res.Transient = countStability(res.Diffs)

	if len(diffs) == 0 {
		res.Status = model.TableEqual
	} else {
		res.Status = model.TableDiff
	}
	return res, nil
}

func countStability(diffs []model.Diff) (stable, transient int64) {
	for _, d := range diffs {
		switch d.Stability {
		case model.StabilityStable:
			stable++
		case model.StabilityTransient:
			transient++
		}
	}
	return
}

// changeToken returns a value that changes when the source is written to.
func (r *Runner) changeToken(ctx context.Context) (string, error) {
	return r.changeTokenSide(ctx, plan.SideSource)
}

// changeTokenSide reads a write activity token from one database.
//
// PostgreSQL: pg_current_snapshot changes when any transaction commits, so
// comparing the token before and after a table's comparison detects writes
// without needing to know which rows changed.
//
// SQLite: PRAGMA data_version changes when another connection commits, which
// is exactly the "someone else wrote" signal needed.  Its value rolls within
// one process, so it is only used as an inequality check.
func (r *Runner) changeTokenSide(ctx context.Context, side plan.Side) (string, error) {
	db := r.Src
	if side == plan.SideTarget {
		db = r.Tgt
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var v string
	switch db.Driver {
	case model.DriverPostgres:
		if err := db.DB.QueryRowContext(ctx, "SELECT pg_current_snapshot()::text").Scan(&v); err != nil {
			return "", err
		}
	case model.DriverSQLite:
		var n int64
		if err := db.DB.QueryRowContext(ctx, "PRAGMA data_version").Scan(&n); err != nil {
			return "", err
		}
		v = fmt.Sprintf("%d", n)
	default:
		return "", nil
	}
	return v, nil
}

func tokensDiffer(a, b string) bool {
	return a != "" && b != "" && a != b
}

type chunkOutcome struct {
	srcRows, tgtRows int64
	scanned          int64
	chunks           []model.ChunkResult
	diffs            []model.Diff
}

// compareChunk runs the cheap aggregate comparison and, on mismatch, drills
// down until the differing rows can be enumerated exactly.
func (r *Runner) compareChunk(ctx context.Context, tp *plan.TablePlan, ch Chunk) (chunkOutcome, error) {
	var out chunkOutcome
	t0 := time.Now()
	srcAgg, tgtAgg, err := r.aggregate(ctx, tp, ch)
	if err != nil {
		return out, err
	}
	out.srcRows = srcAgg.Rows
	out.tgtRows = tgtAgg.Rows
	out.scanned = srcAgg.Rows + tgtAgg.Rows
	ch.SrcRows, ch.TgtRows = srcAgg.Rows, tgtAgg.Rows
	cr := model.ChunkResult{
		Seq: ch.Seq, Depth: ch.Depth,
		SourceRows: srcAgg.Rows, TargetRows: tgtAgg.Rows,
		Matched:    srcAgg.equal(tgtAgg),
		DurationMS: time.Since(t0).Milliseconds(),
		Lo:         chunkBound(tp, ch.Lo), Hi: chunkBound(tp, ch.Hi),
	}
	if cr.Matched {
		out.chunks = append(out.chunks, cr)
		return out, nil
	}
	// Mismatch: enumerate the exact differences for this range, splitting it
	// when the range is too large or contains too many differences.
	sub, err := r.refine(ctx, tp, ch)
	if err != nil {
		return out, err
	}
	cr.Diffs = int64(len(sub.diffs))
	cr.DurationMS = time.Since(t0).Milliseconds()
	out.chunks = append(out.chunks, cr)
	out.chunks = append(out.chunks, sub.chunks...)
	out.diffs = sub.diffs
	out.scanned += sub.scanned
	// Refinement may discover additional rows on either side; the counts from
	// the aggregate pass already cover the whole range, so keep those.
	return out, nil
}

// aggregate computes both sides' chunk aggregates.
func (r *Runner) aggregate(ctx context.Context, tp *plan.TablePlan, ch Chunk) (chunkAgg, chunkAgg, error) {
	srcAgg, err := r.aggregateOne(ctx, tp, plan.SideSource, ch)
	if err != nil {
		return chunkAgg{}, chunkAgg{}, err
	}
	tgtAgg, err := r.aggregateOne(ctx, tp, plan.SideTarget, ch)
	if err != nil {
		return chunkAgg{}, chunkAgg{}, err
	}
	return srcAgg, tgtAgg, nil
}

func (r *Runner) aggregateOne(ctx context.Context, tp *plan.TablePlan, side plan.Side, ch Chunk) (chunkAgg, error) {
	db := r.Src
	lim := r.SourceLim
	if side == plan.SideTarget {
		db = r.Tgt
		lim = r.TargetLim
	}
	q, args := scanQuery(tp, side, ch)
	conn, err := db.Conn(ctx)
	if err != nil {
		return chunkAgg{}, err
	}
	defer conn.Close()
	if lim != nil {
		if err := lim.Wait(ctx, 1); err != nil {
			return chunkAgg{}, err
		}
	}
	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return chunkAgg{}, fmt.Errorf("%s query: %w", sideName(side), err)
	}
	return aggregateSide(ctx, tp, rows, side, lim)
}

// chunkBound renders a chunk bound for the UI.
func chunkBound(tp *plan.TablePlan, kv []plan.KeyVal) string {
	if len(kv) == 0 {
		return ""
	}
	parts := make([]string, len(kv))
	for i, k := range kv {
		parts[i], _ = plan.FormatKeyVal(k)
	}
	return strings.Join(parts, ", ")
}

func sideName(s plan.Side) string {
	if s == plan.SideSource {
		return "source"
	}
	return "target"
}

type refineOutcome struct {
	chunks  []model.ChunkResult
	diffs   []model.Diff
	scanned int64
}

// refine enumerates differences in a mismatching chunk, splitting it when it
// is too large to merge directly or when it contains too many differences.
func (r *Runner) refine(ctx context.Context, tp *plan.TablePlan, ch Chunk) (refineOutcome, error) {
	var out refineOutcome
	// A leaf small enough is always merged exactly.
	small := ch.SrcRows+ch.TgtRows <= int64(r.Opts.LeafRows)
	if ch.Depth >= r.Opts.MaxSplitDepth {
		small = true
	}
	if !small {
		mid, ok, err := r.splitPoint(ctx, tp, ch)
		if err != nil {
			return out, err
		}
		// Remember the counts for the children when the split point came from
		// the aggregate pass (it is only an estimate, so children are still
		// aggregated before being merged).
		if ok {
			if left, right, ok2 := ch.splitAt(mid); ok2 {
				var childDiffs int64
				for _, sub := range []Chunk{left, right} {
					sub.Seq = ch.Seq
					srcAgg, tgtAgg, err := r.aggregate(ctx, tp, sub)
					if err != nil {
						return out, err
					}
					out.scanned += srcAgg.Rows + tgtAgg.Rows
					sub.SrcRows, sub.TgtRows = srcAgg.Rows, tgtAgg.Rows
					child := model.ChunkResult{
						Seq: ch.Seq, Depth: sub.Depth, SourceRows: srcAgg.Rows, TargetRows: tgtAgg.Rows,
						Matched: srcAgg.equal(tgtAgg),
						Lo:      chunkBound(tp, sub.Lo), Hi: chunkBound(tp, sub.Hi),
					}
					if child.Matched {
						out.chunks = append(out.chunks, child)
						continue
					}
					subRes, err := r.refine(ctx, tp, sub)
					if err != nil {
						return out, err
					}
					child.Diffs = int64(len(subRes.diffs))
					out.chunks = append(out.chunks, child)
					out.chunks = append(out.chunks, subRes.chunks...)
					out.diffs = append(out.diffs, subRes.diffs...)
					out.scanned += subRes.scanned
					childDiffs += int64(len(subRes.diffs))
				}
				_ = childDiffs
				return out, nil
			}
		}
	}
	// Merge exactly.  A very dense range is split further instead of being
	// materialised in one go, which keeps memory bounded.
	diffs, scanned, err := r.mergeRange(ctx, tp, ch, r.Opts.DiffMaxRows)
	if err == errTooDense {
		mid, ok, err2 := r.splitPoint(ctx, tp, ch)
		if err2 != nil {
			return out, err2
		}
		if ok {
			if left, right, ok2 := ch.splitAt(mid); ok2 {
				for _, sub := range []Chunk{left, right} {
					subRes, err := r.refine(ctx, tp, sub)
					if err != nil {
						return out, err
					}
					out.diffs = append(out.diffs, subRes.diffs...)
					out.chunks = append(out.chunks, subRes.chunks...)
					out.scanned += subRes.scanned
				}
				return out, nil
			}
		}
		return out, fmt.Errorf("range contains more than %d differences and cannot be split further; raise diff_max_rows", r.Opts.DiffMaxRows)
	}
	if err != nil {
		return out, err
	}
	out.diffs = diffs
	out.scanned += scanned
	return out, nil
}

// splitPoint finds a key strictly inside the chunk, preferring the side with
// rows and falling back to a synthetic midpoint of the bounds.
func (r *Runner) splitPoint(ctx context.Context, tp *plan.TablePlan, ch Chunk) ([]plan.KeyVal, bool, error) {
	for _, side := range []plan.Side{plan.SideSource, plan.SideTarget} {
		kv, ok, err := r.medianKey(ctx, tp, side, ch)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			continue
		}
		if ch.Lo != nil && plan.CompareTuple(kv, ch.Lo) <= 0 {
			continue
		}
		if ch.Hi != nil && plan.CompareTuple(kv, ch.Hi) >= 0 {
			continue
		}
		return kv, true, nil
	}
	// No row median: try a synthetic midpoint component by component.
	if mid, ok := syntheticMid(ch); ok {
		return mid, true, nil
	}
	return nil, false, nil
}

// medianKey returns the middle key of one side inside the range.
func (r *Runner) medianKey(ctx context.Context, tp *plan.TablePlan, side plan.Side, ch Chunk) ([]plan.KeyVal, bool, error) {
	db := r.Src
	if side == plan.SideTarget {
		db = r.Tgt
	}
	keyCols := KeyExprs(tp, side)
	var conds []string
	var args []any
	if w := strings.TrimSpace(whereFor(tp, side)); w != "" {
		conds = append(conds, "("+w+")")
	}
	if rng, a := rangePredicate(tp, side, ch); rng != "" {
		conds = append(conds, "("+rng+")")
		args = append(args, a...)
	}
	base := " FROM " + tableFor(tp, side)
	if len(conds) > 0 {
		base += " WHERE " + strings.Join(conds, " AND ")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	// Count first, then offset by half.  Two statements keep the placeholders
	// dialect safe: PostgreSQL numbers them ($1..$n) so a repeated subquery
	// would need the same parameters twice, which is easy to get wrong.
	var total int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*)"+base, args...).Scan(&total); err != nil {
		return nil, false, err
	}
	if total == 0 {
		return nil, false, nil
	}
	q := "SELECT " + strings.Join(keyCols, ", ") + base + " ORDER BY " + strings.Join(keyCols, ", ") +
		" LIMIT 1 OFFSET " + strconv.FormatInt(total/2, 10)
	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, false, rows.Err()
	}
	raws, err := scanRaws(rows, len(tp.Keys))
	if err != nil {
		return nil, false, err
	}
	kp := keyParser{keys: tp.Keys, srcLoc: tp.SourceLoc, tgtLoc: tp.TargetLoc}
	kvs, err := kp.parse(side, raws)
	if err != nil {
		return nil, false, err
	}
	return kvs, true, nil
}

func syntheticMid(ch Chunk) ([]plan.KeyVal, bool) {
	if ch.Lo == nil && ch.Hi == nil {
		return nil, false
	}
	n := len(ch.Lo)
	if ch.Hi != nil && len(ch.Hi) < n {
		n = len(ch.Hi)
	}
	if n == 0 {
		return nil, false
	}
	out := make([]plan.KeyVal, n)
	for i := 0; i < n; i++ {
		if ch.Lo != nil && ch.Hi != nil {
			if v, ok := plan.Midpoint(ch.Lo[i], ch.Hi[i]); ok {
				out[i] = v
				copy(out[:i], ch.Lo[:i])
				return out, true
			}
			continue
		}
		// Half open range: step one unit above the lower bound.
		if ch.Lo != nil {
			up, ok := plan.Successor(ch.Lo[i])
			if !ok {
				continue
			}
			out[i] = up
			copy(out[:i], ch.Lo[:i])
			return out, true
		}
	}
	return nil, false
}

// mergeRange streams both sides and returns exact differences.
func (r *Runner) mergeRange(ctx context.Context, tp *plan.TablePlan, ch Chunk, maxRows int) ([]model.Diff, int64, error) {
	withXmin := r.Src.Driver == model.DriverPostgres
	srcQ, srcArgs := scanQueryXmin(tp, plan.SideSource, ch, withXmin)
	tgtQ, tgtArgs := scanQueryXmin(tp, plan.SideTarget, ch, r.Tgt.Driver == model.DriverPostgres)
	srcConn, err := r.Src.Conn(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer srcConn.Close()
	tgtConn, err := r.Tgt.Conn(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer tgtConn.Close()
	if r.SourceLim != nil {
		if err := r.SourceLim.Wait(ctx, 1); err != nil {
			return nil, 0, err
		}
	}
	if r.TargetLim != nil {
		if err := r.TargetLim.Wait(ctx, 1); err != nil {
			return nil, 0, err
		}
	}
	srcRows, err := srcConn.QueryContext(ctx, srcQ, srcArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("source query: %w", err)
	}
	tgtRows, err := tgtConn.QueryContext(ctx, tgtQ, tgtArgs...)
	if err != nil {
		srcRows.Close()
		return nil, 0, fmt.Errorf("target query: %w", err)
	}
	var diffs []model.Diff
	collect := func(d model.Diff) error {
		diffs = append(diffs, d)
		return nil
	}
	if r.SourceLim == nil && r.TargetLim == nil {
		// stream without extra accounting
	}
	found, err := mergeChunk(ctx, tp, srcRows, tgtRows,
		withXmin, r.Tgt.Driver == model.DriverPostgres, collect, maxRows,
		r.SourceLim, r.TargetLim)
	if err == errTooDense {
		return nil, 0, errTooDense
	}
	if err != nil {
		return nil, 0, err
	}
	// Report rows examined (both sides) for pacing statistics.
	return diffs, found, nil
}

var _ = dialect.SQLite{}
var _ = sql.ErrNoRows
