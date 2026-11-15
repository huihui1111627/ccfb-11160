package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bankcmp/internal/app"
	"bankcmp/internal/engine"
	"bankcmp/internal/journal"
	"bankcmp/internal/model"
	"bankcmp/internal/plan"
	"bankcmp/internal/source"
	"bankcmp/internal/sqlgen"
)

// repairRequest asks for repair SQL for one table of a task.
type repairRequest struct {
	Table            string   `json:"table"`
	Kinds            []string `json:"kinds"`
	IncludeTransient bool     `json:"include_transient"`
	// DryRun previews without storing.
	DryRun bool `json:"dry_run"`
}

// POST /api/tasks/{id}/repairs
func (s *Server) createRepair(w http.ResponseWriter, r *http.Request, taskID int64) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req repairRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Table == "" {
		writeErr(w, http.StatusBadRequest, "table is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	rep, warn, err := s.generateRepair(ctx, taskID, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !req.DryRun {
		if err := s.Store.SaveRepair(ctx, rep); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	out := map[string]any{"repair": rep}
	if len(warn) > 0 {
		out["warnings"] = warn
	}
	writeJSON(w, http.StatusOK, out)
}

// generateRepair builds the SQL for a table run.
func (s *Server) generateRepair(ctx context.Context, taskID int64, req repairRequest) (*model.RepairBatch, []string, error) {
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return nil, nil, fmt.Errorf("task not found")
	}
	var kinds []model.DiffKind
	for _, k := range req.Kinds {
		switch model.DiffKind(k) {
		case model.KindMissing, model.KindExtra, model.KindModified:
			kinds = append(kinds, model.DiffKind(k))
		default:
			return nil, nil, fmt.Errorf("unknown diff kind %q", k)
		}
	}
	opts := sqlgen.Options{Kinds: kinds, IncludeTransient: req.IncludeTransient}

	// Load the stored diffs of this table (paged to respect memory).
	var diffs []model.Diff
	offset := 0
	for {
		batch, total, err := s.Store.ListDiffs(ctx, journal.DiffQuery{
			TaskID: taskID, Table: req.Table, Limit: 1000, Offset: offset,
		})
		if err != nil {
			return nil, nil, err
		}
		diffs = append(diffs, batch...)
		offset += len(batch)
		if offset >= total || len(batch) == 0 {
			break
		}
	}
	selected, skipped := opts.Prepare(diffs)
	if len(selected) == 0 {
		return &model.RepairBatch{TaskID: taskID, TableName: req.Table, Status: "preview",
			Statements: 0, SQL: "", Kinds: ""}, []string{"no differences selected for repair"}, nil
	}

	cfg, err := app.ParseConfig(task.Config)
	if err != nil {
		return nil, nil, err
	}
	spec, ok := findSpec(cfg, req.Table)
	if !ok {
		return nil, nil, fmt.Errorf("table %q is not part of the task configuration", req.Table)
	}
	// Repair SQL must be built from real column values.  A digest column only
	// ever exposes a SHA-256 of the value, so the digest flag is cleared for
	// this read; writing a digest back would corrupt the target column.
	spec = withoutDigest(spec)
	tgt, err := source.Open(ctx, source.Config{
		Driver: task.TargetDriver, DSN: task.TargetDSN, Charset: cfg.Target.Charset,
		Timezone: "UTC", MaxConns: 8,
		// Generation only reads; the write happens later through the explicit
		// execute endpoint.
		ReadOnly: true, StatementTimeoutMS: defaultTimeoutMS(task.Options),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open target: %w", err)
	}
	defer tgt.Close()
	src, err := source.Open(ctx, source.Config{
		Driver: task.SourceDriver, DSN: task.SourceDSN, Charset: cfg.Source.Charset,
		Timezone: "UTC", ReadOnly: true, MaxConns: 4,
		StatementTimeoutMS: defaultTimeoutMS(task.Options),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	ss, err := src.Introspect(ctx, spec.Source())
	if err != nil {
		return nil, nil, err
	}
	ts, err := tgt.Introspect(ctx, spec.Target())
	if err != nil {
		return nil, nil, err
	}
	tp, err := plan.Build(spec, src, tgt, ss, ts, task.Options)
	if err != nil {
		return nil, nil, err
	}
	in := sqlgen.Input{SourceRows: map[string]sqlgen.SourceRow{}, TargetRows: map[string]sqlgen.SourceRow{}}
	runner := engine.NewRunner(src, tgt, engine.Options{})
	// Build partial diffs to look up only the keys we need.
	var needSrc, needTgt []model.Diff
	for _, d := range selected {
		switch d.Kind {
		case model.KindMissing, model.KindModified:
			needSrc = append(needSrc, d)
		case model.KindExtra:
			needTgt = append(needTgt, d)
		}
	}
	if len(needSrc) > 0 {
		rows, err := runner.FetchFullRows(ctx, tp, plan.SideSource, needSrc)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range rows {
			in.SourceRows[k] = sqlgen.SourceRow{Key: v.Key, Vals: v.Vals}
		}
	}
	if len(needTgt) > 0 {
		rows, err := runner.FetchFullRows(ctx, tp, plan.SideTarget, needTgt)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range rows {
			in.TargetRows[k] = sqlgen.SourceRow{Key: v.Key, Vals: v.Vals}
		}
	}
	// Modified rows also need the current target row for the undo.
	var needTgtUndo []model.Diff
	for _, d := range selected {
		if d.Kind == model.KindModified {
			needTgtUndo = append(needTgtUndo, d)
		}
	}
	if len(needTgtUndo) > 0 {
		rows, err := runner.FetchFullRows(ctx, tp, plan.SideTarget, needTgtUndo)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range rows {
			in.TargetRows[k] = sqlgen.SourceRow{Key: v.Key, Vals: v.Vals}
		}
	}
	batch, err := sqlgen.Generate(tp, tgt.Dial, selected, in, opts)
	if err != nil {
		return nil, nil, err
	}
	rep := &model.RepairBatch{
		TaskID: taskID, TableName: req.Table, Kinds: batch.Kinds, Statements: batch.Statements,
		SQL: batch.SQL, UndoSQL: batch.UndoSQL, Status: "preview",
	}
	warn := append([]string{}, batch.Warnings...)
	if skipped > 0 {
		warn = append(warn, fmt.Sprintf("%d difference(s) skipped by the selected filters", skipped))
	}
	return rep, warn, nil
}

// withoutDigest returns a copy of the table spec that reads full column
// values instead of server side digests.
func withoutDigest(spec model.TableSpec) model.TableSpec {
	out := spec
	out.Columns = append([]model.ColumnSpec(nil), spec.Columns...)
	for i := range out.Columns {
		out.Columns[i].Digest = false
	}
	return out
}

func defaultTimeoutMS(o model.Options) int {
	if o.StatementTimeoutMS > 0 {
		return o.StatementTimeoutMS
	}
	return model.DefaultOptions().StatementTimeoutMS
}

func findSpec(cfg *model.Config, table string) (model.TableSpec, bool) {
	for _, t := range cfg.Tables {
		if strings.EqualFold(t.Name, table) {
			return t, true
		}
	}
	return model.TableSpec{}, false
}

// executeRepair runs a stored repair batch against the target database.
func (s *Server) executeRepair(w http.ResponseWriter, r *http.Request, repairID int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	rep, err := s.Store.GetRepair(ctx, repairID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "repair not found")
		return
	}
	if rep.Status == "executed" {
		writeErr(w, http.StatusConflict, "repair has already been executed")
		return
	}
	// Require an explicit confirmation token: execution writes to the target.
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Confirm != "apply" {
		writeErr(w, http.StatusBadRequest, "send {\"confirm\":\"apply\"} to execute the repair")
		return
	}
	if strings.TrimSpace(rep.SQL) == "" {
		writeErr(w, http.StatusBadRequest, "repair batch has no statements")
		return
	}
	if err := s.Store.UpdateRepairStatus(ctx, repairID, "executing", ""); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	task, err := s.Store.GetTask(ctx, rep.TaskID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "task not found")
		return
	}
	cfg, err := app.ParseConfig(task.Config)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	affected, err := executeSQLBatch(ctx, task.TargetDriver, task.TargetDSN, cfg.Target.Charset, rep.SQL)
	if err != nil {
		_ = s.Store.UpdateRepairStatus(context.WithoutCancel(ctx), repairID, "failed", err.Error())
		writeErr(w, http.StatusConflict, fmt.Sprintf("execution failed after applying part of the batch: %v", err))
		return
	}
	msg := fmt.Sprintf("applied %d statement(s)", affected)
	_ = s.Store.UpdateRepairStatus(ctx, repairID, "executed", msg)
	writeJSON(w, http.StatusOK, map[string]any{"status": "executed", "statements_applied": affected})
}

// executeSQLBatch runs statements in a single transaction.
func executeSQLBatch(ctx context.Context, driver model.Driver, dsn, charset, sqlText string) (int, error) {
	db, err := source.Open(ctx, source.Config{Driver: driver, DSN: dsn, Charset: charset, MaxConns: 2})
	if err != nil {
		return 0, err
	}
	defer db.Close()
	stmts := splitStatements(sqlText)
	if len(stmts) == 0 {
		return 0, fmt.Errorf("no statements")
	}
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	applied := 0
	for i, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return applied, fmt.Errorf("statement %d: %w", i+1, err)
		}
		applied++
	}
	if err := tx.Commit(); err != nil {
		return applied, err
	}
	return applied, nil
}

// splitStatements splits a generated batch on statement terminating semicolons.
// Generated literals escape quotes by doubling them, so a bare ';' at the end
// of a line is a safe separator for the SQL this package produces.
func splitStatements(text string) []string {
	var out []string
	var cur strings.Builder
	inString := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\'':
			cur.WriteByte(c)
			if inString {
				if i+1 < len(text) && text[i+1] == '\'' {
					cur.WriteByte(text[i+1])
					i++
					continue
				}
				inString = false
			} else {
				inString = true
			}
		case c == ';' && !inString:
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

var _ = json.Marshal
var _ = strconv.Itoa
