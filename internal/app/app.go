// Package app wires the comparison engine, the journal and the HTTP API into
// a running job system with a task board.
package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"bankcmp/internal/engine"
	"bankcmp/internal/journal"
	"bankcmp/internal/model"
	"bankcmp/internal/plan"
	"bankcmp/internal/source"
)

// App owns the journal and the running tasks.
type App struct {
	Store *journal.Store
	// Version is reported by the health endpoint.
	Version string

	mu      sync.Mutex
	running map[int64]*runningTask
	queue   chan int64
	limiter *engine.Limiter
	limQPS  *engine.Limiter
}

type runningTask struct {
	cancel  context.CancelFunc
	started time.Time
}

// New builds an App.  maxParallel tasks run at once.
func New(store *journal.Store, maxParallel int) *App {
	if maxParallel <= 0 {
		maxParallel = 1
	}
	a := &App{
		Store:   store,
		running: map[int64]*runningTask{},
		queue:   make(chan int64, 1024),
	}
	for i := 0; i < maxParallel; i++ {
		go a.worker()
	}
	return a
}

func (a *App) worker() {
	for id := range a.queue {
		a.runTask(id)
	}
}

// Enqueue schedules a task for execution.
func (a *App) Enqueue(id int64) { a.queue <- id }

// Cancel stops a running task.
func (a *App) Cancel(id int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if rt, ok := a.running[id]; ok {
		rt.cancel()
		return true
	}
	return false
}

// IsRunning reports whether the task is currently executing.
func (a *App) IsRunning(id int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.running[id]
	return ok
}

// RunningCount returns the number of executing tasks.
func (a *App) RunningCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.running)
}

func (a *App) runTask(id int64) {
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.running[id] = &runningTask{cancel: cancel, started: time.Now()}
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		delete(a.running, id)
		a.mu.Unlock()
	}()

	task, err := a.Store.GetTask(ctx, id)
	if err != nil {
		return
	}
	_ = a.Store.UpdateTaskStatus(ctx, id, model.TaskRunning, "")
	a.log(ctx, id, "info", fmt.Sprintf("task %q started", task.Name))

	// Options flow into the engine.
	opts := task.Options
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if opts.ChunkWorkers <= 0 {
		opts.ChunkWorkers = 1
	}
	if opts.StatementTimeoutMS <= 0 {
		opts.StatementTimeoutMS = model.DefaultOptions().StatementTimeoutMS
	}

	limSource := engine.NewLimiter(opts.RowsPerSecond, opts.QueriesPerSec)
	limTarget := engine.NewLimiter(opts.RowsPerSecond, opts.QueriesPerSec)

	// The comparison only reads: a production source is opened read only so a
	// bug in the tool can never write to it.
	src, err := source.Open(ctx, source.Config{
		Driver: task.SourceDriver, DSN: task.SourceDSN, Charset: srcCharset(task),
		Timezone: "UTC", ReadOnly: true, MaxConns: opts.ChunkWorkers*opts.Workers + 4,
		StatementTimeoutMS: opts.StatementTimeoutMS,
	})
	if err != nil {
		a.fail(ctx, id, fmt.Sprintf("open source: %v", err))
		return
	}
	defer src.Close()
	tgt, err := source.Open(ctx, source.Config{
		Driver: task.TargetDriver, DSN: task.TargetDSN, Charset: tgtCharset(task),
		Timezone: "UTC", MaxConns: opts.ChunkWorkers*opts.Workers + 4,
		StatementTimeoutMS: opts.StatementTimeoutMS,
	})
	if err != nil {
		a.fail(ctx, id, fmt.Sprintf("open target: %v", err))
		return
	}
	defer tgt.Close()

	specs, err := taskSpecs(task)
	if err != nil {
		a.fail(ctx, id, err.Error())
		return
	}

	runner := engine.NewRunner(src, tgt, engine.Options{
		ChunkWorkers:  opts.ChunkWorkers,
		ChunkRows:     opts.ChunkRows,
		DiffMaxRows:   opts.DiffMaxRows,
		LeafRows:      opts.ChunkRows,
		MaxSplitDepth: 14,
		Recheck:       opts.Recheck,
		Settle:        time.Duration(opts.SettleMS) * time.Millisecond,
		SourceLim:     limSource,
		TargetLim:     limTarget,
	})

	// Compare tables with bounded table level parallelism.
	sem := make(chan struct{}, opts.Workers)
	var wg sync.WaitGroup
	totalTables := len(specs)
	doneTables := 0
	var mu sync.Mutex
	canceled := false
	for _, spec := range specs {
		if ctx.Err() != nil {
			canceled = true
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(spec model.TableSpec) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			ok := a.runTable(ctx, runner, src, tgt, task, spec, opts)
			mu.Lock()
			doneTables++
			n := doneTables
			mu.Unlock()
			if ok {
				a.log(ctx, id, "info", fmt.Sprintf("[%d/%d] table %s done", n, totalTables, spec.Name))
			}
		}(spec)
	}
	wg.Wait()
	if canceled || ctx.Err() != nil {
		_ = a.Store.UpdateTaskStatus(context.Background(), id, model.TaskCanceled, "canceled by user")
		a.log(context.Background(), id, "warn", "task canceled")
		return
	}
	_ = a.Store.UpdateTaskStatus(ctx, id, model.TaskSucceeded, "")
	a.log(ctx, id, "info", "task finished")
}

