// Package plan resolves a user supplied table specification against both
// databases and produces the SQL and normalisers needed to compare one table.
package plan

import (
	"fmt"
	"strings"
	"time"

	"bankcmp/internal/canon"
	"bankcmp/internal/dialect"
	"bankcmp/internal/model"
	"bankcmp/internal/source"
)

// KeyKind is the ordering family of a key column.  Both sides must agree on
// it so that a merge join over their sorted streams is meaningful.
type KeyKind string

const (
	KindInt      KeyKind = "int"
	KindNumeric  KeyKind = "numeric"
	KindTemporal KeyKind = "temporal"
	KindText     KeyKind = "text"
	KindBool     KeyKind = "bool"
)

// KeyCol is one resolved key column with side specific names.
type KeyCol struct {
	SourceName string
	TargetName string
	SourceType model.LogicalType
	TargetType model.LogicalType
	// SourcePhysical / TargetPhysical are the types the columns actually have,
	// before any explicit key type override.
	SourcePhysical model.LogicalType
	TargetPhysical model.LogicalType
	// Canonical is the type both sides are parsed into for ordering.  It is
	// the wider of the two (e.g. date + timestamp -> timestamp) so that a
	// range boundary means the same thing on both sides.
	Canonical model.LogicalType
	// SourceSQL / TargetSQL are the SQL types used for parameter casts.
	SourceSQL string
	TargetSQL string
	Kind      KeyKind
	// SourceText / TargetText report whether the side is a text type whose
	// database collation must be overridden with a bytewise one so both sides
	// order identically.  PostgreSQL UUID is deliberately excluded: it has its
	// own value ordering and rejects COLLATE.
	SourceText bool
	TargetText bool
}

// ColPlan pairs one compared column across the two databases.
type ColPlan struct {
	SourceName string
	TargetName string
	SrcNorm    *canon.Normalizer
	TgtNorm    *canon.Normalizer
	// Digest means both sides return a hex digest instead of the value.
	Digest bool
	// DigestBinary records that the underlying column is binary, so the
	// digest expression hashes raw bytes rather than text.
	DigestBinary bool
	// SourceBlankPadded / TargetBlankPadded mark CHAR(n) columns whose
	// trailing spaces are padding rather than data.
	SourceBlankPadded bool
	TargetBlankPadded bool
	SourceSQL         string
	TargetSQL         string
	// Sensitive marks columns excluded from previews/reports (e.g. PAN).
	Sensitive bool
}

// TablePlan is a resolved table comparison.
type TablePlan struct {
	Name         string
	SourceTable  string
	TargetTable  string
	SourceSchema *source.TableSchema
	TargetSchema *source.TableSchema
	Keys         []KeyCol
	Columns      []ColPlan
	SourceWhere  string
	TargetWhere  string
	SourceDial   dialect.Dialect
	TargetDial   dialect.Dialect
	// SourceXmin enables PostgreSQL xmin extraction on the source for
	// transient-write detection.
	SourceXmin bool
	// ChunkRows and DiffMaxRows are the effective per table settings.
	ChunkRows   int
	DiffMaxRows int
	// SourceLoc / TargetLoc are the session timezones used when parsing
	// naive timestamp values.
	SourceLoc *time.Location
	TargetLoc *time.Location
	// SourceCharset / TargetCharset are the connection default charsets.
	SourceCharset string
	TargetCharset string
}

// Side identifies one database in a plan.
type Side int

const (
	SideSource Side = iota
	SideTarget
)

// BuildOurQueryError reports a configuration problem that would make an exact
// comparison impossible.
type ConfigError struct {
	Table string
	Msg   string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("table %s: %s", e.Table, e.Msg)
}

func cfgErr(table, format string, args ...any) error {
	return &ConfigError{Table: table, Msg: fmt.Sprintf(format, args...)}
}

