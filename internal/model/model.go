// Package model holds the shared data structures used by the comparison
// engine, the journal, the repair generator and the web UI.
package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Driver identifies a database engine.
type Driver string

const (
	DriverSQLite   Driver = "sqlite"
	DriverPostgres Driver = "postgres"
)

// LogicalType is the engine independent column type used for canonical
// comparison.  Two columns may be stored in different native types (e.g. a
// legacy TEXT fixed point money column against PostgreSQL numeric(18,4)) but
// resolve to the same logical type.
type LogicalType string

const (
	TypeAuto     LogicalType = ""
	TypeInt      LogicalType = "int"
	TypeDecimal  LogicalType = "decimal"
	TypeFloat    LogicalType = "float"
	TypeString   LogicalType = "string"
	TypeBytes    LogicalType = "bytes"
	TypeBool     LogicalType = "bool"
	TypeDate     LogicalType = "date"
	TypeDateTime LogicalType = "datetime"
	TypeTime     LogicalType = "time"
	TypeUUID     LogicalType = "uuid"
)

// Valid reports whether t names a known logical type.
func (t LogicalType) Valid() bool {
	switch t {
	case TypeAuto, TypeInt, TypeDecimal, TypeFloat, TypeString, TypeBytes,
		TypeBool, TypeDate, TypeDateTime, TypeTime, TypeUUID:
		return true
	}
	return false
}

// SQLNameFor returns the SQL type name used for explicit parameter casts on
// a given driver.
func (t LogicalType) SQLNameFor(d Driver) string {
	switch t {
	case TypeInt:
		return "bigint"
	case TypeDecimal:
		return "numeric"
	case TypeFloat:
		return "double precision"
	case TypeBool:
		return "boolean"
	case TypeDate:
		return "date"
	case TypeDateTime:
		return "timestamptz"
	case TypeTime:
		return "time"
	case TypeBytes:
		return "bytea"
	case TypeUUID:
		return "uuid"
	default:
		return "text"
	}
}

// DiffKind classifies a row level difference.
type DiffKind string

const (
	KindMissing  DiffKind = "missing"  // present in source, absent in target
	KindExtra    DiffKind = "extra"    // present in target, absent in source
	KindModified DiffKind = "modified" // present in both, content differs
)

// Stability classifies whether a difference is trustworthy or a result of
// writes happening on the source while the comparison was running.
type Stability string

const (
	StabilityPending   Stability = "pending"   // not rechecked yet
	StabilityStable    Stability = "stable"    // still differs on recheck
	StabilityTransient Stability = "transient" // row changed or converged during the comparison
)

// TaskStatus is the lifecycle state of a comparison task.
type TaskStatus string

const (
	TaskQueued    TaskStatus = "queued"
	TaskRunning   TaskStatus = "running"
	TaskSucceeded TaskStatus = "succeeded"
	TaskFailed    TaskStatus = "failed"
	TaskCanceled  TaskStatus = "canceled"
)

// TableStatus is the lifecycle state of one table inside a task.
type TableStatus string

const (
	TablePending  TableStatus = "pending"
	TableRunning  TableStatus = "running"
	TableEqual    TableStatus = "equal"
	TableDiff     TableStatus = "diff"
	TableFailed   TableStatus = "failed"
	TableSkipped  TableStatus = "skipped"
	TableCanceled TableStatus = "canceled"
)

// Connection is a saved database connection.
type Connection struct {
	ID        int64  `json:"id" yaml:"id"`
	Name      string `json:"name" yaml:"name"`
	Driver    Driver `json:"driver" yaml:"driver"`
	DSN       string `json:"dsn" yaml:"dsn"`
	Charset   string `json:"charset,omitempty" yaml:"charset,omitempty"` // default text charset of this database
	Note      string `json:"note,omitempty" yaml:"note,omitempty"`
	CreatedAt string `json:"created_at" yaml:"created_at"`
}

// Redacted returns a copy with credentials removed, suitable for the UI.
func (c Connection) Redacted() Connection {
	c.DSN = RedactDSN(c.DSN)
	return c
}

