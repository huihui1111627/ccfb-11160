package journal

import (
	"context"
	"testing"

	"bankcmp/internal/model"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/j.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTaskLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	task := &Task{Name: "t", SourceDriver: model.DriverSQLite, SourceDSN: "a",
		TargetDriver: model.DriverSQLite, TargetDSN: "b", Config: "tables: []"}
	if err := s.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTaskStatus(ctx, task.ID, model.TaskRunning, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskRunning || got.StartedAt == "" {
		t.Fatalf("status=%s started=%q", got.Status, got.StartedAt)
	}
	if err := s.UpdateTaskStatus(ctx, task.ID, model.TaskSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(ctx, task.ID)
	if got.Status != model.TaskSucceeded || got.FinishedAt == "" {
		t.Fatalf("status=%s finished=%q", got.Status, got.FinishedAt)
	}
}

func TestTableRunUpsertKeepsId(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	task := &Task{Name: "t", SourceDriver: model.DriverSQLite, SourceDSN: "a",
		TargetDriver: model.DriverSQLite, TargetDSN: "b"}
	_ = s.CreateTask(ctx, task)

	tr := &model.TableRun{TaskID: task.ID, TableName: "x", Status: model.TableRunning}
	id1, err := s.UpsertTableRun(ctx, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	tr2 := &model.TableRun{TaskID: task.ID, TableName: "x", Status: model.TableDiff, Diffs: 5}
	id2, err := s.UpsertTableRun(ctx, tr2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 || id1 == 0 {
		t.Fatalf("upsert ids differ: %d vs %d", id1, id2)
	}
	// Diffs must attach to a live table run (foreign key).
	diff := model.Diff{TableName: "x", Kind: model.KindMissing, Stability: model.StabilityStable, PK: "1"}
	if err := s.InsertDiffs(ctx, task.ID, id2, []model.Diff{diff}); err != nil {
		t.Fatalf("insert diffs: %v", err)
	}
	diffs, total, err := s.ListDiffs(ctx, DiffQuery{TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(diffs) != 1 {
		t.Fatalf("total=%d len=%d", total, len(diffs))
	}
}

func TestDiffFilters(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	task := &Task{Name: "t", SourceDriver: model.DriverSQLite, SourceDSN: "a",
		TargetDriver: model.DriverSQLite, TargetDSN: "b"}
	_ = s.CreateTask(ctx, task)
	tr := &model.TableRun{TaskID: task.ID, TableName: "x", Status: model.TableDiff}
	id, _ := s.UpsertTableRun(ctx, tr, nil)
	diffs := []model.Diff{
		{TableName: "x", Kind: model.KindMissing, Stability: model.StabilityStable, PK: "1"},
		{TableName: "x", Kind: model.KindExtra, Stability: model.StabilityStable, PK: "2"},
		{TableName: "x", Kind: model.KindModified, Stability: model.StabilityTransient, PK: "3"},
	}
	if err := s.InsertDiffs(ctx, task.ID, id, diffs); err != nil {
		t.Fatal(err)
	}
	byKind, total, _ := s.ListDiffs(ctx, DiffQuery{TaskID: task.ID, Kind: "extra"})
	if total != 1 || len(byKind) != 1 || byKind[0].PK != "2" {
		t.Fatalf("kind filter wrong: %+v total=%d", byKind, total)
	}
	byStab, total, _ := s.ListDiffs(ctx, DiffQuery{TaskID: task.ID, Stability: "transient"})
	if total != 1 || len(byStab) != 1 || byStab[0].PK != "3" {
		t.Fatalf("stability filter wrong: %+v total=%d", byStab, total)
	}
	search, total, _ := s.ListDiffs(ctx, DiffQuery{TaskID: task.ID, Search: "3"})
	if total != 1 || len(search) != 1 {
		t.Fatalf("search filter wrong: %+v total=%d", search, total)
	}
}
