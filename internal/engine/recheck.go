package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"bankcmp/internal/canon"
	"bankcmp/internal/model"
	"bankcmp/internal/plan"
)

// recheck re-reads every differing row after a short settle delay and
// classifies it as stable (a genuine difference) or transient.
//
// The rule is deliberately conservative: a difference is only marked transient
// when there is positive evidence that one of the two rows changed after the
// comparison observed it.  Everything else stays stable, so a genuine lost or
// altered row can never be excused as a transient.
//
//   - missing (source has it, target does not): transient only if the source row
//     was deleted or its content changed.
//   - extra (target has it, source does not): transient only if the target row
//     was deleted/changed, or the source row appeared.
//   - modified: transient only if either side's content changed, appeared or
//     disappeared.
func (r *Runner) recheck(ctx context.Context, tp *plan.TablePlan, diffs []model.Diff) (int, error) {
	if len(diffs) == 0 {
		return 0, nil
	}
	if r.Opts.Settle > 0 {
		if err := sleepCtx(ctx, r.Opts.Settle); err != nil {
			return 0, err
		}
	}
	byKind := map[model.DiffKind][]int{}
	for i := range diffs {
		diffs[i].Stability = model.StabilityStable
		byKind[diffs[i].Kind] = append(byKind[diffs[i].Kind], i)
	}

	// One re-read per side for all rows that need it.
	var needSource, needTarget []int
	for _, kind := range []model.DiffKind{model.KindMissing, model.KindModified} {
		needSource = append(needSource, byKind[kind]...)
	}
	needTarget = append(needTarget, byKind[model.KindExtra]...)
	needTarget = append(needTarget, byKind[model.KindModified]...)
	// An extra row can stop being extra if the source gains the key.
	needSource = append(needSource, byKind[model.KindExtra]...)

	srcNow, err := r.currentRows(ctx, tp, plan.SideSource, diffs, needSource)
	if err != nil {
		return 0, err
	}
	tgtNow, err := r.currentRows(ctx, tp, plan.SideTarget, diffs, needTarget)
	if err != nil {
		return 0, err
	}

	mark := func(i int, note string) {
		diffs[i].Stability = model.StabilityTransient
		diffs[i].Note = note
	}
	for _, i := range byKind[model.KindMissing] {
		v, ok := srcNow[diffs[i].PK]
		switch {
		case !ok:
			mark(i, "source row was deleted during the comparison")
		case v.fp != diffs[i].SourceFP:
			mark(i, "source row changed during the comparison")
		}
	}
	for _, i := range byKind[model.KindExtra] {
		if v, ok := tgtNow[diffs[i].PK]; !ok || v.fp != diffs[i].TargetFP {
			mark(i, "target row changed or disappeared during the comparison")
			continue
		}
		if _, ok := srcNow[diffs[i].PK]; ok {
			mark(i, "source row appeared during the comparison")
		}
	}
	for _, i := range byKind[model.KindModified] {
		sv, sok := srcNow[diffs[i].PK]
		tv, tok := tgtNow[diffs[i].PK]
		switch {
		case !sok:
			mark(i, "source row was deleted during the comparison")
		case !tok:
			mark(i, "target row was deleted during the comparison")
		case sv.fp != diffs[i].SourceFP || tv.fp != diffs[i].TargetFP:
			mark(i, "a row changed during the comparison")
		}
	}

	var changed int
	for i := range diffs {
		if diffs[i].Stability == model.StabilityTransient {
			changed++
		}
	}
	return changed, nil
}

type currentRow struct {
	fp   string
	xmin string
}

