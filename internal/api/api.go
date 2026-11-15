// Package api exposes the task board over HTTP.
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
)

// Server serves the dashboard and its JSON API.
type Server struct {
	App   *app.App
	Store *journal.Store
}

// Handler builds the HTTP mux.
func (s *Server) Handler(static http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.health)
	mux.HandleFunc("/api/connections", s.connections)
	mux.HandleFunc("/api/connections/", s.connectionByID)
	mux.HandleFunc("/api/connections/test", s.testConnection)
	mux.HandleFunc("/api/introspect", s.introspect)
	mux.HandleFunc("/api/tasks", s.tasks)
	mux.HandleFunc("/api/tasks/", s.taskByID)
	mux.HandleFunc("/api/repairs/", s.repairByID)
	if static != nil {
		mux.Handle("/", static)
	}
	return withCommonHeaders(mux)
}

func withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		// Only same origin requests may mutate state; the dashboard is served
		// from this same server.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin := r.Header.Get("Origin")
			if origin != "" && !sameHost(origin, r.Host) {
				writeErr(w, http.StatusForbidden, "cross origin request rejected")
				return
			}
			if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
				writeErr(w, http.StatusUnsupportedMediaType, "expected application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sameHost(origin, host string) bool {
	o := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
	return strings.EqualFold(o, host)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.App.Version,
		"running": s.App.RunningCount(),
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func pathID(r *http.Request, prefix string) (int64, string, error) {
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return 0, "", fmt.Errorf("missing id")
	}
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("invalid id %q", parts[0])
	}
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	return id, sub, nil
}

// connPayload is the create/update body.
type connPayload struct {
	Name    string       `json:"name"`
	Driver  model.Driver `json:"driver"`
	DSN     string       `json:"dsn"`
	Charset string       `json:"charset"`
	Note    string       `json:"note"`
}

func (s *Server) connections(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.Store.ListConnections(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]model.Connection, len(list))
		for i, c := range list {
			out[i] = c.Redacted()
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var p connPayload
		if err := decodeJSON(r, &p); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateConn(p); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		c := &model.Connection{Name: p.Name, Driver: p.Driver, DSN: p.DSN, Charset: p.Charset, Note: p.Note}
		if err := s.Store.SaveConnection(r.Context(), c); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, c.Redacted())
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func validateConn(p connPayload) error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if p.Driver != model.DriverSQLite && p.Driver != model.DriverPostgres {
		return fmt.Errorf("driver must be sqlite or postgres")
	}
	if strings.TrimSpace(p.DSN) == "" {
		return fmt.Errorf("dsn is required")
	}
	switch model.Driver(p.Driver) {
	case model.DriverSQLite:
		if strings.Contains(p.DSN, "://") {
			return fmt.Errorf("sqlite dsn should be a file path, not a URL")
		}
	}
	return nil
}

func (s *Server) connectionByID(w http.ResponseWriter, r *http.Request) {
	id, sub, err := pathID(r, "/api/connections/")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if sub == "test" {
		writeErr(w, http.StatusBadRequest, "use POST /api/connections/test")
		return
	}
	switch r.Method {
	case http.MethodGet:
		c, err := s.Store.GetConnection(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "connection not found")
			return
		}
		writeJSON(w, http.StatusOK, c.Redacted())
	case http.MethodPut:
		var p connPayload
		if err := decodeJSON(r, &p); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateConn(p); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		existing, err := s.Store.GetConnection(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "connection not found")
			return
		}
		// An empty password field means "keep the stored DSN".
		if strings.Contains(p.DSN, "****") {
			p.DSN = existing.DSN
		}
		c := &model.Connection{ID: id, Name: p.Name, Driver: p.Driver, DSN: p.DSN, Charset: p.Charset, Note: p.Note}
		if err := s.Store.SaveConnection(r.Context(), c); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, c.Redacted())
	case http.MethodDelete:
		if err := s.Store.DeleteConnection(r.Context(), id); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) testConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var p connPayload
	if err := decodeJSON(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if p.DSN == "" {
		writeErr(w, http.StatusBadRequest, "dsn is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	db, err := source.Open(ctx, source.Config{
		Driver: p.Driver, DSN: p.DSN, Charset: p.Charset, ReadOnly: true, MaxConns: 2,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer db.Close()
	info, err := db.Info(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": info.ServerVersion, "tables": info.Tables,
	})
}

// introspectRequest asks for the schema of a table on one side.
type introspectRequest struct {
	Driver  model.Driver `json:"driver"`
	DSN     string       `json:"dsn"`
	Charset string       `json:"charset"`
	Table   string       `json:"table"`
}

// ColumnInfo is the UI facing column description.
type ColumnInfo struct {
	Name     string            `json:"name"`
	DBType   string            `json:"db_type"`
	Logical  model.LogicalType `json:"logical_type"`
	Nullable bool              `json:"nullable"`
	Primary  bool              `json:"primary"`
	Length   int64             `json:"length,omitempty"`
	Prec     int32             `json:"precision,omitempty"`
	Scale    int32             `json:"scale,omitempty"`
}

func (s *Server) introspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req introspectRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Table == "" {
		writeErr(w, http.StatusBadRequest, "table is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	db, err := source.Open(ctx, source.Config{Driver: req.Driver, DSN: req.DSN, Charset: req.Charset,
		ReadOnly: true, MaxConns: 2})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	defer db.Close()
	sch, err := db.Introspect(ctx, req.Table)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]ColumnInfo, len(sch.Columns))
	for i, c := range sch.Columns {
		out[i] = ColumnInfo{Name: c.Name, DBType: c.DBType, Logical: c.Logical, Nullable: c.Nullable,
			Primary: c.IsPrimary, Length: c.CharLength, Prec: c.NumPrec, Scale: c.NumScale}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"table":       sch.Name,
		"columns":     out,
		"primary_key": sch.PrimaryKey,
	})
}