// runTable resolves, compares and persists one table.
func (a *App) runTable(ctx context.Context, runner *engine.Runner, src, tgt *source.Database,
	task *journal.Task, spec model.TableSpec, opts model.Options) bool {

	started := time.Now().UTC().Format(time.RFC3339Nano)
	_ = a.Store.SetTableRunStatus(ctx, task.ID, spec.Name, model.TableRunning, started)

	ss, err := src.Introspect(ctx, spec.Source())
	if err != nil {
		a.tableFailed(ctx, task.ID, spec.Name, started, fmt.Sprintf("introspect source: %v", err))
		return false
	}
	ts, err := tgt.Introspect(ctx, spec.Target())
	if err != nil {
		a.tableFailed(ctx, task.ID, spec.Name, started, fmt.Sprintf("introspect target: %v", err))
		return false
	}
	tp, err := plan.Build(spec, src, tgt, ss, ts, opts)
	if err != nil {
		a.tableFailed(ctx, task.ID, spec.Name, started, err.Error())
		return false
	}
	res, err := runner.RunTable(ctx, tp)
	if err != nil && res == nil {
		a.tableFailed(ctx, task.ID, spec.Name, started, err.Error())
		return false
	}

	tr := &model.TableRun{
		TaskID: task.ID, TableName: spec.Name, Status: res.Status,
		SourceRows: res.SourceRows, TargetRows: res.TargetRows,
		ChunksTotal: topLevelChunks(res.Chunks),
		DurationMS:  res.Duration.Milliseconds(),
		Stable:      res.Stable, Transient: res.Transient,
		SourceChanged: res.SourceChanged, TargetChanged: res.TargetChanged,
		StartedAt: started, FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	for _, d := range res.Diffs {
		switch d.Kind {
		case model.KindMissing:
			tr.Missing++
		case model.KindExtra:
			tr.Extra++
		case model.KindModified:
			tr.Modified++
		}
	}
	tr.Diffs = int64(len(res.Diffs))
	tr.ChunksDone = tr.ChunksTotal
	if res.Error != "" {
		tr.Error = res.Error
	}
	id, err := a.Store.UpsertTableRun(ctx, tr, res.Chunks)
	if err != nil {
		a.log(ctx, task.ID, "error", fmt.Sprintf("persist table %s: %v", spec.Name, err))
		return false
	}
	// Replace previous diffs of this table and store the new ones.
	if err := a.Store.DeleteDiffs(ctx, task.ID, spec.Name); err != nil {
		a.log(ctx, task.ID, "error", fmt.Sprintf("clear diffs %s: %v", spec.Name, err))
	}
	if err := a.Store.InsertDiffs(ctx, task.ID, id, res.Diffs); err != nil {
		// A failure to persist differences is serious: the report would
		// understate the damage, so the table is marked failed.
		a.log(ctx, task.ID, "error", fmt.Sprintf("store diffs %s: %v", spec.Name, err))
		tr.Status = model.TableFailed
		tr.Error = "store diffs: " + err.Error()
		_, _ = a.Store.UpsertTableRun(ctx, tr, res.Chunks)
		return false
	}
	if res.SourceChanged {
		a.log(ctx, task.ID, "warn", fmt.Sprintf(
			"table %s: the source was written to during the comparison; rows marked transient changed while being compared and should be re-checked", spec.Name))
	}
	if res.Status == model.TableDiff {
		a.log(ctx, task.ID, "warn", fmt.Sprintf(
			"table %s: %d difference(s) (%d missing, %d extra, %d modified; %d stable, %d transient)",
			spec.Name, len(res.Diffs), tr.Missing, tr.Extra, tr.Modified, res.Stable, res.Transient))
	}
	return true
}

// topLevelChunks counts the coarse ranges, not the refinements, so the UI's
// chunk totals match the ranges a user configured.
func topLevelChunks(chunks []model.ChunkResult) int {
	n := 0
	for _, c := range chunks {
		if c.Depth == 0 {
			n++
		}
	}
	return n
}

func (a *App) tableFailed(ctx context.Context, taskID int64, table, started, msg string) {
	tr := &model.TableRun{TaskID: taskID, TableName: table, Status: model.TableFailed,
		Error: msg, StartedAt: started, FinishedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	_, _ = a.Store.UpsertTableRun(ctx, tr, nil)
	a.log(ctx, taskID, "error", fmt.Sprintf("table %s failed: %s", table, msg))
}

func (a *App) fail(ctx context.Context, taskID int64, msg string) {
	_ = a.Store.UpdateTaskStatus(ctx, taskID, model.TaskFailed, msg)
	a.log(ctx, taskID, "error", msg)
}

func (a *App) log(ctx context.Context, taskID int64, level, msg string) {
	// The context may already be canceled; logging must still succeed.
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = a.Store.AddEvent(c, taskID, level, msg)
}

func srcCharset(t *journal.Task) string { return taskCharset(t, true) }
func tgtCharset(t *journal.Task) string { return taskCharset(t, false) }

func taskCharset(t *journal.Task, source bool) string {
	cfg, err := ParseConfig(t.Config)
	if err != nil {
		return ""
	}
	if source {
		return cfg.Source.Charset
	}
	return cfg.Target.Charset
}
