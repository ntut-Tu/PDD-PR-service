package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pdd/services/internal/sandboxcontroller/model"
	"pdd/services/internal/sandboxcontroller/store"
)

func newRuns(t *testing.T) (*store.Runs, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return store.NewRuns(db), dir
}

func sample(id string) model.Run {
	return model.Run{
		ID:   id,
		Spec: model.Spec{Account: "alice", ProjectID: 7, RepoID: 1, HeadSHA: "a", Files: []model.File{{Path: "src/test/A.java", Content: "x"}}, TestClasses: []string{"A"}},
		Status: model.Status{Phase: model.Pending, Conditions: []model.Condition{},
			CreatedAt: time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC)},
	}
}

func TestCreateGetRoundTrip(t *testing.T) {
	runs, _ := newRuns(t)
	ctx := context.Background()
	want := sample("run_1")
	if err := runs.Create(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, err := runs.Get(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Files[0].Content != "x" || got.Status.Phase != model.Pending || !got.Status.CreatedAt.Equal(want.Status.CreatedAt) || got.DeletionRequested {
		t.Errorf("got %+v", got)
	}
}

func TestGetUnknownIsNotFound(t *testing.T) {
	runs, _ := newRuns(t)
	if _, err := runs.Get(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestDuplicateIDIsRejected(t *testing.T) {
	runs, _ := newRuns(t)
	ctx := context.Background()
	_ = runs.Create(ctx, sample("run_1"))
	if err := runs.Create(ctx, sample("run_1")); err == nil {
		t.Error("duplicate accepted")
	}
}

func TestListKeepsCreationOrder(t *testing.T) {
	runs, _ := newRuns(t)
	ctx := context.Background()
	for _, id := range []string{"run_b", "run_a", "run_c"} {
		if err := runs.Create(ctx, sample(id)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := runs.List(ctx)
	if err != nil || len(list) != 3 || list[0].ID != "run_b" || list[1].ID != "run_a" || list[2].ID != "run_c" {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestUpdateStatusKeepsSpecAndDeletionFlag(t *testing.T) {
	runs, _ := newRuns(t)
	ctx := context.Background()
	_ = runs.Create(ctx, sample("run_1"))
	if err := runs.RequestDeletion(ctx, "run_1"); err != nil {
		t.Fatal(err)
	}
	status := sample("run_1").Status
	status.Phase = model.Failed
	status.Reason = model.Timeout
	status.Result = &model.Result{Output: "out", Tests: []model.TestResult{{Class: "A", Passed: 1}}}

	if err := runs.UpdateStatus(ctx, "run_1", status); err != nil {
		t.Fatal(err)
	}

	got, _ := runs.Get(ctx, "run_1")
	if got.Status.Phase != model.Failed || got.Status.Reason != model.Timeout || got.Status.Result.Tests[0].Passed != 1 ||
		!got.DeletionRequested || got.Spec.Account != "alice" {
		t.Errorf("got %+v", got)
	}
}

func TestMutationsOfUnknownRunAreNotFound(t *testing.T) {
	runs, _ := newRuns(t)
	ctx := context.Background()
	if err := runs.UpdateStatus(ctx, "x", model.Status{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateStatus = %v", err)
	}
	if err := runs.RequestDeletion(ctx, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RequestDeletion = %v", err)
	}
	if err := runs.Delete(ctx, "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Delete = %v", err)
	}
}

func TestDeleteRemovesRecord(t *testing.T) {
	runs, _ := newRuns(t)
	ctx := context.Background()
	_ = runs.Create(ctx, sample("run_1"))
	if err := runs.Delete(ctx, "run_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Get(ctx, "run_1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestRecordsSurviveReopen(t *testing.T) {
	runs, dir := newRuns(t)
	ctx := context.Background()
	_ = runs.Create(ctx, sample("run_1"))
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	list, err := store.NewRuns(db).List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if err := store.NewRuns(db).Ping(ctx); err != nil {
		t.Error(err)
	}
}

func TestCorruptRowIsReported(t *testing.T) {
	runs, dir := newRuns(t)
	ctx := context.Background()
	_ = runs.Create(ctx, sample("run_1"))
	db, _ := store.Open(ctx, dir)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE runs SET spec = 'not json'`); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Get(ctx, "run_1"); err == nil {
		t.Error("Get accepted corrupt spec")
	}
	if _, err := runs.List(ctx); err == nil {
		t.Error("List accepted corrupt spec")
	}
	_, _ = db.Exec(`UPDATE runs SET spec = '{}', status = 'x'`)
	if _, err := runs.Get(ctx, "run_1"); err == nil {
		t.Error("Get accepted corrupt status")
	}
}

func TestOpenFailsWhenDirIsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(context.Background(), filepath.Join(file, "sub")); err == nil {
		t.Error("opened a database below a file")
	}
}