// Build resolves a table spec against both schemas.
func Build(spec model.TableSpec, srcDB, tgtDB *source.Database,
	srcSchema, tgtSchema *source.TableSchema, defaults model.Options) (*TablePlan, error) {

	tp := &TablePlan{
		Name:          spec.Name,
		SourceTable:   srcDB.Dial.Quote(spec.Source()),
		TargetTable:   tgtDB.Dial.Quote(spec.Target()),
		SourceSchema:  srcSchema,
		TargetSchema:  tgtSchema,
		SourceDial:    srcDB.Dial,
		TargetDial:    tgtDB.Dial,
		SourceWhere:   firstNonEmpty(spec.SourceWhere, spec.Where),
		TargetWhere:   firstNonEmpty(spec.TargetWhere, spec.Where),
		ChunkRows:     spec.ChunkRows,
		DiffMaxRows:   spec.DiffMaxRows,
		SourceLoc:     srcDB.Timezone,
		TargetLoc:     tgtDB.Timezone,
		SourceCharset: srcDB.Charset,
		TargetCharset: tgtDB.Charset,
	}
	if tp.ChunkRows <= 0 {
		tp.ChunkRows = defaults.ChunkRows
	}
	if tp.DiffMaxRows <= 0 {
		tp.DiffMaxRows = defaults.DiffMaxRows
	}
	tp.SourceXmin = srcDB.Driver == model.DriverPostgres

	// Resolve key columns.
	keyNames := spec.KeyColumns
	if len(keyNames) == 0 {
		keyNames = srcSchema.PrimaryKey
		if len(keyNames) == 0 {
			return nil, cfgErr(spec.Name, "no primary key on source and no key_columns configured; a stable key is required to compare rows")
		}
		if len(tgtSchema.PrimaryKey) > 0 && !sameColumns(keyNames, tgtSchema.PrimaryKey) {
			return nil, cfgErr(spec.Name, "primary keys differ: source %v, target %v; configure key_columns explicitly",
				keyNames, tgtSchema.PrimaryKey)
		}
	}
	for _, kname := range keyNames {
		sc, ok := findColumn(srcSchema, kname)
		if !ok {
			return nil, cfgErr(spec.Name, "key column %q not found on source", kname)
		}
		// A key column can be overridden to a logical type when it is stored as
		// text but holds numbers (a legacy fixed point id, for instance).  This
		// is explicit because changing the key's type changes its ordering.
		scPhys := sc.Logical
		for _, cs := range spec.Columns {
			if strings.EqualFold(cs.Name, kname) && cs.Type != model.TypeAuto {
				sc.Logical = cs.Type
			}
		}
		// Key target name may be overridden via a column spec.
		tname := kname
		for _, cs := range spec.Columns {
			if strings.EqualFold(cs.Name, kname) && cs.TargetName != "" {
				tname = cs.TargetName
			}
		}
		tc, ok := findColumn(tgtSchema, tname)
		if !ok {
			return nil, cfgErr(spec.Name, "key column %q not found on target", tname)
		}
		tcPhys := tc.Logical
		for _, cs := range spec.Columns {
			if (strings.EqualFold(cs.Name, kname) || strings.EqualFold(cs.Name, tname)) && cs.Type != model.TypeAuto {
				tc.Logical = cs.Type
			}
		}
		kc := KeyCol{
			SourceName:     sc.Name,
			TargetName:     tc.Name,
			SourceType:     sc.Logical,
			TargetType:     tc.Logical,
			SourcePhysical: scPhys,
			TargetPhysical: tcPhys,
			SourceSQL:      sc.Logical.SQLNameFor(srcDB.Driver),
			TargetSQL:      tc.Logical.SQLNameFor(tgtDB.Driver),
			// The ordering family tells whether the database's own byte order
			// is the right one.  A text column holding numbers must be cast in
			// SQL so the database also sorts it numerically; see KeyExpression.
			SourceText: sc.Logical == model.TypeString && scPhys == model.TypeString,
			TargetText: tc.Logical == model.TypeString && tcPhys == model.TypeString,
		}
		sk := orderingKind(sc.Logical)
		tk := orderingKind(tc.Logical)
		if sk != tk {
			return nil, cfgErr(spec.Name, "key column %q orders as %s on source but %s on target; store the key in a compatible type or add an explicit cast",
				kname, sk, tk)
		}
		kc.Kind = sk
		kc.Canonical = resolvePairType(sc.Logical, tc.Logical)
		if kc.Canonical == model.TypeString || kc.Canonical == model.TypeBytes {
			if sc.Logical != tc.Logical && sc.Logical != model.TypeUUID && tc.Logical != model.TypeUUID {
				return nil, cfgErr(spec.Name, "key column %q is text on one side and %s on the other; the byte ordering would differ, so configure it as the same type",
					kname, sc.Logical)
			}
		}
		if kc.Kind == KindText && canonCharset(srcDB.Charset) != canonCharset(tgtDB.Charset) {
			return nil, cfgErr(spec.Name, "key column %q is textual and the two connections use different charsets (%q vs %q); equal characters would order differently. Store keys in one charset or compare a non-text key",
				kname, srcDB.Charset, tgtDB.Charset)
		}
		if sc.Nullable {
			return nil, cfgErr(spec.Name, "key column %q is nullable on source; NULL keys cannot be ranged consistently across engines. Choose a NOT NULL key", kname)
		}
		if tc.Nullable {
			return nil, cfgErr(spec.Name, "key column %q is nullable on target; NULL keys cannot be ranged consistently across engines. Choose a NOT NULL key", kname)
		}
		tp.Keys = append(tp.Keys, kc)
	}
	// Reject normalisation that would break the database ordering promise.
	for _, cs := range spec.Columns {
		for _, k := range tp.Keys {
			if strings.EqualFold(cs.Name, k.SourceName) {
				if cs.TrimSpace || cs.CaseFold || cs.CollapseSpace || cs.NullEqualsEmpty || cs.Scale != 0 || cs.FloatDigits != 0 {
					return nil, cfgErr(spec.Name, "key column %q cannot use value normalisation options (trim/case/scale); the ordering must match the database", cs.Name)
				}
			}
		}
	}

	// Resolve compared columns.
	ignoreSet := map[string]bool{}
	byName := map[string]model.ColumnSpec{}
	for _, cs := range spec.Columns {
		byName[strings.ToLower(cs.Name)] = cs
		if cs.Ignore {
			ignoreSet[strings.ToLower(cs.Name)] = true
		}
	}
	keySet := map[string]bool{}
	for _, k := range tp.Keys {
		keySet[strings.ToLower(k.SourceName)] = true
	}

	var chosen []struct {
		src, tgt source.Column
		cs       model.ColumnSpec
		present  bool
	}
	for _, scol := range srcSchema.Columns {
		low := strings.ToLower(scol.Name)
		if ignoreSet[low] {
			continue
		}
		if keySet[low] {
			// Key columns are compared as part of the merge join; comparing
			// them again would double the transferred data for no benefit.
			continue
		}
		cs, specified := byName[low]
		if spec.ColumnMode == "listed" && !specified && !keySet[low] {
			continue // whitelist mode: only compare the listed columns
		}
		tname := scol.Name
		if specified && cs.TargetName != "" {
			tname = cs.TargetName
		}
		tcol, ok := findColumn(tgtSchema, tname)
		if !ok {
			return nil, cfgErr(spec.Name, "column %q exists on source but not on target (target name %q)", scol.Name, tname)
		}
		chosen = append(chosen, struct {
			src, tgt source.Column
			cs       model.ColumnSpec
			present  bool
		}{scol, tcol, cs, specified})
	}
	if len(chosen) == 0 {
		return nil, cfgErr(spec.Name, "no comparable columns resolved")
	}
	for _, c := range chosen {
		cp, err := buildColumn(spec, c.src, c.tgt, c.cs, c.present, srcDB, tgtDB)
		if err != nil {
			return nil, err
		}
		tp.Columns = append(tp.Columns, cp)
	}
	return tp, nil
}

