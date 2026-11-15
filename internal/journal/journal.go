// Package journal persists tasks, per table runs, differences, repair
// batches and events so the dashboard can show progress and history without
// holding a comparison in memory.
package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"bankcmp/internal/model"
)

// Store is the journal database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (and migrates) the journal at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := path + "?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL&_foreign_keys=ON"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	s := &Store{db: db, path: path}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the journal.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for read only helpers.
func (s *Store) DB() *sql.DB { return s.db }

const schemaSQL = `
CREATE TABLE IF NOT EXISTS connections (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL,
  driver      TEXT NOT NULL,
  dsn         TEXT NOT NULL,
  charset     TEXT NOT NULL DEFAULT '',
  note        TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT NOT NULL,
  source_id     INTEGER,
  target_id     INTEGER,
  source_driver TEXT NOT NULL,
  source_dsn    TEXT NOT NULL,
  target_driver TEXT NOT NULL,
  target_dsn    TEXT NOT NULL,
  options_json  TEXT NOT NULL,
  config_json   TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL,
  error         TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL,
  started_at    TEXT NOT NULL DEFAULT '',
  finished_at   TEXT NOT NULL DEFAULT '',
  duration_ms   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS table_runs (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id         INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  table_name      TEXT NOT NULL,
  status          TEXT NOT NULL,
  source_rows     INTEGER NOT NULL DEFAULT 0,
  target_rows     INTEGER NOT NULL DEFAULT 0,
  chunks_total    INTEGER NOT NULL DEFAULT 0,
  chunks_done     INTEGER NOT NULL DEFAULT 0,
  diffs           INTEGER NOT NULL DEFAULT 0,
  missing         INTEGER NOT NULL DEFAULT 0,
  extra           INTEGER NOT NULL DEFAULT 0,
  modified        INTEGER NOT NULL DEFAULT 0,
  stable          INTEGER NOT NULL DEFAULT 0,
  transient       INTEGER NOT NULL DEFAULT 0,
  duration_ms     INTEGER NOT NULL DEFAULT 0,
  source_count_ms INTEGER NOT NULL DEFAULT 0,
  target_count_ms INTEGER NOT NULL DEFAULT 0,
  error           TEXT NOT NULL DEFAULT '',
  started_at      TEXT NOT NULL DEFAULT '',
  finished_at     TEXT NOT NULL DEFAULT '',
  chunks_json     TEXT NOT NULL DEFAULT '',
  source_changed  INTEGER NOT NULL DEFAULT 0,
  target_changed  INTEGER NOT NULL DEFAULT 0,
  UNIQUE(task_id, table_name)
);
CREATE TABLE IF NOT EXISTS diffs (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id       INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  table_run_id  INTEGER NOT NULL REFERENCES table_runs(id) ON DELETE CASCADE,
  table_name    TEXT NOT NULL,
  kind          TEXT NOT NULL,
  stability     TEXT NOT NULL,
  pk            TEXT NOT NULL,
  pk_json       TEXT NOT NULL,
  columns_json  TEXT NOT NULL DEFAULT '',
  source_json   TEXT NOT NULL DEFAULT '',
  target_json   TEXT NOT NULL DEFAULT '',
  note          TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS diffs_task_table ON diffs(task_id, table_name, kind);
CREATE TABLE IF NOT EXISTS repairs (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id      INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  table_run_id INTEGER NOT NULL DEFAULT 0,
  table_name   TEXT NOT NULL DEFAULT '',
  kinds        TEXT NOT NULL DEFAULT '',
  statements   INTEGER NOT NULL DEFAULT 0,
  sql          TEXT NOT NULL,
  undo_sql     TEXT NOT NULL DEFAULT '',
  status       TEXT NOT NULL,
  result       TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  executed_at  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS events (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  ts      TEXT NOT NULL,
  level   TEXT NOT NULL,
  msg     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_task ON events(task_id, id);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("migrate journal: %w", err)
	}
	// Additive migrations for journals created by an older build.  ALTER TABLE
	// ADD COLUMN fails with "duplicate column name" when already present, so
	// the error is matched rather than ignored wholesale.
	for _, stmt := range []string{
		`ALTER TABLE table_runs ADD COLUMN source_changed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE table_runs ADD COLUMN target_changed INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate journal: %w", err)
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// SaveConnection inserts or updates a connection.
func (s *Store) SaveConnection(ctx context.Context, c *model.Connection) error {
	if c.ID == 0 {
		c.CreatedAt = now()
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO connections(name, driver, dsn, charset, note, created_at) VALUES (?,?,?,?,?,?)`,
			c.Name, string(c.Driver), c.DSN, c.Charset, c.Note, c.CreatedAt)
		if err != nil {
			return err
		}
		c.ID, _ = res.LastInsertId()
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE connections SET name=?, driver=?, dsn=?, charset=?, note=? WHERE id=?`,
		c.Name, string(c.Driver), c.DSN, c.Charset, c.Note, c.ID)
	return err
}

