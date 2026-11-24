package engine

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	_ "modernc.org/sqlite"

	"terrainnormalverify/internal/checkpoint"
	"terrainnormalverify/internal/model"
	"terrainnormalverify/internal/sample"
)

func TestIncrementalInvalidationAndFullEquality(t *testing.T) {
	project := sample.Generate(sample.DefaultSeed)
	runner, _, _ := newTestEngine(t, project, DefaultBudget())
	initial, err := runner.Initialize()
	if err != nil {
		t.Fatal(err)
	}
	if initial.Rebaked != 6 || initial.CacheHits != 0 {
		t.Fatalf("initial bake counts: rebaked=%d cacheHits=%d", initial.Rebaked, initial.CacheHits)
	}
	counts := reportCounts(initial)

	report, err := runner.ApplyEdits([]Edit{{Kind: "height_delta", BlockID: 5, X: 4, Y: 4, Value: 0.1}})
	if err != nil {
		t.Fatal(err)
	}
	assertAffected(t, report, []int{5})
	if report.Rebaked != 1 {
		t.Fatalf("isolated edit rebaked %d blocks", report.Rebaked)
	}
	for id, count := range reportCounts(report) {
		if id != 5 && count != counts[id] {
			t.Fatalf("unaffected block %d rebaked from %d to %d", id, counts[id], count)
		}
	}

	report, err = runner.ApplyEdits([]Edit{{
		Kind: "layer_bias", LayerID: "grass", BiasX: -0.21, BiasY: 0.13, BiasZ: 0, Strength: 1.12,
	}})
	if err != nil {
		t.Fatal(err)
	}
	assertAffected(t, report, []int{1, 2, 3, 4, 5})
	if report.Rebaked != 5 {
		t.Fatalf("layer edit rebaked %d blocks", report.Rebaked)
	}

	report, err = runner.ApplyEdits([]Edit{{Kind: "height_delta", BlockID: 2, X: 8, Y: 8, Value: 0.07}})
	if err != nil {
		t.Fatal(err)
	}
	assertAffected(t, report, []int{1, 2, 3, 4})

	report, err = runner.ApplyEdits([]Edit{{Kind: "difference_operator", Operator: "sobel"}})
	if err != nil {
		t.Fatal(err)
	}
	assertAffected(t, report, []int{1, 2, 3, 4, 5, 6})
	incrementalSnapshot := runner.Snapshot()
	finalProject := runner.Project()

	fullRunner, _, _ := newTestEngine(t, finalProject, DefaultBudget())
	if _, err := fullRunner.BakeAll("test full reference"); err != nil {
		t.Fatal(err)
	}
	if err := assertSnapshotsEqual(fullRunner.Snapshot(), incrementalSnapshot); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fullRunner.Snapshot().SnapshotOrder, incrementalSnapshot.SnapshotOrder) {
		t.Fatal("snapshot submission order differs")
	}
}

