// Package source exposes a uniform, read only view over a database for the
// comparison engine: schema introspection, key range queries, counts and
// per row extraction of comparable values.
package source

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"bankcmp/internal/canon"
	"bankcmp/internal/dialect"
	"bankcmp/internal/model"
)

// Column is the introspected description of a physical column.
type Column struct {
	Name       string
	DBType     string
	Nullable   bool
	IsPrimary  bool
	Ordinal    int
	CharLength int64
	NumScale   int32
	NumPrec    int32
	Logical    model.LogicalType // inferred
	// BlankPadded is true for PostgreSQL CHAR(n): the engine pads values with
	// spaces, which must be stripped before comparison or every row differs
	// against an unpadded legacy column.
	BlankPadded bool
}

// TableSchema is one introspected table.
type TableSchema struct {
	Name       string
	Columns    []Column
	PrimaryKey []string
	// RowIDExpr is a fallback unique expression when no primary key exists;
	// empty when the table cannot be compared without a declared key.
	RowIDExpr string
}

// Database is a read only comparison source.
type Database struct {
	Driver  model.Driver
	Dial    dialect.Dialect
	DB      *sql.DB
	DSN     string
	Charset string
	// Timezone is the session timezone used for naive timestamp columns.
	Timezone *time.Location
	readOnly bool
	// statementTimeoutMS is applied to every pooled connection.
	statementTimeoutMS int
}

// Config describes how to open a Database.
type Config struct {
	Driver   model.Driver
	DSN      string
	Charset  string
	Timezone string
	ReadOnly bool
	// MaxConns caps the connection pool; the comparison engine opens one
	// connection per worker and relies on this cap for pacing.  Zero picks a
	// default that leaves room for every worker plus the planner.
	MaxConns int
	// StatementTimeoutMS bounds a single statement.  Zero leaves it to the
	// server.  It protects a production database from one runaway query
	// holding a lock or a snapshot for minutes.
	StatementTimeoutMS int
}

// Open opens a database connection pool.
func Open(ctx context.Context, cfg Config) (*Database, error) {
	maxConns := cfg.MaxConns
	if maxConns <= 0 {
		maxConns = 16
	}
	switch cfg.Driver {
	case model.DriverSQLite:
		dsn, err := sqliteDSN(cfg.DSN, cfg.ReadOnly)
		if err != nil {
			return nil, err
		}
		db, err := sql.Open(sqliteDriverName, dsn)
		if err != nil {
			return nil, fmt.Errorf("open sqlite: %w", err)
		}
		db.SetMaxOpenConns(maxConns)
		db.SetMaxIdleConns(maxConns)
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			return nil, fmt.Errorf("ping sqlite: %w", err)
		}
		d := dialect.SQLite{}
		out := &Database{Driver: cfg.Driver, Dial: d, DB: db, DSN: cfg.DSN,
			Charset: cfg.Charset, Timezone: parseLocation(cfg.Timezone), readOnly: cfg.ReadOnly,
			statementTimeoutMS: cfg.StatementTimeoutMS}
		return out, nil
	case model.DriverPostgres:
		dsn, err := pgDSN(cfg.DSN, cfg.Timezone)
		if err != nil {
			return nil, err
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, fmt.Errorf("open postgres: %w", err)
		}
		db.SetMaxOpenConns(maxConns)
		db.SetMaxIdleConns(maxConns)
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			return nil, fmt.Errorf("ping postgres: %w", err)
		}
		d := dialect.Postgres{}
		out := &Database{Driver: cfg.Driver, Dial: d, DB: db, DSN: cfg.DSN,
			Charset: cfg.Charset, Timezone: parseLocation(cfg.Timezone), readOnly: cfg.ReadOnly,
			statementTimeoutMS: cfg.StatementTimeoutMS}
		if cfg.StatementTimeoutMS > 0 {
			if err := applyTimeouts(ctx, db, d, cfg.StatementTimeoutMS); err != nil {
				db.Close()
				return nil, fmt.Errorf("set statement timeout: %w", err)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported driver %q", cfg.Driver)
}

func parseLocation(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.UTC
}

// Close closes the pool.
func (d *Database) Close() error { return d.DB.Close() }

// Conn opens a connection with this database's session settings applied.
func (d *Database) Conn(ctx context.Context) (*sql.Conn, error) {
	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if d.Driver == model.DriverPostgres && d.statementTimeoutMS > 0 {
		// A pooled connection can be reused after its session settings reset,
		// so the timeout is re-applied on checkout.
		if _, err := conn.ExecContext(ctx,
			fmt.Sprintf("SET statement_timeout = %d", d.statementTimeoutMS)); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

// applyTimeouts runs the dialect's timeout statements through every pool
// connection so a fresh connection inherits them.
func applyTimeouts(ctx context.Context, db *sql.DB, d dialect.Dialect, ms int) error {
	for _, stmt := range d.StatementTimeoutSQL(ms) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func sqliteDSN(dsn string, readOnly bool) (string, error) {
	path := dsn
	var query string
	if i := strings.Index(dsn, "?"); i >= 0 {
		path, query = dsn[:i], dsn[i+1:]
	}
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	params := []string{"_busy_timeout=30000", "_synchronous=NORMAL", "mode=" + mode}
	if readOnly {
		params = append(params, "_query_only=true")
	}
	if query != "" {
		params = append(params, query)
	}
	return path + "?" + strings.Join(params, "&"), nil
}

func pgDSN(dsn string, timezone string) (string, error) {
	if timezone == "" {
		timezone = "UTC"
	}
	// Keep the timezone deterministic regardless of the server or client
	// environment: naive timestamps must round trip identically.
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse postgres dsn: %w", err)
		}
		q := u.Query()
		if !q.Has("timezone") && !q.Has("TimeZone") {
			q.Set("timezone", timezone)
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	if !strings.Contains(strings.ToLower(dsn), "timezone=") {
		return dsn + " timezone=" + timezone, nil
	}
	return dsn, nil
}

// Exec runs a statement that returns nothing.
func (d *Database) Exec(ctx context.Context, q string, args ...any) error {
	_, err := d.DB.ExecContext(ctx, q, args...)
	return err
}

// Information is a friendly schema summary for the UI.
type Information struct {
	ServerVersion string
	Tables        []string
}

// Info returns basic server metadata.
func (d *Database) Info(ctx context.Context) (Information, error) {
	var info Information
	switch d.Driver {
	case model.DriverSQLite:
		var v string
		if err := d.DB.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&v); err == nil {
			info.ServerVersion = "SQLite " + v
		}
	case model.DriverPostgres:
		var v string
		if err := d.DB.QueryRowContext(ctx, "SHOW server_version").Scan(&v); err == nil {
			info.ServerVersion = "PostgreSQL " + v
		}
	}
	tables, err := d.Tables(ctx)
	if err != nil {
		return info, err
	}
	info.Tables = tables
	return info, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = canon.FromDriver