// RedactDSN hides passwords in a DSN/URL while keeping it recognisable.
func RedactDSN(dsn string) string {
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		at := strings.LastIndex(rest, "@")
		if at < 0 {
			return dsn
		}
		userinfo := rest[:at]
		if c := strings.Index(userinfo, ":"); c >= 0 {
			rest = userinfo[:c] + ":****" + rest[at:]
			return dsn[:i+3] + rest
		}
		return dsn
	}
	// key=value style DSN
	parts := strings.Fields(dsn)
	for i, p := range parts {
		low := strings.ToLower(p)
		if strings.HasPrefix(low, "password=") || strings.HasPrefix(low, "pass=") ||
			strings.HasPrefix(low, "pwd=") {
			k := p[:strings.Index(p, "=")+1]
			parts[i] = k + "****"
		}
	}
	return strings.Join(parts, " ")
}

// ColumnSpec describes how one logical column is compared.
type ColumnSpec struct {
	// Name is the name on the source side (and, unless TargetName is set,
	// also the name on the target side).
	Name string `json:"name" yaml:"name"`
	// TargetName overrides the target column name (case sensitive).
	TargetName string `json:"target_name,omitempty" yaml:"target_name,omitempty"`
	// Type is the logical type.  Empty means infer from the database schema.
	Type LogicalType `json:"type,omitempty" yaml:"type,omitempty"`
	// Ignore excludes the column from the comparison but keeps it visible in
	// the schema overview.
	Ignore bool `json:"ignore,omitempty" yaml:"ignore,omitempty"`
	// Digest compares only a server side digest of the value instead of
	// transferring the full column.  Intended for CLOB/BLOB columns.
	Digest bool `json:"digest,omitempty" yaml:"digest,omitempty"`
	// SourceCharset is the character set of the raw bytes on the source side
	// (e.g. GBK).  Empty means the connection default / UTF-8.
	SourceCharset string `json:"source_charset,omitempty" yaml:"source_charset,omitempty"`
	// Scale rounds decimal values to this many fractional digits before
	// comparison.  Zero means "no rounding, compare numeric value".
	Scale int32 `json:"scale,omitempty" yaml:"scale,omitempty"`
	// FloatDigits limits the significant digits of a float for hashing.
	// Zero means full float64 round-trip precision.
	FloatDigits int `json:"float_digits,omitempty" yaml:"float_digits,omitempty"`
	// FloatTolerance treats floats closer than this as equal during the row
	// level comparison (it cannot be used for chunk checksums, so a chunk
	// may be flagged and later cleared by the row pass).
	FloatTolerance float64 `json:"float_tolerance,omitempty" yaml:"float_tolerance,omitempty"`
	// TrimSpace trims leading/trailing whitespace before comparison.
	TrimSpace bool `json:"trim_space,omitempty" yaml:"trim_space,omitempty"`
	// CollapseSpace collapses runs of whitespace into one space.
	CollapseSpace bool `json:"collapse_space,omitempty" yaml:"collapse_space,omitempty"`
	// CaseFold compares strings case insensitively.
	CaseFold bool `json:"case_fold,omitempty" yaml:"case_fold,omitempty"`
	// NullEqualsEmpty treats NULL and the empty string / zero bytes as equal.
	NullEqualsEmpty bool `json:"null_equals_empty,omitempty" yaml:"null_equals_empty,omitempty"`
	// Layouts are date/time layouts accepted for string values, in addition
	// to the built in defaults.
	Layouts []string `json:"layouts,omitempty" yaml:"layouts,omitempty"`
	// Timezone is the zone assumed for naive date/time values on this side.
	// Defaults to the connection timezone, else UTC.
	Timezone string `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	// BoolTrue / BoolFalse list textual representations of true/false.
	BoolTrue  []string `json:"bool_true,omitempty" yaml:"bool_true,omitempty"`
	BoolFalse []string `json:"bool_false,omitempty" yaml:"bool_false,omitempty"`
	// Key marks the column as part of the comparison key.
	Key bool `json:"key,omitempty" yaml:"key,omitempty"`
}

// TargetColumn returns the target side column name.
func (c ColumnSpec) TargetColumn() string {
	if c.TargetName != "" {
		return c.TargetName
	}
	return c.Name
}

// TableSpec describes one table pair to compare.
type TableSpec struct {
	Name        string       `json:"name" yaml:"name"`
	SourceTable string       `json:"source_table,omitempty" yaml:"source_table,omitempty"`
	TargetTable string       `json:"target_table,omitempty" yaml:"target_table,omitempty"`
	Columns     []ColumnSpec `json:"columns,omitempty" yaml:"columns,omitempty"`
	// ColumnMode is "all" (default: compare every shared column, the list only
	// overrides options) or "listed" (compare only the listed columns).
	ColumnMode   string   `json:"column_mode,omitempty" yaml:"column_mode,omitempty"`
	KeyColumns   []string `json:"key_columns,omitempty" yaml:"key_columns,omitempty"`
	Ignore       bool     `json:"ignore,omitempty" yaml:"ignore,omitempty"`
	Where        string   `json:"where,omitempty" yaml:"where,omitempty"`                 // optional extra predicate, same dialect on both sides
	SourceWhere  string   `json:"source_where,omitempty" yaml:"source_where,omitempty"`   // overrides Where on source
	TargetWhere  string   `json:"target_where,omitempty" yaml:"target_where,omitempty"`   // overrides Where on target
	ChunkRows    int      `json:"chunk_rows,omitempty" yaml:"chunk_rows,omitempty"`       // rows per coarse range
	DiffMaxRows  int      `json:"diff_max_rows,omitempty" yaml:"diff_max_rows,omitempty"` // max rows materialised during exact diff
	Note         string   `json:"note,omitempty" yaml:"note,omitempty"`
	AllowExecute bool     `json:"allow_execute,omitempty" yaml:"allow_execute,omitempty"`
}

func (t TableSpec) Source() string {
	if t.SourceTable != "" {
		return t.SourceTable
	}
	return t.Name
}

func (t TableSpec) Target() string {
	if t.TargetTable != "" {
		return t.TargetTable
	}
	return t.Name
}

// Options are the global comparison knobs.
type Options struct {
	Workers            int     `json:"workers" yaml:"workers"`                 // tables compared in parallel
	ChunkWorkers       int     `json:"chunk_workers" yaml:"chunk_workers"`     // ranges compared in parallel per table
	ChunkRows          int     `json:"chunk_rows" yaml:"chunk_rows"`           // default coarse chunk size in rows
	DiffMaxRows        int     `json:"diff_max_rows" yaml:"diff_max_rows"`     // exact diff materialisation cap per chunk
	RowsPerSecond      int     `json:"rows_per_second" yaml:"rows_per_second"` // 0 = unlimited, per connection
	QueriesPerSec      int     `json:"queries_per_second" yaml:"queries_per_second"`
	SettleMS           int     `json:"settle_ms" yaml:"settle_ms"` // wait before rechecking transient candidates
	Recheck            bool    `json:"recheck" yaml:"recheck"`     // re-read differing rows to classify stability
	StatementTimeoutMS int     `json:"statement_timeout_ms" yaml:"statement_timeout_ms"`
	CountMode          string  `json:"count_mode" yaml:"count_mode"` // exact | estimate
	ReportEveryMS      int     `json:"report_every_ms" yaml:"report_every_ms"`
	MaxDiffStore       int     `json:"max_diff_store" yaml:"max_diff_store"` // max diff rows stored per table (0 = unlimited)
	SampleMode         bool    `json:"sample_mode" yaml:"sample_mode"`       // compare only a sample (testing)
	SamplePercent      float64 `json:"sample_percent" yaml:"sample_percent"`
}

// DefaultOptions returns sensible defaults for a local proof run.
func DefaultOptions() Options {
	return Options{
		Workers:            2,
		ChunkWorkers:       4,
		ChunkRows:          20000,
		DiffMaxRows:        50000,
		RowsPerSecond:      0,
		QueriesPerSec:      0,
		SettleMS:           500,
		Recheck:            true,
		StatementTimeoutMS: 120000,
		CountMode:          "exact",
		ReportEveryMS:      500,
		MaxDiffStore:       0,
	}
}

// Config is the top level YAML configuration.
type Config struct {
	Source      Endpoint    `yaml:"source" json:"source"`
	Target      Endpoint    `yaml:"target" json:"target"`
	Options     Options     `yaml:"options" json:"options"`
	Tables      []TableSpec `yaml:"tables" json:"tables"`
	Description string      `yaml:"description,omitempty" json:"description,omitempty"`
}

// Endpoint is one side of the comparison in a config file.
type Endpoint struct {
	Driver   Driver `yaml:"driver" json:"driver"`
	DSN      string `yaml:"dsn" json:"dsn"`
	Charset  string `yaml:"charset,omitempty" json:"charset,omitempty"`
	Timezone string `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	// ReadOnly opens SQLite databases read only and sets PG sessions to
	// read-only where supported.
	ReadOnly bool `yaml:"read_only,omitempty" json:"read_only,omitempty"`
}

