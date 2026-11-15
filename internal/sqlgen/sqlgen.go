// Package sqlgen turns differences into repair statements for the target
// database.  Statements are generated in a canonical, reviewable form and
// always come with a matching undo statement so a preview can be checked before
// anything runs.
package sqlgen

import (
	"fmt"
	"sort"
	"strings"

	"bankcmp/internal/canon"
	"bankcmp/internal/dialect"
	"bankcmp/internal/model"
	"bankcmp/internal/plan"
)

// Options controls repair generation.
type Options struct {
	// Kinds selects which difference kinds to repair (missing -> INSERT,
	// extra -> DELETE, modified -> UPDATE).
	Kinds []model.DiffKind
	// IncludeTransient includes differences classified as transient.  Off by
	// default: transient rows changed while comparing, so repairing them
	// would write stale data.
	IncludeTransient bool
}

// DefaultOptions repairs all stable differences.
func DefaultOptions() Options {
	return Options{Kinds: []model.DiffKind{model.KindMissing, model.KindExtra, model.KindModified}}
}

// SourceRow is a complete row value used to build an INSERT or UPDATE.
// Rows are fetched fresh from the database, never reconstructed from the
// truncated previews shown in the UI.
type SourceRow struct {
	// Key is in plan key order.
	Key  []plan.KeyVal
	Vals []canon.Value
}

// Batch is generated SQL plus a preview.
type Batch struct {
	Statements int
	SQL        string
	UndoSQL    string
	Kinds      string
	// Skipped counts diffs excluded by options.
	Skipped int
	// Warnings are notes that limit the batch's usefulness.
	Warnings []string
}

func (o Options) allowsKind(k model.DiffKind) bool {
	if len(o.Kinds) == 0 {
		return true
	}
	for _, x := range o.Kinds {
		if x == k {
			return true
		}
	}
	return false
}

// Prepare filters the diffs by kind and stability and returns the ones that
// will be repaired.
func (o Options) Prepare(diffs []model.Diff) (selected []model.Diff, skipped int) {
	for _, d := range diffs {
		if !o.allowsKind(d.Kind) {
			skipped++
			continue
		}
		if d.Stability == model.StabilityTransient && !o.IncludeTransient {
			skipped++
			continue
		}
		selected = append(selected, d)
	}
	return selected, skipped
}

// Input carries the full row data for a selection of diffs.
type Input struct {
	// SourceRows maps canonical PK to the current source row, used for
	// INSERT and UPDATE statements.
	SourceRows map[string]SourceRow
	// TargetRows maps canonical PK to the current target row, used to build
	// the undo of a DELETE.
	TargetRows map[string]SourceRow
}

// Generate builds repair SQL for one table's differences.
func Generate(tp *plan.TablePlan, d dialect.Dialect, diffs []model.Diff, in Input, opts Options) (*Batch, error) {
	b := &Batch{}
	var stmts, undos []string
	var kinds []string
	var missingRows int
	for _, kind := range []model.DiffKind{model.KindMissing, model.KindExtra, model.KindModified} {
		any := false
		for _, df := range diffs {
			if df.Kind != kind {
				continue
			}
			stmt, undo, err := statementFor(tp, d, df, in)
			if err != nil {
				return nil, fmt.Errorf("%s row %s: %w", kind, df.PK, err)
			}
			if stmt == "" {
				missingRows++
				continue
			}
			if !any {
				kinds = append(kinds, string(kind))
				any = true
			}
			stmts = append(stmts, stmt)
			if undo != "" {
				undos = append(undos, undo)
			}
		}
	}
	b.Statements = len(stmts)
	b.Kinds = strings.Join(kinds, ",")
	if missingRows > 0 {
		b.Warnings = append(b.Warnings, fmt.Sprintf("%d difference(s) no longer exist on the source or target; the databases changed after the comparison. Rerun it before repairing", missingRows))
	}
	b.SQL = strings.Join(stmts, ";\n") + trailingSemi(stmts)
	b.UndoSQL = strings.Join(undos, ";\n") + trailingSemi(undos)
	return b, nil
}

func trailingSemi(stmts []string) string {
	if len(stmts) == 0 {
		return ""
	}
	return ";"
}

// statementFor returns the repair statement and its undo counterpart.
func statementFor(tp *plan.TablePlan, d dialect.Dialect, df model.Diff, in Input) (string, string, error) {
	switch df.Kind {
	case model.KindMissing:
		row, ok := in.SourceRows[df.PK]
		if !ok {
			return "", "", nil
		}
		stmt, err := insertStatement(tp, d, row)
		if err != nil {
			return "", "", err
		}
		return stmt, "DELETE FROM " + tp.TargetTable + " WHERE " + whereClause(tp, d, row.Key), nil
	case model.KindExtra:
		row, ok := in.TargetRows[df.PK]
		if !ok {
			return "", "", nil
		}
		stmt := "DELETE FROM " + tp.TargetTable + " WHERE " + whereClause(tp, d, row.Key)
		undo, err := insertStatement(tp, d, row)
		return stmt, undo, err
	case model.KindModified:
		srcRow, ok := in.SourceRows[df.PK]
		if !ok {
			return "", "", nil
		}
		tgtRow, ok := in.TargetRows[df.PK]
		if !ok {
			return "", "", nil
		}
		return updateStatement(tp, d, srcRow, tgtRow)
	}
	return "", "", fmt.Errorf("unknown diff kind %q", df.Kind)
}

