package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"bankcmp/internal/canon"
	"bankcmp/internal/plan"
)

// Chunk is one key range of a table.  A nil Lo means -infinity and a nil Hi
// means +infinity, so keys present on only one side always fall in some chunk.
type Chunk struct {
	Seq         int
	Lo          []plan.KeyVal
	Hi          []plan.KeyVal
	Depth       int
	CountsKnown bool
	SrcRows     int64
	TgtRows     int64
}

// Split returns the midpoint split of a chunk, or false when it cannot be
// split.  The split point is chosen from whichever side has rows, preferring
// a value strictly inside the range.
func (c Chunk) splitAt(mid []plan.KeyVal) (Chunk, Chunk, bool) {
	if len(mid) == 0 {
		return Chunk{}, Chunk{}, false
	}
	if c.Lo != nil && plan.CompareTuple(mid, c.Lo) <= 0 {
		return Chunk{}, Chunk{}, false
	}
	if c.Hi != nil && plan.CompareTuple(mid, c.Hi) >= 0 {
		return Chunk{}, Chunk{}, false
	}
	return Chunk{Lo: c.Lo, Hi: mid, Depth: c.Depth + 1},
		Chunk{Lo: mid, Hi: c.Hi, Depth: c.Depth + 1}, true
}

// KeyExprs returns the ordering expressions for one side.
func KeyExprs(tp *plan.TablePlan, side plan.Side) []string {
	d := dialFor(tp, side)
	out := make([]string, len(tp.Keys))
	for i, k := range tp.Keys {
		out[i] = k.KeyExpression(side, d)
	}
	return out
}

func tableFor(tp *plan.TablePlan, side plan.Side) string {
	if side == plan.SideSource {
		return tp.SourceTable
	}
	return tp.TargetTable
}

func whereFor(tp *plan.TablePlan, side plan.Side) string {
	if side == plan.SideSource {
		return tp.SourceWhere
	}
	return tp.TargetWhere
}

func tupleExpr(tp *plan.TablePlan, side plan.Side) string {
	exprs := KeyExprs(tp, side)
	if len(exprs) == 1 {
		return exprs[0]
	}
	return "(" + strings.Join(exprs, ", ") + ")"
}

func tupleRHS(phs []string) string {
	if len(phs) == 1 {
		return phs[0]
	}
	return "(" + strings.Join(phs, ", ") + ")"
}

func keyPlaceholders(tp *plan.TablePlan, side plan.Side, start int) []string {
	d := dialFor(tp, side)
	out := make([]string, len(tp.Keys))
	for i, k := range tp.Keys {
		out[i] = k.KeyParamCast(side, d, start+i)
	}
	return out
}

func keyArgs(kvs []plan.KeyVal) []any {
	out := make([]any, len(kvs))
	for i := range kvs {
		out[i] = plan.KeyArg(kvs[i])
	}
	return out
}

// rangePredicate returns the WHERE fragment and args restricting a side to ch.
func rangePredicate(tp *plan.TablePlan, side plan.Side, ch Chunk) (string, []any) {
	var parts []string
	var args []any
	if ch.Lo != nil {
		phs := keyPlaceholders(tp, side, len(args)+1)
		parts = append(parts, tupleExpr(tp, side)+" >= "+tupleRHS(phs))
		args = append(args, keyArgs(ch.Lo)...)
	}
	if ch.Hi != nil {
		phs := keyPlaceholders(tp, side, len(args)+1)
		parts = append(parts, tupleExpr(tp, side)+" < "+tupleRHS(phs))
		args = append(args, keyArgs(ch.Hi)...)
	}
	return strings.Join(parts, " AND "), args
}

// selectList projects key expressions followed by compared column expressions.
func selectList(tp *plan.TablePlan, side plan.Side) string {
	d := dialFor(tp, side)
	parts := KeyExprs(tp, side)
	for _, c := range tp.Columns {
		parts = append(parts, c.Expression(side, d))
	}
	return strings.Join(parts, ", ")
}

// scanQuery builds the ordered streaming query for one side of a chunk.
func scanQuery(tp *plan.TablePlan, side plan.Side, ch Chunk) (string, []any) {
	return scanQueryXmin(tp, side, ch, false)
}

// scanQueryXmin optionally appends the PostgreSQL tuple id so the recheck can
// tell whether a row was rewritten after it was observed.
func scanQueryXmin(tp *plan.TablePlan, side plan.Side, ch Chunk, withXmin bool) (string, []any) {
	var conds []string
	var args []any
	if w := strings.TrimSpace(whereFor(tp, side)); w != "" {
		conds = append(conds, "("+w+")")
	}
	if rng, a := rangePredicate(tp, side, ch); rng != "" {
		conds = append(conds, "("+rng+")")
		args = append(args, a...)
	}
	sel := selectList(tp, side)
	if withXmin {
		sel += ", xmin::text"
	}
	q := "SELECT " + sel + " FROM " + tableFor(tp, side)
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY " + strings.Join(KeyExprs(tp, side), ", ")
	return q, args
}