// TableRun is the per-table result row stored in the journal and returned by
// the API.
type TableRun struct {
	ID            int64       `json:"id" yaml:"id"`
	TaskID        int64       `json:"task_id" yaml:"task_id"`
	TableName     string      `json:"table_name" yaml:"table_name"`
	Status        TableStatus `json:"status" yaml:"status"`
	SourceRows    int64       `json:"source_rows" yaml:"source_rows"`
	TargetRows    int64       `json:"target_rows" yaml:"target_rows"`
	ChunksTotal   int         `json:"chunks_total" yaml:"chunks_total"`
	ChunksDone    int         `json:"chunks_done" yaml:"chunks_done"`
	Diffs         int64       `json:"diffs" yaml:"diffs"`
	Missing       int64       `json:"missing" yaml:"missing"`
	Extra         int64       `json:"extra" yaml:"extra"`
	Modified      int64       `json:"modified" yaml:"modified"`
	Stable        int64       `json:"stable" yaml:"stable"`
	Transient     int64       `json:"transient" yaml:"transient"`
	DurationMS    int64       `json:"duration_ms" yaml:"duration_ms"`
	Error         string      `json:"error,omitempty" yaml:"error,omitempty"`
	StartedAt     string      `json:"started_at,omitempty" yaml:"started_at,omitempty"`
	FinishedAt    string      `json:"finished_at,omitempty" yaml:"finished_at,omitempty"`
	SourceCountMS int64       `json:"source_count_ms" yaml:"source_count_ms"`
	TargetCountMS int64       `json:"target_count_ms" yaml:"target_count_ms"`
	// SourceChanged reports that the source was written to during this table's
	// comparison, so some differences may be transient.
	SourceChanged bool `json:"source_changed" yaml:"source_changed"`
	TargetChanged bool `json:"target_changed" yaml:"target_changed"`
}