// currentRows re-reads the given diffs' keys on one side and returns their
// current fingerprints keyed by canonical primary key.
func (r *Runner) currentRows(ctx context.Context, tp *plan.TablePlan, side plan.Side, diffs []model.Diff, idxs []int) (map[string]currentRow, error) {
	db := r.Src
	if side == plan.SideTarget {
		db = r.Tgt
	}
	out := make(map[string]currentRow, len(idxs))
	// Read in small batches so the query stays inside bind limits and can use
	// the key index.
	const batch = 200
	for start := 0; start < len(idxs); start += batch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + batch
		if end > len(idxs) {
			end = len(idxs)
		}
		q, args := keyLookupQuery(tp, side, diffs, idxs[start:end])
		rows, err := db.DB.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("recheck %s: %w", sideName(side), err)
		}
		st := newSideStream(rows, tp, side, false)
		for st.next(tp, side) {
			fp, _ := rowFingerprintInto(nil, st.cur.Key, st.cur.Vals)
			pk := model.ParsePK(keyValues(tp, st.cur.Key))
			out[pk] = currentRow{fp: string(fp[:]), xmin: st.cur.Xmin}
		}
		err = st.err
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// keyLookupQuery builds a query selecting the compared columns for the given
// diff keys.
func keyLookupQuery(tp *plan.TablePlan, side plan.Side, diffs []model.Diff, idxs []int) (string, []any) {
	var clauses []string
	var args []any
	for _, i := range idxs {
		kvs := diffKeyVals(diffs[i])
		if len(kvs) != len(tp.Keys) {
			continue
		}
		phs := keyPlaceholders(tp, side, len(args)+1)
		clauses = append(clauses, tupleExpr(tp, side)+" = "+tupleRHS(phs))
		args = append(args, keyArgs(kvs)...)
	}
	q := "SELECT " + selectList(tp, side) + " FROM " + tableFor(tp, side)
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " OR ")
	} else {
		q += " WHERE 1=0"
	}
	return q, args
}

func keyValues(tp *plan.TablePlan, kvs []plan.KeyVal) []model.KeyValue {
	out := make([]model.KeyValue, len(kvs))
	for i, k := range kvs {
		s, isNull := plan.FormatKeyVal(k)
		out[i] = model.KeyValue{Column: tp.Keys[i].SourceName, Value: s, Null: isNull, Type: k.Type}
	}
	return out
}

// diffKeyVals reconstructs typed key values from a stored diff.
func diffKeyVals(d model.Diff) []plan.KeyVal {
	out := make([]plan.KeyVal, len(d.PKValues))
	for i, kv := range d.PKValues {
		out[i] = keyValFromStored(kv)
	}
	return out
}

func keyValFromStored(kv model.KeyValue) plan.KeyVal {
	if kv.Null {
		return plan.KeyVal{Type: kv.Type, Null: true}
	}
	return plan.KeyValFromString(kv.Type, kv.Value)
}

var _ = canon.FromDriver
var _ = sql.ErrNoRows

// FullRow is one row read back from a database with complete, untruncated
// canonical values, used by the repair generator so that SQL is never built
// from display previews.
type FullRow struct {
	Key  []plan.KeyVal
	Vals []canon.Value
}

// FetchFullRows re-reads the rows identified by diffs on one side and returns
// their complete values keyed by canonical primary key.
func (r *Runner) FetchFullRows(ctx context.Context, tp *plan.TablePlan, side plan.Side, diffs []model.Diff) (map[string]FullRow, error) {
	db := r.Src
	if side == plan.SideTarget {
		db = r.Tgt
	}
	out := make(map[string]FullRow, len(diffs))
	const batch = 100
	for start := 0; start < len(diffs); start += batch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + batch
		if end > len(diffs) {
			end = len(diffs)
		}
		idx := make([]int, 0, end-start)
		for i := start; i < end; i++ {
			idx = append(idx, i)
		}
		q, args := keyLookupQuery(tp, side, diffs, idx)
		rows, err := db.DB.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("read %s rows: %w", sideName(side), err)
		}
		st := newSideStream(rows, tp, side, false)
		for st.next(tp, side) {
			pk := model.ParsePK(keyValues(tp, st.cur.Key))
			out[pk] = FullRow{Key: st.cur.Key, Vals: st.cur.Vals}
		}
		err = st.err
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