func buildColumn(spec model.TableSpec, sc, tc source.Column, cs model.ColumnSpec,
	specified bool, srcDB, tgtDB *source.Database) (ColPlan, error) {

	cp := ColPlan{
		SourceName: sc.Name,
		TargetName: tc.Name,
		Sensitive:  isSensitiveName(sc.Name),
	}
	// Resolve the comparison type for the pair.
	ctype := resolvePairType(sc.Logical, tc.Logical)
	if specified && cs.Type != model.TypeAuto {
		if !cs.Type.Valid() {
			return cp, cfgErr(spec.Name, "column %q has unknown type %q", sc.Name, cs.Type)
		}
		ctype = cs.Type
	}
	digestBinary := false
	if cs.Digest {
		if ctype != model.TypeString && ctype != model.TypeBytes {
			return cp, cfgErr(spec.Name, "column %q: digest comparison supports only string/binary columns (logical type is %s)", sc.Name, ctype)
		}
		if cs.TrimSpace || cs.CollapseSpace || cs.CaseFold || cs.NullEqualsEmpty {
			return cp, cfgErr(spec.Name, "column %q: digest comparison cannot apply trim/case normalisation; drop the option or the digest", sc.Name)
		}
		digestBinary = ctype == model.TypeBytes
		// The digest itself is hex text on both sides; compare it as a string.
		ctype = model.TypeString
	}
	srcCharset := firstNonEmpty(cs.SourceCharset, srcDB.Charset)
	// Target side charset: the target connection charset if set, else UTF-8.
	tgtCharset := tgtDB.Charset

	srcOpt := canon.Options{
		Type: ctype, Charset: srcCharset, Scale: cs.Scale, FloatDigits: cs.FloatDigits,
		FloatTolerance: cs.FloatTolerance, TrimSpace: cs.TrimSpace, CollapseSpace: cs.CollapseSpace,
		CaseFold: cs.CaseFold, NullEqualsEmpty: cs.NullEqualsEmpty, Layouts: cs.Layouts,
		Timezone: srcDB.Timezone, BoolTrue: cs.BoolTrue, BoolFalse: cs.BoolFalse, Digest: cs.Digest,
	}
	tgtOpt := srcOpt
	tgtOpt.Charset = tgtCharset
	tgtOpt.Timezone = tgtDB.Timezone

	sn, err := canon.NewNormalizer(srcOpt)
	if err != nil {
		return cp, cfgErr(spec.Name, "column %q source: %v", sc.Name, err)
	}
	tn, err := canon.NewNormalizer(tgtOpt)
	if err != nil {
		return cp, cfgErr(spec.Name, "column %q target: %v", tc.Name, err)
	}
	cp.SrcNorm, cp.TgtNorm = sn, tn
	cp.SourceBlankPadded = sc.BlankPadded
	cp.TargetBlankPadded = tc.BlankPadded
	cp.Digest = cs.Digest
	cp.DigestBinary = digestBinary
	cp.SourceSQL = sc.Logical.SQLNameFor(srcDB.Driver)
	cp.TargetSQL = tc.Logical.SQLNameFor(tgtDB.Driver)
	return cp, nil
}

