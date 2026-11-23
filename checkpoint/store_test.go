package checkpoint

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"storyworkbench/engine"
	"storyworkbench/story"
)

const samplePath = "../samples/glass-meridian-20261124.json"

func loadSample(t *testing.T) *story.Project {
	t.Helper()
	project, err := story.LoadProject(samplePath)
	if err != nil {
		t.Fatalf("load sample: %v", err)
	}
	return project
}

func openTempStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestCheckpointRestartMatchesContinuous(t *testing.T) {
	project := loadSample(t)
	continuous, err := engine.Play(project, project.Choices)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	session, err := engine.NewSession(project, project.Choices)
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := RecordSession(context.Background(), store, session)
	if err != nil {
		t.Fatalf("record session: %v", err)
	}
	if err := engine.CompareRuns(continuous, recorded); err != nil {
		t.Fatalf("recorded run diverged: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	latest, err := reopened.LoadLatest(project)
	if err != nil {
		t.Fatalf("load latest: %v", err)
	}
	if latest.State.ShotProgress != continuous.ShotCount {
		t.Fatalf("latest checkpoint has %d shots, want %d", latest.State.ShotProgress, continuous.ShotCount)
	}
	resumed, err := engine.Resume(project, latest, project.Choices)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := engine.CompareRuns(continuous, resumed); err != nil {
		t.Fatalf("restarted run diverged: %v", err)
	}
	summary, err := reopened.Summary(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ShotCount != continuous.ShotCount {
		t.Fatalf("summary shot count %d, want %d", summary.ShotCount, continuous.ShotCount)
	}
}

func TestCheckpointVersionOrHashMismatchDiscardsEverything(t *testing.T) {
	cases := []struct {
		name   string
		update string
		field  string
	}{
		{"content_hash", `UPDATE checkpoint_meta SET content_hash = 'bad-hash' WHERE project_id = ?`, "content_hash"},
		{"algorithm_version", `UPDATE checkpoint_meta SET algorithm_version = 'legacy' WHERE project_id = ?`, "algorithm_version"},
		{"schema_version", `UPDATE checkpoint_meta SET save_schema_version = 99 WHERE project_id = ?`, "schema_version"},
		{"row_algorithm", `UPDATE shot_checkpoints SET algorithm_version = 'legacy' WHERE shot_seq = 3`, "algorithm_version"},
		{"row_corrupt_json", `UPDATE shot_checkpoints SET save_json = '{' WHERE shot_seq = 3`, "save"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project := loadSample(t)
			store := openTempStore(t)
			session, err := engine.NewSession(project, project.Choices)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := RecordSession(context.Background(), store, session); err != nil {
				t.Fatal(err)
			}
			args := []any{project.ID}
			if tc.name == "row_algorithm" || tc.name == "row_corrupt_json" {
				args = nil
			}
			if _, err := store.db.Exec(tc.update, args...); err != nil {
				t.Fatalf("corrupt checkpoint: %v", err)
			}
			_, err = store.LoadLatest(project)
			if err == nil {
				t.Fatal("expected checkpoint mismatch")
			}
			appErr, ok := story.AsError(err)
			if !ok || appErr.Code != story.CodeSave {
				t.Fatalf("expected E_SAVE, got %T %v", err, err)
			}
			if appErr.Field != tc.field && !(tc.field == "save" && appErr.Field != "") {
				t.Fatalf("expected field %s, got %s (%v)", tc.field, appErr.Field, err)
			}
			if _, err := store.Summary(project.ID); !errors.Is(err, ErrNoCheckpoints) {
				t.Fatalf("expected all checkpoints discarded, got %v", err)
			}
		})
	}
}

func TestConcurrentDeterministicCommitsStayContiguousAndSerialized(t *testing.T) {
	project := loadSample(t)
	store := openTempStore(t)
	const writers = 6
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			session, err := engine.NewSession(project, project.Choices)
			if err != nil {
				errs[index] = err
				return
			}
			_, errs[index] = RecordSession(context.Background(), store, session)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d failed: %v", i, err)
		}
	}
	latest, err := store.LoadLatest(project)
	if err != nil {
		t.Fatalf("load latest: %v", err)
	}
	continuous, err := engine.Play(project, project.Choices)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := engine.Resume(project, latest, project.Choices)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.CompareRuns(continuous, resumed); err != nil {
		t.Fatalf("concurrent checkpoint run diverged: %v", err)
	}
	var rows int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM shot_checkpoints WHERE project_id = ?`, project.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != continuous.ShotCount {
		t.Fatalf("checkpoint row count %d, want %d", rows, continuous.ShotCount)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("expected error for empty path")
	}
}
