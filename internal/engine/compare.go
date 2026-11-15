package engine

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"bankcmp/internal/canon"
	"bankcmp/internal/model"
	"bankcmp/internal/plan"
)

// rowValues is one side's row: parsed key plus one canonical value per
// compared column.
type rowValues struct {
	Key  []plan.KeyVal
	Vals []canon.Value
	// Xmin is the PostgreSQL tuple transaction id, used to tell whether a row
	// was modified during the comparison.
	Xmin string
}

// scanSide streams one side of a chunk in key order.
type sideStream struct {
	rows    *sql.Rows
	nkeys   int
	ncols   int
	hasXmin bool
	kp      keyParser
	norms   []*canon.Normalizer
	cur     rowValues
	valid   bool
	err     error
	// pacing charges rows against the limiter as they are scanned.
	pacing *limiterPacer
}

// limiterPacer throttles a stream by rows actually read, so a 10 million row
// table cannot flood past the configured rows per second even though the
// driver fetches in batches.
type limiterPacer struct {
	lim   *Limiter
	ctx   context.Context
	batch int
	n     int
}

func (p *limiterPacer) charge() error {
	if p == nil || p.lim == nil {
		return nil
	}
	p.n++
	if p.n < p.batch {
		return nil
	}
	n := p.n
	p.n = 0
	return p.lim.Wait(p.ctx, n)
}

func (p *limiterPacer) flush() error {
	if p == nil || p.lim == nil || p.n == 0 {
		return nil
	}
	n := p.n
	p.n = 0
	return p.lim.Wait(p.ctx, n)
}

func newSideStream(rows *sql.Rows, tp *plan.TablePlan, side plan.Side, withXmin bool) *sideStream {
	return newPacedStream(rows, tp, side, withXmin, nil)
}

func newPacedStream(rows *sql.Rows, tp *plan.TablePlan, side plan.Side, withXmin bool, pacer *limiterPacer) *sideStream {
	var norms []*canon.Normalizer
	for _, c := range tp.Columns {
		if side == plan.SideSource {
			norms = append(norms, c.SrcNorm)
		} else {
			norms = append(norms, c.TgtNorm)
		}
	}
	return &sideStream{
		rows: rows, nkeys: len(tp.Keys), ncols: len(tp.Columns), hasXmin: withXmin,
		kp:    keyParser{keys: tp.Keys, srcLoc: tp.SourceLoc, tgtLoc: tp.TargetLoc},
		norms: norms, pacing: pacer,
	}
}

// next advances the stream.  It returns false at end of rows or on error.
func (s *sideStream) next(tp *plan.TablePlan, side plan.Side) bool {
	if !s.rows.Next() {
		s.valid = false
		s.err = s.rows.Err()
		return false
	}
	total := s.nkeys + s.ncols
	if s.hasXmin {
		total++
	}
	raws, err := scanRaws(s.rows, total)
	if err != nil {
		s.valid = false
		s.err = err
		return false
	}
	kvs, err := s.kp.parse(side, raws[:s.nkeys])
	if err != nil {
		s.valid = false
		s.err = err
		return false
	}
	vals := make([]canon.Value, s.ncols)
	for i := 0; i < s.ncols; i++ {
		v, err := s.norms[i].Normalize(raws[s.nkeys+i])
		if err != nil {
			s.valid = false
			s.err = fmt.Errorf("column %q: %w", columnName(tp, side, i), err)
			return false
		}
		vals[i] = v
	}
	if err := s.pacing.charge(); err != nil {
		s.valid = false
		s.err = err
		return false
	}
	s.cur = rowValues{Key: kvs, Vals: vals}
	if s.hasXmin {
		r := raws[s.nkeys+s.ncols]
		if !r.Null {
			if r.Kind == 'b' {
				s.cur.Xmin = string(r.B)
			} else {
				s.cur.Xmin = keyText(r)
			}
		}
	}
	s.valid = true
	return true
}

func columnName(tp *plan.TablePlan, side plan.Side, i int) string {
	if side == plan.SideSource {
		return tp.Columns[i].SourceName
	}
	return tp.Columns[i].TargetName
}

// rowFingerprintInto hashes key and values with type/length framing so that
// concatenation ambiguity can never hide a difference.
func rowFingerprintInto(buf []byte, key []plan.KeyVal, vals []canon.Value) ([16]byte, []byte) {
	buf = buf[:0]
	buf = appendKeyVals(buf, key)
	for _, v := range vals {
		buf = appendValue(buf, v)
	}
	return hash128(buf), buf
}

func appendKeyVals(buf []byte, key []plan.KeyVal) []byte {
	for _, k := range key {
		buf = append(buf, byte(len(k.Type)), 1)
		buf = append(buf, k.Type...)
		if k.Null {
			buf = append(buf, 0)
			continue
		}
		buf = append(buf, 1)
		buf = appendKeyValPayload(buf, k)
	}
	return buf
}

