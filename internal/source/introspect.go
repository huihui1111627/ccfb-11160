package source

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"bankcmp/internal/model"
)

// Introspect reads the schema of a table.
func (d *Database) Introspect(ctx context.Context, table string) (*TableSchema, error) {
	switch d.Driver {
	case model.DriverSQLite:
		return d.introspectSQLite(ctx, table)
	case model.DriverPostgres:
		return d.introspectPostgres(ctx, table)
	}
	return nil, fmt.Errorf("unsupported driver %q", d.Driver)
}

// Tables lists user tables (and views) in the default schema.
func (d *Database) Tables(ctx context.Context) ([]string, error) {
	switch d.Driver {
	case model.DriverSQLite:
		rows, err := d.DB.QueryContext(ctx,
			`SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return nil, err
			}
			out = append(out, n)
		}
		return out, rows.Err()
	case model.DriverPostgres:
		rows, err := d.DB.QueryContext(ctx, `
			SELECT table_schema || '.' || table_name
			  FROM information_schema.tables
			 WHERE table_type = 'BASE TABLE'
			   AND table_schema NOT IN ('pg_catalog','information_schema')
			 ORDER BY 1`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return nil, err
			}
			out = append(out, n)
		}
		return out, rows.Err()
	}
	return nil, fmt.Errorf("unsupported driver %q", d.Driver)
}

// TableExists reports whether the given table can be queried.
func (d *Database) TableExists(ctx context.Context, table string) (bool, error) {
	sch, err := d.Introspect(ctx, table)
	if err != nil {
		return false, err
	}
	return len(sch.Columns) > 0, nil
}

func (d *Database) introspectSQLite(ctx context.Context, table string) (*TableSchema, error) {
	schemaName, base := splitSQLiteName(table)

	// PRAGMA table_info
	q := fmt.Sprintf(`PRAGMA %s.table_info(%s)`, dialectQuoteIdentForPragma(schemaName), sqlString(base))
	rows, err := d.DB.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("introspect %s: %w", table, err)
	}
	defer rows.Close()
	sch := &TableSchema{Name: table}
	type pkCol struct {
		name string
		pos  int
	}
	var pks []pkCol
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		// SQLite allows NULL in a non INTEGER PRIMARY KEY column in engineering
		// terms, but a PRIMARY KEY declared column is NOT NULL by definition
		// and table_info reports notnull=0 for both rowid aliases and
		// WITHOUT ROWID style keys.  Treat every key column as non-nullable so
		// the nullable-key check does not reject perfectly valid tables.
		nullable := notNull == 0
		if pk > 0 {
			nullable = false
		}
		col := Column{
			Name:      name,
			DBType:    strings.ToUpper(strings.TrimSpace(ctype)),
			Nullable:  nullable,
			IsPrimary: pk > 0,
			Ordinal:   cid,
			Logical:   InferSQLiteType(ctype),
		}
		col.CharLength, col.NumPrec, col.NumScale = parseSQLiteTypeArgs(ctype)
		sch.Columns = append(sch.Columns, col)
		if pk > 0 {
			pks = append(pks, pkCol{name, pk})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sch.Columns) == 0 {
		return nil, fmt.Errorf("table %q not found or has no columns", table)
	}
	// Order the primary key by its declared position.
	for i := 0; i < len(pks); i++ {
		for j := i + 1; j < len(pks); j++ {
			if pks[j].pos < pks[i].pos {
				pks[i], pks[j] = pks[j], pks[i]
			}
		}
	}
	for _, p := range pks {
		sch.PrimaryKey = append(sch.PrimaryKey, p.name)
	}
	if len(sch.PrimaryKey) == 0 {
		// A UNIQUE index over NOT NULL columns is an acceptable key too.
		if pk := sqliteUniqueIndex(ctx, d, schemaName, base); len(pk) > 0 {
			sch.PrimaryKey = pk
		}
	}
	for i := range sch.Columns {
		sch.Columns[i].IsPrimary = containsFold(sch.PrimaryKey, sch.Columns[i].Name)
	}
	return sch, nil
}

// sqliteUniqueIndex finds a UNIQUE index whose columns are all NOT NULL and
// that covers every column (no partial index), usable as a comparison key.
func sqliteUniqueIndex(ctx context.Context, d *Database, schemaName, table string) []string {
	q := fmt.Sprintf(`PRAGMA %s.index_list(%s)`, dialectQuoteIdentForPragma(schemaName), sqlString(table))
	rows, err := d.DB.QueryContext(ctx, q)
	if err != nil {
		return nil
	}
	type idx struct {
		name    string
		unique  int
		partial int
	}
	var idxs []idx
	for rows.Next() {
		var (
			seq     int
			name    string
			unique  int
			origin  string
			partial int
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return nil
		}
		if unique == 1 && partial == 0 {
			idxs = append(idxs, idx{name, unique, partial})
		}
	}
	rows.Close()
	for _, ix := range idxs {
		q2 := fmt.Sprintf(`PRAGMA %s.index_info(%s)`, dialectQuoteIdentForPragma(schemaName), sqlString(ix.name))
		r2, err := d.DB.QueryContext(ctx, q2)
		if err != nil {
			continue
		}
		var cols []string
		ok := true
		for r2.Next() {
			var (
				seqno int
				cid   int
				name  sql.NullString
			)
			if err := r2.Scan(&seqno, &cid, &name); err != nil {
				ok = false
				break
			}
			if !name.Valid || cid < 0 {
				ok = false // expression index, not usable as a plain key
				break
			}
			cols = append(cols, name.String)
		}
		r2.Close()
		if ok && len(cols) > 0 {
			return cols
		}
	}
	return nil
}

func dialectQuoteIdentForPragma(s string) string {
	if s == "" || s == "main" {
		return "main"
	}
	return s
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func splitSQLiteName(table string) (schema, base string) {
	if i := strings.LastIndex(table, "."); i >= 0 {
		return table[:i], table[i+1:]
	}
	return "", table
}

func (d *Database) introspectPostgres(ctx context.Context, table string) (*TableSchema, error) {
	schema, base := splitPGName(table)
	q := `
		SELECT c.column_name,
		       c.data_type,
		       COALESCE(c.udt_name, ''),
		       c.is_nullable = 'YES',
		       c.ordinal_position,
		       COALESCE(c.character_maximum_length, 0),
		       COALESCE(c.numeric_precision, 0),
		       COALESCE(c.numeric_scale, 0)
		  FROM information_schema.columns c
		 WHERE c.table_schema = $1 AND c.table_name = $2
		 ORDER BY c.ordinal_position`
	rows, err := d.DB.QueryContext(ctx, q, schema, base)
	if err != nil {
		return nil, fmt.Errorf("introspect %s: %w", table, err)
	}
	defer rows.Close()
	sch := &TableSchema{Name: table}
	for rows.Next() {
		var (
			name, dataType, udt string
			nullable            bool
			ordinal             int
			charLen             int64
			numPrec             int32
			numScale            int32
		)
		if err := rows.Scan(&name, &dataType, &udt, &nullable, &ordinal, &charLen, &numPrec, &numScale); err != nil {
			return nil, err
		}
		dbType := strings.ToUpper(dataType)
		col := Column{
			Name:        name,
			DBType:      dbType,
			Nullable:    nullable,
			Ordinal:     ordinal,
			CharLength:  charLen,
			NumPrec:     numPrec,
			NumScale:    numScale,
			Logical:     InferPostgresType(dataType, udt),
			BlankPadded: strings.EqualFold(dataType, "character") || strings.EqualFold(udt, "bpchar"),
		}
		sch.Columns = append(sch.Columns, col)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sch.Columns) == 0 {
		return nil, fmt.Errorf("table %q not found in schema %q", base, schema)
	}
	// Primary key, ordered by key position.
	pkRows, err := d.DB.QueryContext(ctx, `
		SELECT a.attname
		  FROM pg_index i
		  JOIN pg_class t ON t.oid = i.indrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace
		  JOIN unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
		  JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
		 WHERE i.indisprimary AND n.nspname = $1 AND t.relname = $2
		 ORDER BY k.ord`, schema, base)
	if err != nil {
		return nil, err
	}
	for pkRows.Next() {
		var name string
		if err := pkRows.Scan(&name); err != nil {
			pkRows.Close()
			return nil, err
		}
		sch.PrimaryKey = append(sch.PrimaryKey, name)
	}
	pkRows.Close()
	if len(sch.PrimaryKey) == 0 {
		// Fall back to a unique index.  Only ONE index may be used: taking
		// columns from several partial unique indexes would invent a key that
		// is not unique, which would make the merge join drop rows.
		uRows, err := d.DB.QueryContext(ctx, `
			SELECT i.indexrelid::regclass::text || '|' || a.attname
			  FROM pg_index i
			  JOIN pg_class t ON t.oid = i.indrelid
			  JOIN pg_namespace n ON n.oid = t.relnamespace
			  JOIN unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
			  JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
			 WHERE i.indisunique AND NOT i.indisprimary AND n.nspname = $1 AND t.relname = $2
			   AND i.indpred IS NULL AND i.indexprs IS NULL
			 ORDER BY i.indexrelid, k.ord`, schema, base)
		if err == nil {
			byIndex := map[string][]string{}
			var order []string
			for uRows.Next() {
				var rec string
				if err := uRows.Scan(&rec); err != nil {
					continue
				}
				idx, col, ok := strings.Cut(rec, "|")
				if !ok {
					continue
				}
				if _, seen := byIndex[idx]; !seen {
					order = append(order, idx)
				}
				byIndex[idx] = append(byIndex[idx], col)
			}
			uRows.Close()
			for _, idx := range order {
				cols := byIndex[idx]
				if allNotNull(sch, cols) {
					sch.PrimaryKey = cols
					break
				}
			}
		}
	}
	for i := range sch.Columns {
		sch.Columns[i].IsPrimary = containsFold(sch.PrimaryKey, sch.Columns[i].Name)
	}
	return sch, nil
}

func allNotNull(sch *TableSchema, cols []string) bool {
	for _, c := range cols {
		found := false
		for _, col := range sch.Columns {
			if strings.EqualFold(col.Name, c) {
				found = true
				if col.Nullable {
					return false
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func splitPGName(table string) (schema, base string) {
	table = strings.Trim(table, `"`)
	if i := strings.LastIndex(table, "."); i >= 0 {
		return strings.Trim(table[:i], `"`), strings.Trim(table[i+1:], `"`)
	}
	return "public", table
}

// InferPostgresType maps an information_schema type to a logical type.
func InferPostgresType(dataType, udt string) model.LogicalType {
	dt := strings.ToLower(dataType)
	u := strings.ToLower(udt)
	switch {
	case dt == "integer" || dt == "bigint" || dt == "smallint" || u == "int8" || u == "int4" || u == "int2":
		return model.TypeInt
	case dt == "numeric" || dt == "decimal" || u == "numeric":
		return model.TypeDecimal
	case dt == "real" || dt == "double precision" || u == "float4" || u == "float8":
		return model.TypeFloat
	case dt == "boolean" || u == "bool":
		return model.TypeBool
	case dt == "date":
		return model.TypeDate
	case strings.HasPrefix(dt, "timestamp") || u == "timestamptz":
		return model.TypeDateTime
	case dt == "time without time zone" || dt == "time with time zone" || strings.HasPrefix(dt, "time"):
		return model.TypeTime
	case u == "bytea":
		return model.TypeBytes
	case u == "uuid":
		return model.TypeUUID
	case u == "json" || u == "jsonb":
		return model.TypeString
	case dt == "text" || dt == "character varying" || dt == "character" || dt == "name" ||
		u == "text" || u == "varchar" || u == "bpchar" || u == "citext":
		return model.TypeString
	case dt == "array" || u == "array":
		return model.TypeString
	}
	if strings.Contains(dt, "char") || strings.Contains(dt, "text") {
		return model.TypeString
	}
	return model.TypeString
}

// InferSQLiteType maps a declared SQLite type to a logical type using the
// usual type affinity rules, refined by the declared name.
func InferSQLiteType(declared string) model.LogicalType {
	d := strings.ToUpper(strings.TrimSpace(declared))
	base := d
	if i := strings.IndexAny(base, "("); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	switch {
	case strings.Contains(base, "BOOL"):
		return model.TypeBool
	case strings.Contains(base, "DATETIME") || strings.Contains(base, "TIMESTAMP"):
		return model.TypeDateTime
	case base == "DATE" || strings.HasSuffix(base, " DATE"):
		return model.TypeDate
	case strings.Contains(base, "TIME"):
		return model.TypeTime
	case strings.Contains(base, "INT"):
		return model.TypeInt
	case strings.Contains(base, "CHAR") || strings.Contains(base, "CLOB") || strings.Contains(base, "TEXT"):
		return model.TypeString
	case strings.Contains(base, "BLOB") || strings.Contains(base, "BINARY"):
		return model.TypeBytes
	case strings.Contains(base, "REAL") || strings.Contains(base, "FLOA") || strings.Contains(base, "DOUB"):
		return model.TypeFloat
	case strings.Contains(base, "DEC") || strings.Contains(base, "NUM") || strings.Contains(base, "MONEY"):
		return model.TypeDecimal
	case base == "":
		return model.TypeString
	}
	return model.TypeString
}

// parseSQLiteTypeArgs extracts (length) or (precision, scale) from a declared
// type such as VARCHAR(20) or NUMERIC(18,4).
func parseSQLiteTypeArgs(declared string) (length int64, prec int32, scale int32) {
	i := strings.Index(declared, "(")
	j := strings.Index(declared, ")")
	if i < 0 || j < i {
		return
	}
	inner := declared[i+1 : j]
	parts := strings.Split(inner, ",")
	if len(parts) == 1 {
		var n int64
		fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &n)
		return n, 0, 0
	}
	var p, s int32
	fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &p)
	fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &s)
	return 0, p, s
}