// ChunkResult records the comparison of one key range.
type ChunkResult struct {
	Seq        int    `json:"seq" yaml:"seq"`
	Lo         string `json:"lo,omitempty" yaml:"lo,omitempty"`
	Hi         string `json:"hi,omitempty" yaml:"hi,omitempty"`
	SourceRows int64  `json:"source_rows" yaml:"source_rows"`
	TargetRows int64  `json:"target_rows" yaml:"target_rows"`
	Matched    bool   `json:"matched" yaml:"matched"`
	Diffs      int64  `json:"diffs" yaml:"diffs"`
	DurationMS int64  `json:"duration_ms" yaml:"duration_ms"`
	Depth      int    `json:"depth" yaml:"depth"`
	Error      string `json:"error,omitempty" yaml:"error,omitempty"`
}

// Diff is one row level difference.
type Diff struct {
	ID         int64             `json:"id" yaml:"id"`
	TableRunID int64             `json:"table_run_id" yaml:"table_run_id"`
	TableName  string            `json:"table_name" yaml:"table_name"`
	Kind       DiffKind          `json:"kind" yaml:"kind"`
	Stability  Stability         `json:"stability" yaml:"stability"`
	PK         string            `json:"pk" yaml:"pk"`               // canonical, human readable
	PKValues   []KeyValue        `json:"pk_values" yaml:"pk_values"` // ordered key column values
	Columns    []ColumnDiff      `json:"columns,omitempty" yaml:"columns,omitempty"`
	SourceRow  map[string]string `json:"source_row,omitempty" yaml:"source_row,omitempty"` // canonical preview, truncated
	TargetRow  map[string]string `json:"target_row,omitempty" yaml:"target_row,omitempty"`
	RepairSQL  string            `json:"repair_sql,omitempty" yaml:"repair_sql,omitempty"`
	UndoSQL    string            `json:"undo_sql,omitempty" yaml:"undo_sql,omitempty"`
	Note       string            `json:"note,omitempty" yaml:"note,omitempty"`
	CreatedAt  string            `json:"created_at,omitempty" yaml:"created_at,omitempty"`
	// SourceFP / TargetFP are the canonical row fingerprints observed during
	// the comparison, used to decide whether a row changed afterwards.  They
	// are internal and never serialised.
	SourceFP string `json:"-" yaml:"-"`
	TargetFP string `json:"-" yaml:"-"`
	// SourceXmin is the PostgreSQL tuple id observed for the source row.
	SourceXmin string `json:"-" yaml:"-"`
}