func appendKeyValPayload(buf []byte, k plan.KeyVal) []byte {
	switch k.Type {
	case model.TypeInt:
		buf = appendUvarint(buf, uint64(k.Int))
	case model.TypeDecimal, model.TypeFloat:
		buf = appendString(buf, k.Dec.String())
	case model.TypeDate, model.TypeDateTime, model.TypeTime:
		buf = appendUvarint(buf, uint64(k.Time.UnixNano()))
	case model.TypeBytes:
		buf = appendBytes(buf, k.Bytes)
	default:
		buf = appendString(buf, k.Str)
	}
	return buf
}

func appendValue(buf []byte, v canon.Value) []byte {
	if v.Null {
		return append(buf, 0)
	}
	buf = append(buf, 1)
	return appendBytes(buf, v.Raw)
}

func appendString(buf []byte, s string) []byte { return appendBytes(buf, []byte(s)) }

func appendBytes(buf []byte, b []byte) []byte {
	buf = appendUvarint(buf, uint64(len(b)))
	return append(buf, b...)
}

func appendUvarint(buf []byte, v uint64) []byte {
	var tmp [10]byte
	n := 0
	for v >= 0x80 {
		tmp[n] = byte(v) | 0x80
		v >>= 7
		n++
	}
	tmp[n] = byte(v)
	return append(buf, tmp[:n+1]...)
}