// Expression returns the SQL select expression for this column on a side.
func (c ColPlan) Expression(side Side, d dialect.Dialect) string {
	var name, charset string
	if side == SideSource {
		name, charset = c.SourceName, c.SrcNorm.Opt.Charset
	} else {
		name, charset = c.TargetName, c.TgtNorm.Opt.Charset
	}
	padded := c.TargetBlankPadded
	if side == SideSource {
		padded = c.SourceBlankPadded
	}
	if !c.Digest {
		if padded {
			// CHAR(n) is blank padded; the padding is not part of the value.
			return fmt.Sprintf("rtrim(%s)", d.Quote(name))
		}
		return d.Quote(name)
	}
	switch d.Name() {
	case model.DriverSQLite:
		lt := "text"
		if c.DigestBinary {
			lt = "bytes"
		}
		return sqliteDigestExpr(name, lt, charset)
	case model.DriverPostgres:
		q := d.Quote(name)
		if c.DigestBinary {
			return fmt.Sprintf("CASE WHEN %s IS NULL THEN NULL ELSE encode(sha256(%s), 'hex') END", q, q)
		}
		return fmt.Sprintf("CASE WHEN %s IS NULL THEN NULL ELSE encode(sha256(convert_to(%s::text, 'UTF8')), 'hex') END", q, q)
	}
	return d.Quote(name)
}

func sqliteDigestExpr(name, logical, charset string) string {
	q := `"` + strings.ReplaceAll(strings.Trim(name, `"`), `"`, `""`) + `"`
	if logical == "text" && charset != "" && !strings.EqualFold(charset, "utf-8") && !strings.EqualFold(charset, "utf8") {
		return fmt.Sprintf("CASE WHEN %s IS NULL THEN NULL ELSE bc_sha256_text(%s, '%s') END", q, q, strings.ReplaceAll(charset, "'", "''"))
	}
	return fmt.Sprintf("CASE WHEN %s IS NULL THEN NULL ELSE bc_sha256(%s) END", q, q)
}