// KeyValue is one key column value rendered for display and repair SQL.
type KeyValue struct {
	Column string      `json:"column" yaml:"column"`
	Value  string      `json:"value" yaml:"value"` // canonical string; empty + Null for NULL
	Null   bool        `json:"null,omitempty" yaml:"null,omitempty"`
	Type   LogicalType `json:"type,omitempty" yaml:"type,omitempty"`
}

// ColumnDiff is one differing column inside a modified row.
type ColumnDiff struct {
	Column string `json:"column" yaml:"column"`
	Source string `json:"source" yaml:"source"`
	Target string `json:"target" yaml:"target"`
	// Changed is false when the two side values are considered equal after
	// normalisation (e.g. a float within tolerance), in which case the row may
	// end up not being reported.
	Changed bool `json:"changed" yaml:"changed"`
}

// RepairBatch is a generated, previewed set of repair statements.
type RepairBatch struct {
	ID         int64  `json:"id" yaml:"id"`
	TaskID     int64  `json:"task_id" yaml:"task_id"`
	TableRunID int64  `json:"table_run_id,omitempty" yaml:"table_run_id,omitempty"`
	TableName  string `json:"table_name,omitempty" yaml:"table_name,omitempty"`
	Kinds      string `json:"kinds,omitempty" yaml:"kinds,omitempty"`
	Statements int    `json:"statements" yaml:"statements"`
	SQL        string `json:"sql" yaml:"sql"`
	UndoSQL    string `json:"undo_sql,omitempty" yaml:"undo_sql,omitempty"`
	Status     string `json:"status" yaml:"status"`
	CreatedAt  string `json:"created_at" yaml:"created_at"`
	ExecutedAt string `json:"executed_at,omitempty" yaml:"executed_at,omitempty"`
	Result     string `json:"result,omitempty" yaml:"result,omitempty"`
}

// Report is the task level comparison report.
type Report struct {
	TaskID       int64      `json:"task_id" yaml:"task_id"`
	TaskName     string     `json:"task_name" yaml:"task_name"`
	GeneratedAt  string     `json:"generated_at" yaml:"generated_at"`
	SourceDSN    string     `json:"source_dsn" yaml:"source_dsn"`
	TargetDSN    string     `json:"target_dsn" yaml:"target_dsn"`
	Options      Options    `json:"options" yaml:"options"`
	TotalTables  int        `json:"total_tables" yaml:"total_tables"`
	EqualTables  int        `json:"equal_tables" yaml:"equal_tables"`
	DiffTables   int        `json:"diff_tables" yaml:"diff_tables"`
	FailedTables int        `json:"failed_tables" yaml:"failed_tables"`
	SourceRows   int64      `json:"source_rows" yaml:"source_rows"`
	TargetRows   int64      `json:"target_rows" yaml:"target_rows"`
	TotalDiffs   int64      `json:"total_diffs" yaml:"total_diffs"`
	Missing      int64      `json:"missing" yaml:"missing"`
	Extra        int64      `json:"extra" yaml:"extra"`
	Modified     int64      `json:"modified" yaml:"modified"`
	Stable       int64      `json:"stable" yaml:"stable"`
	Transient    int64      `json:"transient" yaml:"transient"`
	DurationMS   int64      `json:"duration_ms" yaml:"duration_ms"`
	Tables       []TableRun `json:"tables" yaml:"tables"`
	// TablesWithSourceWrites counts tables whose source was written during the
	// comparison; those reports carry a stability caveat.
	TablesWithSourceWrites int `json:"tables_with_source_writes"`
}

// Event is one log line attached to a task, streamed to the dashboard.
type Event struct {
	ID     int64  `json:"id" yaml:"id"`
	TaskID int64  `json:"task_id" yaml:"task_id"`
	TS     string `json:"ts" yaml:"ts"`
	Level  string `json:"level" yaml:"level"`
	Msg    string `json:"msg" yaml:"msg"`
}

// ParsePK encodes key values as a compact human readable string.
func ParsePK(kvs []KeyValue) string {
	parts := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		if kv.Null {
			parts = append(parts, "NULL")
			continue
		}
		parts = append(parts, kv.Value)
	}
	return strings.Join(parts, "|")
}

// FormatFloat is a tiny helper used by SQL generation and previews.
func FormatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// JSON is a convenience used by journal columns.
func JSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%q", fmt.Sprint(v))
	}
	return string(b)
}

// Truncate shortens s for previews, marking it with an ellipsis.
func Truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	// avoid splitting a UTF-8 rune
	cut := n
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…", true
}