// valuesEqual compares all compared columns with the column policies applied.
func valuesEqual(tp *plan.TablePlan, a, b []canon.Value) bool {
	for i := range tp.Columns {
		if !tp.Columns[i].SrcNorm.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// buildDiff renders a row difference, including per column detail.
func buildDiff(tp *plan.TablePlan, kind model.DiffKind, key []plan.KeyVal, src, tgt *rowValues) model.Diff {
	d := model.Diff{
		TableName: tp.Name,
		Kind:      kind,
		Stability: model.StabilityPending,
	}
	for i, k := range key {
		s, isNull := plan.FormatKeyVal(k)
		name := tp.Keys[i].SourceName
		d.PKValues = append(d.PKValues, model.KeyValue{Column: name, Value: s, Null: isNull, Type: k.Type})
	}
	d.PK = model.ParsePK(d.PKValues)
	// Record the observed fingerprints so the stability recheck can tell
	// whether the row changed afterwards without trusting the driver mapping.
	if src != nil {
		fp, _ := rowFingerprintInto(nil, src.Key, src.Vals)
		d.SourceFP = string(fp[:])
		d.SourceXmin = src.Xmin
	}
	if tgt != nil {
		fp, _ := rowFingerprintInto(nil, tgt.Key, tgt.Vals)
		d.TargetFP = string(fp[:])
	}
	switch kind {
	case model.KindModified:
		for i, c := range tp.Columns {
			sv, tv := src.Vals[i], tgt.Vals[i]
			if c.SrcNorm.Equal(sv, tv) {
				continue
			}
			d.Columns = append(d.Columns, model.ColumnDiff{
				Column:  c.SourceName,
				Source:  displayValue(sv, c),
				Target:  displayValue(tv, c),
				Changed: true,
			})
		}
		d.SourceRow = previewRow(tp, plan.SideSource, src.Vals)
		d.TargetRow = previewRow(tp, plan.SideTarget, tgt.Vals)
	case model.KindMissing:
		d.SourceRow = previewRow(tp, plan.SideSource, src.Vals)
	case model.KindExtra:
		d.TargetRow = previewRow(tp, plan.SideTarget, tgt.Vals)
	}
	return d
}

// displayValue renders a value for the difference list, hiding sensitive
// values behind a fixed mask while still reporting that they differ.
func displayValue(v canon.Value, c plan.ColPlan) string {
	if c.Sensitive {
		if v.Null {
			return "NULL"
		}
		return "******"
	}
	return canon.Excerpt(v, c.SrcNorm.Opt.Type == model.TypeBytes, 256)
}

func previewRow(tp *plan.TablePlan, side plan.Side, vals []canon.Value) map[string]string {
	out := make(map[string]string, len(vals))
	for i, c := range tp.Columns {
		name := c.SourceName
		if side == plan.SideTarget {
			name = c.TargetName
		}
		if c.Sensitive {
			if vals[i].Null {
				out[name] = "NULL"
			} else {
				out[name] = "******"
			}
			continue
		}
		out[name] = canon.Excerpt(vals[i], c.SrcNorm.Opt.Type == model.TypeBytes, 128)
	}
	return out
}

// mergeChunk streams both sides and returns the differences it finds.  When
// maxRows is positive and more candidate rows than that are encountered, the
// chunk is reported as too dense so the caller can subdivide it.
func mergeChunk(ctx context.Context, tp *plan.TablePlan, srcRows, tgtRows *sql.Rows,
	withXmin, tgtXmin bool, sink func(model.Diff) error, maxRows int,
	srcLim, tgtLim *Limiter) (int64, error) {

	src := newPacedStream(srcRows, tp, plan.SideSource, withXmin,
		&limiterPacer{lim: srcLim, ctx: ctx, batch: 2000})
	tgt := newPacedStream(tgtRows, tp, plan.SideTarget, tgtXmin,
		&limiterPacer{lim: tgtLim, ctx: ctx, batch: 2000})
	defer srcRows.Close()
	defer tgtRows.Close()

	srcOK := src.next(tp, plan.SideSource)
	if src.err != nil {
		return 0, src.err
	}
	tgtOK := tgt.next(tp, plan.SideTarget)
	if tgt.err != nil {
		return 0, tgt.err
	}
	var found int64
	for {
		if err := ctx.Err(); err != nil {
			return found, err
		}
		switch {
		case !srcOK && !tgtOK:
			return found, nil
		case !srcOK:
			if err := sink(buildDiff(tp, model.KindExtra, tgt.cur.Key, nil, &tgt.cur)); err != nil {
				return found, err
			}
			found++
			if maxRows > 0 && found > int64(maxRows) {
				return found, errTooDense
			}
			tgtOK = tgt.next(tp, plan.SideTarget)
			if tgt.err != nil {
				return found, tgt.err
			}
		case !tgtOK:
			if err := sink(buildDiff(tp, model.KindMissing, src.cur.Key, &src.cur, nil)); err != nil {
				return found, err
			}
			found++
			if maxRows > 0 && found > int64(maxRows) {
				return found, errTooDense
			}
			srcOK = src.next(tp, plan.SideSource)
			if src.err != nil {
				return found, src.err
			}
		default:
			cmp := plan.CompareTuple(src.cur.Key, tgt.cur.Key)
			switch {
			case cmp < 0:
				if err := sink(buildDiff(tp, model.KindMissing, src.cur.Key, &src.cur, nil)); err != nil {
					return found, err
				}
				found++
				srcOK = src.next(tp, plan.SideSource)
				if src.err != nil {
					return found, src.err
				}
			case cmp > 0:
				if err := sink(buildDiff(tp, model.KindExtra, tgt.cur.Key, nil, &tgt.cur)); err != nil {
					return found, err
				}
				found++
				tgtOK = tgt.next(tp, plan.SideTarget)
				if tgt.err != nil {
					return found, tgt.err
				}
			default:
				if !valuesEqual(tp, src.cur.Vals, tgt.cur.Vals) {
					d := buildDiff(tp, model.KindModified, src.cur.Key, &src.cur, &tgt.cur)
					if len(d.Columns) > 0 {
						if err := sink(d); err != nil {
							return found, err
						}
						found++
					}
				}
				srcOK = src.next(tp, plan.SideSource)
				if src.err != nil {
					return found, src.err
				}
				tgtOK = tgt.next(tp, plan.SideTarget)
				if tgt.err != nil {
					return found, tgt.err
				}
			}
		}
		if maxRows > 0 && found > int64(maxRows) {
			return found, errTooDense
		}
	}
}

// errTooDense signals that a chunk contains more differences than the
// configured limit, so it should be subdivided instead of materialised.
var errTooDense = fmt.Errorf("chunk has too many differences; subdividing")

// fingerprintChunk computes order independent aggregates for one side of a
// chunk.  Row count, framed 128 bit row hash sum and XOR of the same hashes
// detect both missing rows and changed values, and are computable on both
// sides without trusting the database's collation or hash functions.
type chunkAgg struct {
	Rows int64
	Sum  [16]byte
	Xor  [16]byte
}

func (a *chunkAgg) add(fp [16]byte) {
	a.Rows++
	for i := 0; i < 16; i++ {
		a.Sum[i] += fp[i]
		a.Xor[i] ^= fp[i]
	}
}

func (a chunkAgg) equal(b chunkAgg) bool {
	return a.Rows == b.Rows && a.Sum == b.Sum && a.Xor == b.Xor
}

// aggregateSide scans one side of a chunk and returns its aggregate.  It also
// returns the number of rows, which callers use for pacing.
func aggregateSide(ctx context.Context, tp *plan.TablePlan, rows *sql.Rows, side plan.Side, lim *Limiter) (chunkAgg, error) {
	defer rows.Close()
	pacer := &limiterPacer{lim: lim, ctx: ctx, batch: 2000}
	st := newPacedStream(rows, tp, side, false, pacer)
	var agg chunkAgg
	var buf []byte
	for st.next(tp, side) {
		var fp [16]byte
		fp, buf = rowFingerprintInto(buf, st.cur.Key, st.cur.Vals)
		agg.add(fp)
	}
	if st.err != nil {
		return agg, st.err
	}
	if err := pacer.flush(); err != nil {
		return agg, err
	}
	return agg, nil
}

var _ = bytes.Equal
var _ = sort.Slice
var _ = strings.TrimSpace