// KeyExpression returns the ordering expression for a key column.
//
//   - textual keys get a bytewise collation so both engines order them the
//     same way;
//   - a text column that was declared as a numeric key is cast so the database
//     sorts it numerically, matching the comparison;
//   - everything else uses the native type's own ordering.
func (k KeyCol) KeyExpression(side Side, d dialect.Dialect) string {
	var name string
	var text bool
	eff, phys := k.SourceType, k.SourcePhysical
	if side == SideSource {
		name, text = k.SourceName, k.SourceText
	} else {
		name, text = k.TargetName, k.TargetText
		eff, phys = k.TargetType, k.TargetPhysical
	}
	q := d.Quote(name)
	if text {
		return d.CollateKey(q)
	}
	if phys == model.TypeString && eff != model.TypeString {
		// The physical column is text but the key was declared numeric or
		// temporal.  Cast it so the database's ordering matches the parsed
		// key values; a mismatch here would silently break the merge.
		if cast := keyCastType(eff); cast != "" {
			return fmt.Sprintf("CAST(%s AS %s)", q, cast)
		}
	}
	return q
}

// keyCastType returns the SQL type a textual key is cast to so its order
// matches the comparison.
func keyCastType(t model.LogicalType) string {
	switch t {
	case model.TypeInt:
		return "bigint"
	case model.TypeDecimal:
		return "numeric"
	case model.TypeDate:
		return "date"
	case model.TypeDateTime:
		return "timestamp"
	}
	return ""
}

// KeyParamCast returns the placeholder with an explicit type cast for a key
// column on one side.
func (k KeyCol) KeyParamCast(side Side, d dialect.Dialect, pos int) string {
	ph := d.Placeholder(pos)
	if d.Name() == model.DriverPostgres {
		typ := k.TargetSQL
		if side == SideSource {
			typ = k.SourceSQL
		}
		return ph + "::" + typ
	}
	return ph
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func findColumn(sch *source.TableSchema, name string) (source.Column, bool) {
	for _, c := range sch.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return source.Column{}, false
}

func sameColumns(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// resolvePairType chooses the canonical comparison type for two physical
// types.  It deliberately widens where a value cannot be lost:
//   - int+decimal -> decimal
//   - date+datetime -> datetime (a date becomes midnight)
//   - text+typed -> the typed side, since both text parsers can parse it
//   - anything else -> string
func resolvePairType(a, b model.LogicalType) model.LogicalType {
	if a == b {
		return a
	}
	set := map[model.LogicalType]bool{a: true, b: true}
	switch {
	case set[model.TypeInt] && set[model.TypeDecimal]:
		return model.TypeDecimal
	case (set[model.TypeInt] || set[model.TypeDecimal]) && set[model.TypeFloat]:
		return model.TypeFloat
	case set[model.TypeDate] && set[model.TypeDateTime]:
		return model.TypeDateTime
	case set[model.TypeString] && (set[model.TypeInt] || set[model.TypeDecimal] || set[model.TypeBool] ||
		set[model.TypeDate] || set[model.TypeDateTime] || set[model.TypeTime] || set[model.TypeUUID]):
		if set[model.TypeInt] {
			return model.TypeInt
		}
		if set[model.TypeDecimal] {
			return model.TypeDecimal
		}
		if set[model.TypeBool] {
			return model.TypeBool
		}
		if set[model.TypeDate] {
			return model.TypeDate
		}
		if set[model.TypeDateTime] {
			return model.TypeDateTime
		}
		if set[model.TypeTime] {
			return model.TypeTime
		}
		return model.TypeUUID
	case set[model.TypeBytes] && set[model.TypeString]:
		return model.TypeBytes
	case set[model.TypeBytes] && set[model.TypeUUID]:
		return model.TypeBytes
	}
	return model.TypeString
}

func canonCharset(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "utf8", "utf-8", "unicode":
		return "utf-8"
	case "gb2312", "cp936", "gbk":
		return "gbk"
	}
	return strings.ToLower(strings.TrimSpace(s))
}

func orderingKind(t model.LogicalType) KeyKind {
	switch t {
	case model.TypeInt:
		return KindInt
	case model.TypeDecimal, model.TypeFloat:
		return KindNumeric
	case model.TypeDate, model.TypeDateTime, model.TypeTime:
		return KindTemporal
	case model.TypeBool:
		return KindBool
	default:
		return KindText
	}
}

// isSensitiveName flags columns that should not be dumped into reports.
func isSensitiveName(name string) bool {
	n := strings.ToLower(name)
	for _, frag := range []string{"password", "passwd", "pwd", "secret", "token", "pan", "cvv", "id_card", "idcard", "idno", "id_no"} {
		if strings.Contains(n, frag) {
			return true
		}
	}
	return false
}