// insertStatement builds a full column INSERT for a source row.
func insertStatement(tp *plan.TablePlan, d dialect.Dialect, row SourceRow) (string, error) {
	var cols, vals []string
	for i, k := range tp.Keys {
		cols = append(cols, d.Quote(k.TargetName))
		lit, err := literalForKeyVal(d, row.Key[i], k)
		if err != nil {
			return "", err
		}
		vals = append(vals, lit)
	}
	for i, c := range tp.Columns {
		if c.Sensitive {
			return "", fmt.Errorf("refusing to generate INSERT for sensitive column %q; exclude the table or mark it ignored", c.SourceName)
		}
		cols = append(cols, d.Quote(c.TargetName))
		lit, err := literalForValue(d, row.Vals[i], c)
		if err != nil {
			return "", fmt.Errorf("column %q: %w", c.SourceName, err)
		}
		vals = append(vals, lit)
	}
	if len(cols) == 0 {
		return "", fmt.Errorf("no columns to insert")
	}
	return "INSERT INTO " + tp.TargetTable + " (" + strings.Join(cols, ", ") +
		") VALUES (" + strings.Join(vals, ", ") + ")", nil
}

// updateStatement builds UPDATE statements for every column that differs,
// plus an undo restoring the old values.
func updateStatement(tp *plan.TablePlan, d dialect.Dialect, src, tgt SourceRow) (string, string, error) {
	var sets, undoSets []string
	for i, c := range tp.Columns {
		if c.SrcNorm.Equal(src.Vals[i], tgt.Vals[i]) {
			continue
		}
		if c.Sensitive {
			return "", "", fmt.Errorf("refusing to generate UPDATE for sensitive column %q", c.SourceName)
		}
		newLit, err := literalForValue(d, src.Vals[i], c)
		if err != nil {
			return "", "", fmt.Errorf("column %q: %w", c.SourceName, err)
		}
		oldLit, err := literalForValue(d, tgt.Vals[i], c)
		if err != nil {
			return "", "", fmt.Errorf("column %q: %w", c.SourceName, err)
		}
		sets = append(sets, d.Quote(c.TargetName)+" = "+newLit)
		undoSets = append(undoSets, d.Quote(c.TargetName)+" = "+oldLit)
	}
	if len(sets) == 0 {
		// The row no longer differs (a race); nothing to repair.
		return "", "", nil
	}
	where := whereClause(tp, d, src.Key)
	return "UPDATE " + tp.TargetTable + " SET " + strings.Join(sets, ", ") + " WHERE " + where,
		"UPDATE " + tp.TargetTable + " SET " + strings.Join(undoSets, ", ") + " WHERE " + where, nil
}

func whereClause(tp *plan.TablePlan, d dialect.Dialect, key []plan.KeyVal) string {
	var parts []string
	for i, k := range tp.Keys {
		if i >= len(key) {
			continue
		}
		lit, err := literalForKeyVal(d, key[i], k)
		if err != nil {
			lit = "NULL"
		}
		parts = append(parts, d.Quote(k.TargetName)+" = "+lit)
	}
	return strings.Join(parts, " AND ")
}

// literalForKeyVal renders a typed key value.
func literalForKeyVal(d dialect.Dialect, kv plan.KeyVal, k plan.KeyCol) (string, error) {
	if kv.Null {
		return "NULL", nil
	}
	v, err := canonicalValueFromKey(kv)
	if err != nil {
		return "", err
	}
	typ := k.TargetType
	if typ == model.TypeAuto {
		typ = k.Canonical
	}
	return d.Literal(v, typ), nil
}

func canonicalValueFromKey(kv plan.KeyVal) (canon.Value, error) {
	switch kv.Type {
	case model.TypeInt:
		return canon.Value{Raw: []byte(fmt.Sprintf("%d", kv.Int))}, nil
	case model.TypeDecimal, model.TypeFloat:
		return canon.Value{Raw: []byte(kv.Dec.String())}, nil
	case model.TypeDate:
		return canon.Value{Raw: []byte(kv.Time.UTC().Format("2006-01-02"))}, nil
	case model.TypeTime:
		return canon.Value{Raw: []byte(kv.Time.UTC().Format("15:04:05.999999999"))}, nil
	case model.TypeDateTime:
		return canon.Value{Raw: []byte(kv.Time.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))}, nil
	case model.TypeBytes:
		return canon.Value{Raw: kv.Bytes}, nil
	default:
		return canon.Value{Raw: []byte(kv.Str)}, nil
	}
}

// literalForValue renders a canonical compared value for the target engine.
func literalForValue(d dialect.Dialect, v canon.Value, c plan.ColPlan) (string, error) {
	typ := c.TgtNorm.Opt.Type
	if v.Null {
		return "NULL", nil
	}
	if typ == model.TypeBytes {
		return d.BytesLiteral(v.Raw), nil
	}
	return d.Literal(v, typ), nil
}

// sortStatements is exposed for callers that want a stable preview ordering.
func sortStatements(stmts []string) {
	sort.Strings(stmts)
}