// GetConnection loads one connection.
func (s *Store) GetConnection(ctx context.Context, id int64) (*model.Connection, error) {
	c := &model.Connection{}
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, driver, dsn, charset, note, created_at FROM connections WHERE id=?`, id).
		Scan(&c.ID, &c.Name, &c.Driver, &c.DSN, &c.Charset, &c.Note, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ListConnections returns all saved connections.
func (s *Store) ListConnections(ctx context.Context) ([]model.Connection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, driver, dsn, charset, note, created_at FROM connections ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Connection{}
	for rows.Next() {
		var c model.Connection
		if err := rows.Scan(&c.ID, &c.Name, &c.Driver, &c.DSN, &c.Charset, &c.Note, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConnection removes a connection.
func (s *Store) DeleteConnection(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connections WHERE id=?`, id)
	return err
}

// Task is a persisted comparison task.
type Task struct {
	ID           int64        `json:"id"`
	Name         string       `json:"name"`
	SourceID     int64        `json:"source_id"`
	TargetID     int64        `json:"target_id"`
	SourceDriver model.Driver `json:"source_driver"`
	// SourceDSN is redacted in API responses.
	SourceDSN    string           `json:"source_dsn"`
	TargetDriver model.Driver     `json:"target_driver"`
	TargetDSN    string           `json:"target_dsn"`
	Options      model.Options    `json:"options"`
	Config       string           `json:"config"`
	Status       model.TaskStatus `json:"status"`
	Error        string           `json:"error,omitempty"`
	CreatedAt    string           `json:"created_at"`
	StartedAt    string           `json:"started_at,omitempty"`
	FinishedAt   string           `json:"finished_at,omitempty"`
	DurationMS   int64            `json:"duration_ms"`
}

// CreateTask stores a task and returns it with an ID.
func (s *Store) CreateTask(ctx context.Context, t *Task) error {
	t.CreatedAt = now()
	opts, _ := json.Marshal(t.Options)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks(name, source_id, target_id, source_driver, source_dsn, target_driver, target_dsn,
		                  options_json, config_json, status, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		t.Name, t.SourceID, t.TargetID, string(t.SourceDriver), t.SourceDSN,
		string(t.TargetDriver), t.TargetDSN, string(opts), t.Config, string(model.TaskQueued), t.CreatedAt)
	if err != nil {
		return err
	}
	t.ID, _ = res.LastInsertId()
	t.Status = model.TaskQueued
	return nil
}

// UpdateTaskStatus sets the status (and timestamps) of a task.
func (s *Store) UpdateTaskStatus(ctx context.Context, id int64, status model.TaskStatus, errMsg string) error {
	switch status {
	case model.TaskRunning:
		_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status=?, started_at=? WHERE id=?`,
			string(status), now(), id)
		return err
	case model.TaskSucceeded, model.TaskFailed, model.TaskCanceled:
		_, err := s.db.ExecContext(ctx, `
			UPDATE tasks SET status=?, error=?, finished_at=?,
			                 duration_ms = CAST((julianday(?) - julianday(created_at)) * 86400000 AS INTEGER)
			 WHERE id=?`, string(status), errMsg, now(), now(), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET status=?, error=? WHERE id=?`, string(status), errMsg, id)
	return err
}

// Redacted returns a copy safe to send to clients.
func (t *Task) Redacted() *Task {
	c := *t
	c.SourceDSN = model.RedactDSN(t.SourceDSN)
	c.TargetDSN = model.RedactDSN(t.TargetDSN)
	return &c
}