// createTaskRequest is the task creation body.
type createTaskRequest struct {
	Name     string            `json:"name"`
	SourceID int64             `json:"source_id"`
	TargetID int64             `json:"target_id"`
	Source   *connPayload      `json:"source"`
	Target   *connPayload      `json:"target"`
	Options  model.Options     `json:"options"`
	Config   string            `json:"config"`
	Tables   []model.TableSpec `json:"tables"`
	Start    bool              `json:"start"`
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.Store.ListTasks(r.Context(), 200)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		safe := make([]*journal.Task, len(list))
		for i := range list {
			safe[i] = list[i].Redacted()
		}
		writeJSON(w, http.StatusOK, safe)
	case http.MethodPost:
		s.createTask(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	opts := req.Options
	if opts.ChunkRows <= 0 {
		opts.ChunkRows = model.DefaultOptions().ChunkRows
	}
	if opts.ChunkWorkers <= 0 {
		opts.ChunkWorkers = model.DefaultOptions().ChunkWorkers
	}
	if opts.Workers <= 0 {
		opts.Workers = model.DefaultOptions().Workers
	}
	if opts.SettleMS <= 0 {
		opts.SettleMS = model.DefaultOptions().SettleMS
	}
	// Resolve endpoints: saved connections first, inline payload second.
	src, err := s.resolveEndpoint(r.Context(), req.SourceID, req.Source)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "source: "+err.Error())
		return
	}
	tgt, err := s.resolveEndpoint(r.Context(), req.TargetID, req.Target)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "target: "+err.Error())
		return
	}
	cfg := model.Config{Source: *src, Target: *tgt, Options: opts, Tables: req.Tables}
	cfgText := req.Config
	if len(req.Tables) > 0 {
		b, err := yamlMarshalConfig(cfg)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		cfgText = b
	} else if strings.TrimSpace(cfgText) == "" {
		writeErr(w, http.StatusBadRequest, "provide tables or a config")
		return
	} else {
		parsed, err := app.ParseConfig(cfgText)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(parsed.Tables) == 0 {
			writeErr(w, http.StatusBadRequest, "config has no tables")
			return
		}
	}
	task := &journal.Task{
		Name:     req.Name,
		SourceID: req.SourceID, TargetID: req.TargetID,
		SourceDriver: src.Driver, SourceDSN: src.DSN,
		TargetDriver: tgt.Driver, TargetDSN: tgt.DSN,
		Options: opts, Config: cfgText,
	}
	if err := s.Store.CreateTask(r.Context(), task); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Start {
		s.App.Enqueue(task.ID)
	}
	writeJSON(w, http.StatusCreated, task.Redacted())
}

func (s *Server) resolveEndpoint(ctx context.Context, id int64, inline *connPayload) (*model.Endpoint, error) {
	if id > 0 {
		c, err := s.Store.GetConnection(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("connection %d not found", id)
		}
		return &model.Endpoint{Driver: c.Driver, DSN: c.DSN, Charset: c.Charset}, nil
	}
	if inline == nil {
		return nil, fmt.Errorf("no connection given")
	}
	if err := validateConn(*inline); err != nil {
		return nil, err
	}
	return &model.Endpoint{Driver: inline.Driver, DSN: inline.DSN, Charset: inline.Charset}, nil
}

