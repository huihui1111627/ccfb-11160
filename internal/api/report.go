package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"bankcmp/internal/model"
)

// yamlMarshalConfig renders a config for storage in the journal.
func yamlMarshalConfig(cfg model.Config) (string, error) {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// BuildReport assembles the comparison report.
func (s *Server) buildReport(ctx context.Context, taskID int64) (*model.Report, error) {
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("task not found")
	}
	tables, err := s.Store.ListTableRuns(ctx, taskID)
	if err != nil {
		return nil, err
	}
	counts, _ := s.Store.DiffCounts(ctx, taskID)
	rep := &model.Report{
		TaskID: task.ID, TaskName: task.Name,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		SourceDSN:   model.RedactDSN(task.SourceDSN),
		TargetDSN:   model.RedactDSN(task.TargetDSN),
		Options:     task.Options,
		Tables:      tables,
		DurationMS:  task.DurationMS,
	}
	rep.TotalTables = len(tables)
	for _, t := range tables {
		rep.SourceRows += t.SourceRows
		rep.TargetRows += t.TargetRows
		rep.TotalDiffs += t.Diffs
		rep.Missing += t.Missing
		rep.Extra += t.Extra
		rep.Modified += t.Modified
		rep.Stable += t.Stable
		rep.Transient += t.Transient
		switch t.Status {
		case model.TableEqual:
			rep.EqualTables++
		case model.TableDiff:
			rep.DiffTables++
		case model.TableFailed:
			rep.FailedTables++
		}
		if t.SourceChanged || t.TargetChanged {
			rep.TablesWithSourceWrites++
		}
	}
	for k, v := range counts {
		_ = k
		_ = v
	}
	return rep, nil
}

func (s *Server) taskReportCSV(w http.ResponseWriter, r *http.Request, id int64) {
	rep, err := s.buildReport(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", "compare-report-"+strconv.FormatInt(id, 10)+".csv"))
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{"table", "status", "source_rows", "target_rows", "diffs", "missing", "extra",
		"modified", "stable", "transient", "chunks", "duration_ms", "error"})
	for _, t := range rep.Tables {
		_ = cw.Write([]string{
			t.TableName, string(t.Status), i64(t.SourceRows), i64(t.TargetRows), i64(t.Diffs),
			i64(t.Missing), i64(t.Extra), i64(t.Modified), i64(t.Stable), i64(t.Transient),
			strconv.Itoa(t.ChunksTotal), i64(t.DurationMS), t.Error,
		})
	}
}

func i64(v int64) string { return strconv.FormatInt(v, 10) }