// countQuery counts the rows of a chunk on one side.
func countQuery(tp *plan.TablePlan, side plan.Side, ch Chunk) (string, []any) {
	var conds []string
	var args []any
	if w := strings.TrimSpace(whereFor(tp, side)); w != "" {
		conds = append(conds, "("+w+")")
	}
	if rng, a := rangePredicate(tp, side, ch); rng != "" {
		conds = append(conds, "("+rng+")")
		args = append(args, a...)
	}
	q := "SELECT COUNT(*) FROM " + tableFor(tp, side)
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	return q, args
}

// scanRaws reads n values of the current row into canonical Raws.
func scanRaws(rows *sql.Rows, n int) ([]canon.Raw, error) {
	vals := make([]any, n)
	ptrs := make([]any, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make([]canon.Raw, n)
	for i, v := range vals {
		out[i] = canon.FromDriver(v)
	}
	return out, nil
}

// planChunks samples the source key space and builds sorted, non overlapping
// ranges covering the whole key space.  Sampling happens in the source's own
// index order (cheap, uses the primary key) and the sample is then re-sorted
// into canonical order.
//
// The first range is unbounded below and starts at the smallest sampled key;
// the last is unbounded above.  Every key on either side therefore falls in
// exactly one range, including keys that only exist on the target.
//
// Sampling uses row_number() because it is portable and preserves the
// database's own key order.  On very large tables that window is expensive,
// so a caller that knows the table is huge can raise chunk_rows; the
// comparison itself only ever reads the keys it needs.
func planChunks(ctx context.Context, conn *sql.Conn, tp *plan.TablePlan, chunkRows int) ([]Chunk, error) {
	if chunkRows < 1 {
		chunkRows = 20000
	}
	kp := keyParser{keys: tp.Keys, srcLoc: tp.SourceLoc, tgtLoc: tp.TargetLoc}
	bounds, err := sampleBoundaries(ctx, conn, tp, kp, chunkRows)
	if err != nil {
		return nil, err
	}
	// sort in canonical order
	sortKeyVals(bounds)
	uniq := make([][]plan.KeyVal, 0, len(bounds))
	for _, b := range bounds {
		if len(uniq) > 0 && plan.CompareTuple(uniq[len(uniq)-1], b) == 0 {
			continue
		}
		uniq = append(uniq, b)
	}
	if len(uniq) == 0 {
		return []Chunk{{Seq: 0}}, nil
	}
	chunks := make([]Chunk, 0, len(uniq)+1)
	chunks = append(chunks, Chunk{Seq: 0, Hi: uniq[0]})
	for i := range uniq {
		ch := Chunk{Lo: uniq[i]}
		if i+1 < len(uniq) {
			ch.Hi = uniq[i+1]
		}
		ch.Seq = len(chunks)
		chunks = append(chunks, ch)
	}
	return chunks, nil
}

func sampleBoundaries(ctx context.Context, conn *sql.Conn, tp *plan.TablePlan, kp keyParser, every int) ([][]plan.KeyVal, error) {
	if every < 1 {
		every = 1
	}
	keyCols := KeyExprs(tp, plan.SideSource)
	innerCols := append(append([]string{}, keyCols...),
		fmt.Sprintf("row_number() OVER (ORDER BY %s) AS bc_rn", strings.Join(keyCols, ", ")))
	q := "SELECT " + strings.Join(keyCols, ", ") + " FROM (SELECT " + strings.Join(innerCols, ", ") +
		" FROM " + tableFor(tp, plan.SideSource)
	if w := strings.TrimSpace(whereFor(tp, plan.SideSource)); w != "" {
		q += " WHERE (" + w + ")"
	}
	// The modulo is applied in SQL so only about 1/every keys cross the wire.
	q += ") bc_t WHERE bc_rn % " + fmt.Sprint(every) + " = 1 ORDER BY bc_rn"
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sample keys: %w", err)
	}
	defer rows.Close()
	var out [][]plan.KeyVal
	for rows.Next() {
		raws, err := scanRaws(rows, len(tp.Keys))
		if err != nil {
			return nil, err
		}
		kvs, err := kp.parse(plan.SideSource, raws)
		if err != nil {
			return nil, err
		}
		out = append(out, kvs)
	}
	return out, rows.Err()
}

func sortKeyVals(v [][]plan.KeyVal) {
	// insertion sort is fine: the sample is small relative to the table, and
	// it keeps the comparison function in one place.
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && plan.CompareTuple(v[j], v[j-1]) < 0; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