func (s *Server) taskByID(w http.ResponseWriter, r *http.Request) {
	id, sub, err := pathID(r, "/api/tasks/")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		s.getTask(w, r, id)
	case sub == "" && r.Method == http.MethodDelete:
		_ = s.Store.DeleteTask(r.Context(), id)
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	case sub == "start" && r.Method == http.MethodPost:
		s.startTask(w, r, id)
	case sub == "cancel" && r.Method == http.MethodPost:
		if !s.App.Cancel(id) {
			// A queued task has not started; mark it canceled directly.
			_ = s.Store.UpdateTaskStatus(r.Context(), id, model.TaskCanceled, "canceled before start")
		}
		writeJSON(w, http.StatusOK, map[string]bool{"canceled": true})
	case sub == "tables":
		s.taskTables(w, r, id)
	case sub == "tables/":
		s.tableDetail(w, r, id)
	case strings.HasPrefix(sub, "tables/"):
		s.tableDetail(w, r, id)
	case sub == "diffs":
		s.taskDiffs(w, r, id)
	case sub == "events":
		s.taskEvents(w, r, id)
	case sub == "report":
		s.taskReport(w, r, id)
	case sub == "report.csv":
		s.taskReportCSV(w, r, id)
	case sub == "repairs":
		if r.Method == http.MethodPost {
			s.createRepair(w, r, id)
		} else {
			s.taskRepairs(w, r, id)
		}
	case strings.HasPrefix(sub, "repairs/"):
		s.repairByID(w, r)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) startTask(w http.ResponseWriter, r *http.Request, id int64) {
	if _, err := s.Store.GetTask(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if s.App.IsRunning(id) {
		writeErr(w, http.StatusConflict, "task is already running")
		return
	}
	s.App.Enqueue(id)
	writeJSON(w, http.StatusOK, map[string]bool{"queued": true})
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request, id int64) {
	t, err := s.Store.GetTask(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	tables, _ := s.Store.ListTableRuns(r.Context(), id)
	counts, _ := s.Store.DiffCounts(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{
		"task":        t.Redacted(),
		"tables":      tables,
		"diff_counts": counts,
		"running":     s.App.IsRunning(id),
	})
}

func (s *Server) taskTables(w http.ResponseWriter, r *http.Request, id int64) {
	tables, err := s.Store.ListTableRuns(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tables)
}

func (s *Server) tableDetail(w http.ResponseWriter, r *http.Request, taskID int64) {
	name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), fmt.Sprint(taskID)+"/tables/")
	name = strings.Trim(name, "/")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "table name required")
		return
	}
	chunks, err := s.Store.TableRunChunks(r.Context(), taskID, name)
	if err != nil {
		writeErr(w, http.StatusNotFound, "table run not found")
		return
	}
	tables, _ := s.Store.ListTableRuns(r.Context(), taskID)
	var run *model.TableRun
	for i := range tables {
		if tables[i].TableName == name {
			run = &tables[i]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"table": run, "chunks": chunks})
}

func (s *Server) taskDiffs(w http.ResponseWriter, r *http.Request, id int64) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	diffs, total, err := s.Store.ListDiffs(r.Context(), journal.DiffQuery{
		TaskID: id, Table: q.Get("table"), Kind: q.Get("kind"),
		Stability: q.Get("stability"), Search: q.Get("q"), Limit: limit, Offset: offset,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"diffs": diffs, "total": total, "limit": limit, "offset": offset})
}

func (s *Server) taskEvents(w http.ResponseWriter, r *http.Request, id int64) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	events, err := s.Store.ListEvents(r.Context(), id, after, 500)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) taskReport(w http.ResponseWriter, r *http.Request, id int64) {
	rep, err := s.buildReport(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) taskRepairs(w http.ResponseWriter, r *http.Request, id int64) {
	list, err := s.Store.ListRepairs(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) repairByID(w http.ResponseWriter, r *http.Request) {
	// /api/repairs/<id> or /api/repairs/<id>/execute
	rest := strings.TrimPrefix(r.URL.Path, "/api/repairs/")
	parts := strings.SplitN(strings.Trim(rest, "/"), "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid repair id")
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		rep, err := s.Store.GetRepair(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "repair not found")
			return
		}
		writeJSON(w, http.StatusOK, rep)
	case action == "execute" && r.Method == http.MethodPost:
		s.executeRepair(w, r, id)
	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

var _ = engine.NewLimiter
var _ = plan.SideSource