// GetTask loads a task.
func (s *Store) GetTask(ctx context.Context, id int64) (*Task, error) {
	t := &Task{}
	var opts string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, COALESCE(source_id,0), COALESCE(target_id,0), source_driver, source_dsn,
		       target_driver, target_dsn, options_json, config_json, status, error, created_at,
		       started_at, finished_at, duration_ms
		  FROM tasks WHERE id=?`, id).
		Scan(&t.ID, &t.Name, &t.SourceID, &t.TargetID, &t.SourceDriver, &t.SourceDSN,
			&t.TargetDriver, &t.TargetDSN, &opts, &t.Config, &t.Status, &t.Error, &t.CreatedAt,
			&t.StartedAt, &t.FinishedAt, &t.DurationMS)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(opts), &t.Options)
	return t, nil
}

// ListTasks returns tasks, newest first.
func (s *Store) ListTasks(ctx context.Context, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, COALESCE(source_id,0), COALESCE(target_id,0), source_driver, source_dsn,
		       target_driver, target_dsn, options_json, config_json, status, error, created_at,
		       started_at, finished_at, duration_ms
		  FROM tasks ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var t Task
		var opts string
		if err := rows.Scan(&t.ID, &t.Name, &t.SourceID, &t.TargetID, &t.SourceDriver, &t.SourceDSN,
			&t.TargetDriver, &t.TargetDSN, &opts, &t.Config, &t.Status, &t.Error, &t.CreatedAt,
			&t.StartedAt, &t.FinishedAt, &t.DurationMS); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(opts), &t.Options)
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpsertTableRun inserts or replaces a table run and returns its id.
func (s *Store) UpsertTableRun(ctx context.Context, tr *model.TableRun, chunks []model.ChunkResult) (int64, error) {
	cj, _ := json.Marshal(chunks)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO table_runs(task_id, table_name, status, source_rows, target_rows, chunks_total, chunks_done,
		                       diffs, missing, extra, modified, stable, transient, duration_ms,
		                       source_count_ms, target_count_ms, error, started_at, finished_at, chunks_json,
		                       source_changed, target_changed)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(task_id, table_name) DO UPDATE SET
		  status=excluded.status, source_rows=excluded.source_rows, target_rows=excluded.target_rows,
		  chunks_total=excluded.chunks_total, chunks_done=excluded.chunks_done, diffs=excluded.diffs,
		  missing=excluded.missing, extra=excluded.extra, modified=excluded.modified,
		  stable=excluded.stable, transient=excluded.transient, duration_ms=excluded.duration_ms,
		  source_count_ms=excluded.source_count_ms, target_count_ms=excluded.target_count_ms,
		  error=excluded.error, started_at=excluded.started_at, finished_at=excluded.finished_at,
		  chunks_json=excluded.chunks_json,
		  source_changed=excluded.source_changed, target_changed=excluded.target_changed`,
		tr.TaskID, tr.TableName, string(tr.Status), tr.SourceRows, tr.TargetRows, tr.ChunksTotal,
		tr.ChunksDone, tr.Diffs, tr.Missing, tr.Extra, tr.Modified, tr.Stable, tr.Transient,
		tr.DurationMS, tr.SourceCountMS, tr.TargetCountMS, tr.Error, tr.StartedAt, tr.FinishedAt, string(cj),
		boolInt(tr.SourceChanged), boolInt(tr.TargetChanged))
	if err != nil {
		return 0, err
	}
	// Always resolve the id: on conflict LastInsertId is stale.
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM table_runs WHERE task_id=? AND table_name=?`,
		tr.TaskID, tr.TableName).Scan(&tr.ID); err != nil {
		return 0, err
	}
	_ = res
	return tr.ID, nil
}

// SetTableRunStatus updates only the status of a table run.
func (s *Store) SetTableRunStatus(ctx context.Context, taskID int64, table string, status model.TableStatus, started string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO table_runs(task_id, table_name, status, started_at) VALUES (?,?,?,?)
		ON CONFLICT(task_id, table_name) DO UPDATE SET status=excluded.status, started_at=excluded.started_at`,
		taskID, table, string(status), started)
	return err
}

