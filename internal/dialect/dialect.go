// Package dialect holds the small amount of SQL syntax that differs between
// the supported engines, plus literal rendering used by the repair generator.
package dialect

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"bankcmp/internal/canon"
	"bankcmp/internal/model"
)

// Dialect renders engine specific SQL fragments.
type Dialect interface {
	Name() model.Driver
	// Quote quotes an identifier, handling schema qualified names.
	Quote(string) string
	// Placeholder returns the bind placeholder for the i-th parameter (1-based).
	Placeholder(i int) string
	// CollateKey wraps a key expression so that ordering is bytewise and
	// engine independent (PostgreSQL C collation; SQLite BINARY is default).
	CollateKey(string) string
	// Literal renders a canonical value as a SQL literal of the target engine.
	Literal(v canon.Value, logical model.LogicalType) string
	// BytesLiteral renders raw bytes.
	BytesLiteral(b []byte) string
	// BoolLiteral renders true/false.
	BoolLiteral(b bool) string
	// LimitClause renders LIMIT/OFFSET.
	LimitClause(limit, offset int) string
	// CurrentChangeToken returns a SQL expression that changes when writes
	// happen (or "" when unavailable).
	CurrentChangeToken() string
	// SupportsRowValue reports whether (a,b) > (?,?) is supported.
	SupportsRowValue() bool
	// StatementTimeoutSQL returns statements to set a per-session timeout.
	StatementTimeoutSQL(ms int) []string
}

// SQLite renders SQLite SQL.
type SQLite struct{}

func (SQLite) Name() model.Driver { return model.DriverSQLite }

func (SQLite) Quote(id string) string {
	parts := strings.Split(id, ".")
	for i, p := range parts {
		p = strings.Trim(p, "\"`[]")
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}

func (SQLite) Placeholder(int) string { return "?" }

func (SQLite) CollateKey(expr string) string { return expr + " COLLATE BINARY" }

func (SQLite) Literal(v canon.Value, logical model.LogicalType) string {
	if v.Null {
		return "NULL"
	}
	switch logical {
	case model.TypeInt, model.TypeDecimal, model.TypeFloat:
		s := string(v.Raw)
		if s == "" {
			return "NULL"
		}
		return s
	case model.TypeBool:
		if string(v.Raw) == "1" {
			return "1"
		}
		return "0"
	case model.TypeBytes:
		return "X'" + hex.EncodeToString(v.Raw) + "'"
	default:
		return quoteString(string(v.Raw))
	}
}

func (SQLite) BytesLiteral(b []byte) string { return "X'" + hex.EncodeToString(b) + "'" }

func (SQLite) BoolLiteral(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (SQLite) LimitClause(limit, offset int) string {
	if limit <= 0 {
		return ""
	}
	if offset > 0 {
		return fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)
	}
	return fmt.Sprintf(" LIMIT %d", limit)
}

func (SQLite) CurrentChangeToken() string { return "" }

func (SQLite) SupportsRowValue() bool { return true }

func (SQLite) StatementTimeoutSQL(ms int) []string {
	if ms <= 0 {
		return nil
	}
	return []string{fmt.Sprintf("PRAGMA busy_timeout = %d", ms)}
}

// Postgres renders PostgreSQL SQL.
type Postgres struct{}

func (Postgres) Name() model.Driver { return model.DriverPostgres }

func (Postgres) Quote(id string) string {
	parts := strings.Split(id, ".")
	for i, p := range parts {
		p = strings.Trim(p, `"`)
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}

func (Postgres) Placeholder(i int) string { return fmt.Sprintf("$%d", i) }

func (Postgres) CollateKey(expr string) string { return expr + ` COLLATE "C"` }

func (Postgres) Literal(v canon.Value, logical model.LogicalType) string {
	if v.Null {
		return "NULL"
	}
	s := string(v.Raw)
	switch logical {
	case model.TypeInt, model.TypeDecimal, model.TypeFloat:
		if s == "" {
			return "NULL"
		}
		return s
	case model.TypeBool:
		if s == "1" {
			return "TRUE"
		}
		return "FALSE"
	case model.TypeBytes:
		return "'\\x" + hex.EncodeToString(v.Raw) + "'::bytea"
	case model.TypeDate:
		return quoteString(s) + "::date"
	case model.TypeTime:
		return quoteString(s) + "::time"
	case model.TypeDateTime:
		return quoteString(s) + "::timestamptz"
	case model.TypeUUID:
		return quoteString(s) + "::uuid"
	default:
		return quoteString(s)
	}
}

func (Postgres) BytesLiteral(b []byte) string {
	return "'\\x" + hex.EncodeToString(b) + "'::bytea"
}

func (Postgres) BoolLiteral(b bool) string {
	if b {
		return "TRUE"
	}
	return "FALSE"
}

func (Postgres) LimitClause(limit, offset int) string {
	if limit <= 0 {
		return ""
	}
	if offset > 0 {
		return fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)
	}
	return fmt.Sprintf(" LIMIT %d", limit)
}

func (Postgres) CurrentChangeToken() string { return "pg_current_snapshot()::text" }

func (Postgres) SupportsRowValue() bool { return true }

func (Postgres) StatementTimeoutSQL(ms int) []string {
	if ms <= 0 {
		return nil
	}
	return []string{fmt.Sprintf("SET statement_timeout = %d", ms)}
}

// quoteString renders a single quoted SQL string, doubling embedded quotes.
// Backslashes are left alone because PostgreSQL runs with standard_conforming
// strings on by default and SQLite does not interpret them.
func quoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ForDriver returns the dialect for a driver.
func ForDriver(d model.Driver) (Dialect, error) {
	switch d {
	case model.DriverSQLite:
		return SQLite{}, nil
	case model.DriverPostgres:
		return Postgres{}, nil
	}
	return nil, fmt.Errorf("unsupported driver %q", d)
}

// QuoteTime renders a time for a debug preview.
func QuoteTime(t time.Time) string { return quoteString(t.UTC().Format(time.RFC3339Nano)) }