func TestSeamFailureReturnsCodeAndDoesNotCommit(t *testing.T) {
	project := sample.Generate(sample.DefaultSeed)
	for i := range project.Seams {
		if project.Seams[i].ID == "s2-basin-terrace" {
			project.Seams[i].MaxIterations = 0
		}
	}
	runner, store, _ := newTestEngine(t, project, DefaultBudget())
	report, err := runner.BakeAll("seam failure test")
	if err == nil {
		t.Fatal("expected E_SEAM")
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.ErrSeam {
		t.Fatalf("expected E_SEAM, got %v", err)
	}
	values, ok := typed.Value.(map[string]any)
	if !ok || values["seamId"] != "s2-basin-terrace" || values["blockA"] == nil || values["blockB"] == nil {
		t.Fatalf("E_SEAM missing block pair details: %+v", typed.Value)
	}
	count, countErr := store.CountBlocks()
	if countErr != nil {
		t.Fatal(countErr)
	}
	if count != 0 {
		t.Fatalf("seam failure committed %d blocks", count)
	}
	if report == nil || report.Seams != nil {
		t.Fatal("seam failure report should retain no accepted seam stats")
	}
}

func TestVersionMismatchWipesSnapshotAndRebuilds(t *testing.T) {
	project := sample.Generate(sample.DefaultSeed)
	dbPath := filepath.Join(t.TempDir(), "version.sqlite")
	store := openTestStore(t, dbPath)
	runner, err := New(project, store, Config{Budget: DefaultBudget()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Initialize(); err != nil {
		t.Fatal(err)
	}
	baseline := runner.Snapshot()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE blocks SET algorithm_version='old-algorithm' WHERE block_id=1`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	store = openTestStore(t, dbPath)
	runner, err = New(project, store, Config{Budget: DefaultBudget()})
	if err != nil {
		t.Fatal(err)
	}
	report, err := runner.Initialize()
	if err != nil {
		t.Fatal(err)
	}
	if !report.SnapshotInvalidated || report.Rebaked != 6 {
		t.Fatalf("expected full invalidation, got invalidated=%t rebaked=%d", report.SnapshotInvalidated, report.Rebaked)
	}
	if err := assertSnapshotsEqual(baseline, runner.Snapshot()); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptNormalHashWipesWholeSnapshot(t *testing.T) {
	project := sample.Generate(sample.DefaultSeed)
	dbPath := filepath.Join(t.TempDir(), "corrupt.sqlite")
	store := openTestStore(t, dbPath)
	runner, err := New(project, store, Config{Budget: DefaultBudget()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Initialize(); err != nil {
		t.Fatal(err)
	}
	baseline := runner.Snapshot()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE blocks SET normal_hash='corrupt' WHERE block_id=2`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	store = openTestStore(t, dbPath)
	runner, err = New(project, store, Config{Budget: DefaultBudget()})
	if err != nil {
		t.Fatal(err)
	}
	report, err := runner.Initialize()
	if err != nil {
		t.Fatal(err)
	}
	if !report.SnapshotInvalidated || report.Rebaked != 6 {
		t.Fatalf("corruption should wipe snapshot, got invalidated=%t rebaked=%d", report.SnapshotInvalidated, report.Rebaked)
	}
	if err := assertSnapshotsEqual(baseline, runner.Snapshot()); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetQueuesAndNeverDropsBlocks(t *testing.T) {
	project := sample.Generate(sample.DefaultSeed)
	budget := Budget{
		MaxParallelism:       4,
		PerThreadPixelBuffer: 100,
		NormalMapMemoryBytes: 60000,
	}
	runner, _, _ := newTestEngine(t, project, budget)
	report, err := runner.Initialize()
	if err != nil {
		t.Fatal(err)
	}
	if report.Rebaked != 6 {
		t.Fatalf("budgeted run completed %d blocks", report.Rebaked)
	}
	if report.PeakMemoryBytes > budget.NormalMapMemoryBytes {
		t.Fatalf("peak %d exceeded budget %d", report.PeakMemoryBytes, budget.NormalMapMemoryBytes)
	}
	reasons := map[string]bool{}
	for _, record := range report.Queue {
		reasons[record.Reason] = true
	}
	if !reasons["normal_map_memory_budget"] {
		t.Fatalf("expected memory queue records, got %+v", report.Queue)
	}

	tiny := budget
	tiny.NormalMapMemoryBytes = 1000
	tinyRunner, tinyStore, _ := newTestEngine(t, project, tiny)
	tinyReport, err := tinyRunner.Initialize()
	if err == nil {
		t.Fatal("expected E_BUDGET when a single block exceeds memory")
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.ErrBudget {
		t.Fatalf("expected E_BUDGET, got %v", err)
	}
	if tinyReport == nil || tinyReport.Failed != 6 {
		t.Fatalf("expected six failed blocks, got %+v", tinyReport)
	}
	count, countErr := tinyStore.CountBlocks()
	if countErr != nil {
		t.Fatal(countErr)
	}
	if count != 0 {
		t.Fatalf("budget failure committed %d blocks", count)
	}
}

func newTestEngine(t *testing.T, project *model.Project, budget Budget) (*Engine, *checkpoint.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "engine.sqlite")
	store := openTestStore(t, dbPath)
	runner, err := New(project, store, Config{Budget: budget})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	return runner, store, dbPath
}

func openTestStore(t *testing.T, path string) *checkpoint.Store {
	t.Helper()
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func assertAffected(t *testing.T, report *RunReport, want []int) {
	t.Helper()
	got := append([]int(nil), report.Affected...)
	sort.Ints(got)
	want = append([]int(nil), want...)
	sort.Ints(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("affected=%v want=%v", got, want)
	}
}

func reportCounts(report *RunReport) map[int]int {
	out := make(map[int]int)
	for _, stat := range report.Blocks {
		out[stat.BlockID] = stat.BakeCount
	}
	return out
}

func assertSnapshotsEqual(a, b Snapshot) error {
	for id, result := range a.Blocks {
		other, ok := b.Blocks[id]
		if !ok {
			return errors.New("missing block in comparison snapshot")
		}
		if model.HashNormals(result.Normals) != model.HashNormals(other.Normals) {
			return errors.New("normal map mismatch")
		}
		if model.HashFloat64(result.Slopes) != model.HashFloat64(other.Slopes) {
			return errors.New("slope map mismatch")
		}
		if !reflect.DeepEqual(result.BBox, other.BBox) {
			return errors.New("bounding box mismatch")
		}
	}
	if model.HashJSON(a.Seams) != model.HashJSON(b.Seams) {
		return errors.New("seam statistics mismatch")
	}
	return nil
}