// ListTableRuns returns the table runs of a task.
func (s *Store) ListTableRuns(ctx context.Context, taskID int64) ([]model.TableRun, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, table_name, status, source_rows, target_rows, chunks_total, chunks_done,
		       diffs, missing, extra, modified, stable, transient, duration_ms, error,
		       started_at, finished_at, source_count_ms, target_count_ms,
		       source_changed, target_changed
		  FROM table_runs WHERE task_id=? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.TableRun{}
	for rows.Next() {
		var r model.TableRun
		if err := rows.Scan(&r.ID, &r.TaskID, &r.TableName, &r.Status, &r.SourceRows, &r.TargetRows,
			&r.ChunksTotal, &r.ChunksDone, &r.Diffs, &r.Missing, &r.Extra, &r.Modified,
			&r.Stable, &r.Transient, &r.DurationMS, &r.Error, &r.StartedAt, &r.FinishedAt,
			&r.SourceCountMS, &r.TargetCountMS,
			&r.SourceChanged, &r.TargetChanged); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TableRunChunks returns the stored chunk results for a table run.
func (s *Store) TableRunChunks(ctx context.Context, taskID int64, table string) ([]model.ChunkResult, error) {
	var cj string
	err := s.db.QueryRowContext(ctx, `SELECT chunks_json FROM table_runs WHERE task_id=? AND table_name=?`,
		taskID, table).Scan(&cj)
	if err != nil {
		return nil, err
	}
	chunks := []model.ChunkResult{}
	if cj != "" {
		_ = json.Unmarshal([]byte(cj), &chunks)
	}
	return chunks, nil
}

// InsertDiffs stores differences for a table run.
func (s *Store) InsertDiffs(ctx context.Context, taskID, tableRunID int64, diffs []model.Diff) error {
	if len(diffs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx, `
		INSERT INTO diffs(task_id, table_run_id, table_name, kind, stability, pk, pk_json, columns_json,
		                  source_json, target_json, note, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer st.Close()
	ts := now()
	for _, d := range diffs {
		pkJSON, _ := json.Marshal(d.PKValues)
		colJSON, _ := json.Marshal(d.Columns)
		srcJSON, _ := json.Marshal(d.SourceRow)
		tgtJSON, _ := json.Marshal(d.TargetRow)
		if _, err := st.ExecContext(ctx, taskID, tableRunID, d.TableName, string(d.Kind), string(d.Stability),
			d.PK, string(pkJSON), string(colJSON), string(srcJSON), string(tgtJSON), d.Note, ts); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Mirror the note about the transient/stability tally on the table run.
	return nil
}

// DeleteDiffs removes stored diffs of a table run (used when a rerun replaces them).
func (s *Store) DeleteDiffs(ctx context.Context, taskID int64, table string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM diffs WHERE task_id=? AND table_name=?`, taskID, table)
	return err
}

// DiffQuery filters the difference list.
type DiffQuery struct {
	TaskID    int64
	Table     string
	Kind      string
	Stability string
	Search    string
	Limit     int
	Offset    int
}

// ListDiffs returns stored differences and the total count.
func (s *Store) ListDiffs(ctx context.Context, q DiffQuery) ([]model.Diff, int, error) {
	var conds []string
	var args []any
	conds = append(conds, "task_id=?")
	args = append(args, q.TaskID)
	if q.Table != "" {
		conds = append(conds, "table_name=?")
		args = append(args, q.Table)
	}
	if q.Kind != "" {
		conds = append(conds, "kind=?")
		args = append(args, q.Kind)
	}
	if q.Stability != "" {
		conds = append(conds, "stability=?")
		args = append(args, q.Stability)
	}
	if q.Search != "" {
		conds = append(conds, "(pk LIKE ? OR columns_json LIKE ? OR source_json LIKE ? OR target_json LIKE ?)")
		like := "%" + q.Search + "%"
		args = append(args, like, like, like, like)
	}
	where := " WHERE " + strings.Join(conds, " AND ")
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM diffs"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	sqlq := `SELECT id, table_run_id, table_name, kind, stability, pk, pk_json, columns_json,
	                source_json, target_json, note, created_at FROM diffs` + where +
		` ORDER BY table_name, kind, id LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, sqlq, append(args, limit, q.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []model.Diff{}
	for rows.Next() {
		var d model.Diff
		var pkJSON, colJSON, srcJSON, tgtJSON string
		d.TableRunID = 0
		if err := rows.Scan(&d.ID, &d.TableRunID, &d.TableName, &d.Kind, &d.Stability, &d.PK,
			&pkJSON, &colJSON, &srcJSON, &tgtJSON, &d.Note, &d.CreatedAt); err != nil {
			return nil, 0, err
		}
		_ = json.Unmarshal([]byte(pkJSON), &d.PKValues)
		_ = json.Unmarshal([]byte(colJSON), &d.Columns)
		_ = json.Unmarshal([]byte(srcJSON), &d.SourceRow)
		_ = json.Unmarshal([]byte(tgtJSON), &d.TargetRow)
		out = append(out, d)
	}
	return out, total, rows.Err()
}

// GetDiff loads one difference.
func (s *Store) GetDiff(ctx context.Context, id int64) (*model.Diff, error) {
	var d model.Diff
	var pkJSON, colJSON, srcJSON, tgtJSON string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, table_run_id, table_name, kind, stability, pk, pk_json, columns_json,
		       source_json, target_json, note, created_at FROM diffs WHERE id=?`, id).
		Scan(&d.ID, &d.TableRunID, &d.TableName, &d.Kind, &d.Stability, &d.PK,
			&pkJSON, &colJSON, &srcJSON, &tgtJSON, &d.Note, &d.CreatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(pkJSON), &d.PKValues)
	_ = json.Unmarshal([]byte(colJSON), &d.Columns)
	_ = json.Unmarshal([]byte(srcJSON), &d.SourceRow)
	_ = json.Unmarshal([]byte(tgtJSON), &d.TargetRow)
	return &d, nil
}

// DiffCounts summarises a task's differences per kind.
func (s *Store) DiffCounts(ctx context.Context, taskID int64) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM diffs WHERE task_id=? GROUP BY kind`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// AddEvent records a log line for a task.
func (s *Store) AddEvent(ctx context.Context, taskID int64, level, msg string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events(task_id, ts, level, msg) VALUES (?,?,?,?)`,
		taskID, now(), level, msg)
	return err
}

// ListEvents returns events after a given id.
func (s *Store) ListEvents(ctx context.Context, taskID, afterID int64, limit int) ([]model.Event, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, ts, level, msg FROM events WHERE task_id=? AND id>? ORDER BY id LIMIT ?`,
		taskID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		var e model.Event
		if err := rows.Scan(&e.ID, &e.TaskID, &e.TS, &e.Level, &e.Msg); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SaveRepair stores a generated repair batch.
func (s *Store) SaveRepair(ctx context.Context, r *model.RepairBatch) error {
	r.CreatedAt = now()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO repairs(task_id, table_run_id, table_name, kinds, statements, sql, undo_sql, status, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		r.TaskID, r.TableRunID, r.TableName, r.Kinds, r.Statements, r.SQL, r.UndoSQL, r.Status, r.CreatedAt)
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

// UpdateRepairStatus records the outcome of an executed batch.
func (s *Store) UpdateRepairStatus(ctx context.Context, id int64, status, result string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE repairs SET status=?, result=?, executed_at=? WHERE id=?`,
		status, result, now(), id)
	return err
}

// GetRepair loads one repair batch.
func (s *Store) GetRepair(ctx context.Context, id int64) (*model.RepairBatch, error) {
	r := &model.RepairBatch{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, task_id, table_run_id, table_name, kinds, statements, sql, undo_sql, status,
		       created_at, executed_at, result FROM repairs WHERE id=?`, id).
		Scan(&r.ID, &r.TaskID, &r.TableRunID, &r.TableName, &r.Kinds, &r.Statements, &r.SQL,
			&r.UndoSQL, &r.Status, &r.CreatedAt, &r.ExecutedAt, &r.Result)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// ListRepairs returns repair batches for a task.
func (s *Store) ListRepairs(ctx context.Context, taskID int64) ([]model.RepairBatch, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, table_run_id, table_name, kinds, statements, sql, undo_sql, status,
		       created_at, executed_at, result FROM repairs WHERE task_id=? ORDER BY id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.RepairBatch{}
	for rows.Next() {
		var r model.RepairBatch
		if err := rows.Scan(&r.ID, &r.TaskID, &r.TableRunID, &r.TableName, &r.Kinds, &r.Statements,
			&r.SQL, &r.UndoSQL, &r.Status, &r.CreatedAt, &r.ExecutedAt, &r.Result); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteTask removes a task and everything attached to it.
func (s *Store) DeleteTask(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id=?`, id)
	return err
}

// ErrNotFound is returned when a row is missing.
var ErrNotFound = errors.New("not found")
