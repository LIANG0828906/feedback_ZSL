package bake_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"terrain-bake/bake"
	"terrain-bake/internal/testutil"
	"terrain-bake/project"
)

type managerFixture struct {
	DataDir  string
	CacheDir string
	OutDir   string
	Project  *project.Project
	Graph    *bake.Graph
	Manager  *bake.Manager
	Options  bake.ManagerOptions
}

func newManagerFixture(t *testing.T, workers int) *managerFixture {
	t.Helper()
	dataDir := testutil.CopyProject(t)
	p, e := project.Load(dataDir)
	if e != nil {
		t.Fatal(e)
	}
	g, e := bake.NewGraph(p)
	if e != nil {
		t.Fatal(e)
	}
	base := t.TempDir()
	params := bake.ParamsFromManifest(p.Manifest)
	if workers > 0 {
		params.MaxWorkers = workers
	}
	opts := bake.ManagerOptions{
		CacheDir:  filepath.Join(base, "cache"),
		OutputDir: filepath.Join(base, "build"),
		Params:    params,
	}
	m, e := bake.NewManager(p, g, opts)
	if e != nil {
		t.Fatal(e)
	}
	return &managerFixture{
		DataDir: dataDir, CacheDir: opts.CacheDir, OutDir: opts.OutputDir,
		Project: p, Graph: g, Manager: m, Options: opts,
	}
}

func reloadManager(t *testing.T, dataDir string, workers int, cacheDir, outDir string) (*project.Project, *bake.Graph, *bake.Manager) {
	t.Helper()
	p, e := project.Load(dataDir)
	if e != nil {
		t.Fatal(e)
	}
	g, e := bake.NewGraph(p)
	if e != nil {
		t.Fatal(e)
	}
	params := bake.ParamsFromManifest(p.Manifest)
	if workers > 0 {
		params.MaxWorkers = workers
	}
	m, e := bake.NewManager(p, g, bake.ManagerOptions{CacheDir: cacheDir, OutputDir: outDir, Params: params})
	if e != nil {
		t.Fatal(e)
	}
	return p, g, m
}

func mustBakeAll(t *testing.T, m *bake.Manager) *bake.BakeReport {
	t.Helper()
	r, e := m.BakeAll(context.Background(), false)
	if e != nil {
		t.Fatalf("bake all: %v", e)
	}
	return r
}

func artifactBytes(t *testing.T, m *bake.Manager, ids []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		a := m.Artifact(id)
		if a == nil {
			t.Fatalf("missing artifact %s", id)
		}
		out[id] = string(a.CanonicalBytes())
	}
	return out
}

func TestInitialBakeAndSnapshotRestart(t *testing.T) {
	f := newManagerFixture(t, 4)
	report := mustBakeAll(t, f.Manager)
	if report.Generation != 1 || len(report.Committed) != 6 || len(report.Failures) != 0 {
		t.Fatalf("unexpected initial report: %+v", report)
	}
	initial := artifactBytes(t, f.Manager, f.Graph.OrderAll())
	status := f.Manager.Status()
	if !status.CacheValid || status.ArtifactCount != 6 {
		t.Fatalf("snapshot not valid after bake: %+v", status)
	}

	_, _, m2 := reloadManager(t, f.DataDir, 2, f.CacheDir, f.OutDir)
	status2 := m2.Status()
	if !status2.CacheValid || status2.ArtifactCount != 6 {
		t.Fatalf("restart did not restore snapshot: %+v", status2)
	}
	for _, st := range status2.States {
		if !st.CacheHit || st.Status != "cached" {
			t.Fatalf("block %s not restored from cache: %+v", st.BlockID, st)
		}
	}
	report2 := mustBakeAll(t, m2)
	if len(report2.Affected) != 0 || len(report2.CacheHits) != 6 {
		t.Fatalf("restart rebaked unexpectedly: %+v", report2)
	}
	restored := artifactBytes(t, m2, m2.SortedBlockIDs())
	if !reflect.DeepEqual(initial, restored) {
		t.Fatal("restored artifacts differ from committed artifacts")
	}
}

func TestIncrementalPreciseInvalidationAndFullByteEquality(t *testing.T) {
	f := newManagerFixture(t, 4)
	mustBakeAll(t, f.Manager)
	beforeB3 := artifactBytes(t, f.Manager, []string{"B_3_0", "B_3_1"})

	report, e := f.Manager.EditHeight(context.Background(), "B_0_0", 10, 10, 2.25)
	if e != nil {
		t.Fatalf("edit height: %v", e)
	}
	if !reflect.DeepEqual(report.Affected, []string{"B_0_0", "B_1_0", "B_0_1", "B_1_1"}) {
		t.Fatalf("affected = %v", report.Affected)
	}
	if !reflect.DeepEqual(report.CacheHits, []string{"B_3_0", "B_3_1"}) {
		t.Fatalf("cache hits = %v", report.CacheHits)
	}
	if !reflect.DeepEqual(report.CommitOrder, report.Affected) {
		t.Fatalf("commit order = %v, affected = %v", report.CommitOrder, report.Affected)
	}
	afterB3 := artifactBytes(t, f.Manager, []string{"B_3_0", "B_3_1"})
	if !reflect.DeepEqual(beforeB3, afterB3) {
		t.Fatal("untouched island artifacts were rewritten")
	}
	for _, st := range f.Manager.Status().States {
		if st.BlockID == "B_3_0" || st.BlockID == "B_3_1" {
			if !st.CacheHit || st.Status != "cached" {
				t.Fatalf("untouched block did not report cache hit: %+v", st)
			}
		}
	}
	diff, e := f.Manager.CompareToFull(context.Background())
	if e != nil {
		t.Fatalf("compare after incremental: %v", e)
	}
	if !diff.Equal || diff.Compared != 6 {
		t.Fatalf("incremental/full comparison: %+v", diff)
	}

	report2, e := f.Manager.EditMaterial(context.Background(), "B_1_1", 7, 9, 1, 7000)
	if e != nil {
		t.Fatalf("edit material: %v", e)
	}
	if len(report2.Failures) != 0 {
		t.Fatalf("material edit failed: %+v", report2.Failures)
	}
	diff, e = f.Manager.CompareToFull(context.Background())
	if e != nil || !diff.Equal {
		t.Fatalf("compare after material edit: diff=%+v err=%v", diff, e)
	}
}

func TestEditThenRestartRestoresCurrentSnapshot(t *testing.T) {
	f := newManagerFixture(t, 4)
	mustBakeAll(t, f.Manager)
	if _, e := f.Manager.EditHeight(context.Background(), "B_3_1", 9, 8, 1.75); e != nil {
		t.Fatal(e)
	}
	want := artifactBytes(t, f.Manager, f.Manager.SortedBlockIDs())
	wantStatus := f.Manager.Status()
	_, _, m2 := reloadManager(t, f.DataDir, 4, f.CacheDir, f.OutDir)
	status := m2.Status()
	if !status.CacheValid || status.Generation != wantStatus.Generation {
		t.Fatalf("edited snapshot not restored: %+v", status)
	}
	if !status.TruthStale {
		t.Fatal("truth should be marked stale after persisted edit")
	}
	got := artifactBytes(t, m2, m2.SortedBlockIDs())
	if !reflect.DeepEqual(got, want) {
		t.Fatal("artifacts after restart differ from committed incremental artifacts")
	}
}

func TestGlobalHashMismatchInvalidatesWholeSnapshot(t *testing.T) {
	f := newManagerFixture(t, 4)
	mustBakeAll(t, f.Manager)
	path := filepath.Join(f.DataDir, "blocks", "B_0_0", "height.f64")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0x01
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, m2 := reloadManager(t, f.DataDir, 4, f.CacheDir, f.OutDir)
	status := m2.Status()
	if status.CacheValid || status.ArtifactCount != 0 {
		t.Fatalf("snapshot was partially reused after content mismatch: %+v", status)
	}
	if status.CacheReason != "global content hash mismatch" {
		t.Fatalf("unexpected cache reason: %s", status.CacheReason)
	}
	report := mustBakeAll(t, m2)
	if len(report.Affected) != 6 || len(report.CacheHits) != 0 {
		t.Fatalf("expected whole-project full bake, got %+v", report)
	}
}

func TestParameterVersionMismatchInvalidatesWholeSnapshot(t *testing.T) {
	f := newManagerFixture(t, 4)
	mustBakeAll(t, f.Manager)
	p, e := project.Load(f.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	g, e := bake.NewGraph(p)
	if e != nil {
		t.Fatal(e)
	}
	params := bake.ParamsFromManifest(p.Manifest)
	params.ParamsVersion = "terrain-bake-v2"
	m, e := bake.NewManager(p, g, bake.ManagerOptions{CacheDir: f.CacheDir, OutputDir: f.OutDir, Params: params})
	if e != nil {
		t.Fatal(e)
	}
	status := m.Status()
	if status.CacheValid || status.ArtifactCount != 0 {
		t.Fatalf("snapshot reused across parameter version: %+v", status)
	}
}

func TestCorruptSnapshotInvalidatesAllEntries(t *testing.T) {
	f := newManagerFixture(t, 4)
	mustBakeAll(t, f.Manager)
	if err := os.WriteFile(filepath.Join(f.CacheDir, "snapshot.json"), []byte(`{"schemaVersion":1,`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, m2 := reloadManager(t, f.DataDir, 4, f.CacheDir, f.OutDir)
	status := m2.Status()
	if status.CacheValid || status.ArtifactCount != 0 {
		t.Fatalf("corrupt snapshot should be ignored wholesale: %+v", status)
	}
}

func TestBlockFailureIsIsolatedAndRetryable(t *testing.T) {
	dataDir := testutil.CopyProject(t)
	p, e := project.Load(dataDir)
	if e != nil {
		t.Fatal(e)
	}
	g, e := bake.NewGraph(p)
	if e != nil {
		t.Fatal(e)
	}
	base := t.TempDir()
	params := bake.ParamsFromManifest(p.Manifest)
	var fired atomic.Bool
	m, e := bake.NewManager(p, g, bake.ManagerOptions{
		CacheDir:  filepath.Join(base, "cache"),
		OutputDir: filepath.Join(base, "build"),
		Params:    params,
		FailureInjector: func(blockID string) error {
			if blockID == "B_0_0" && fired.CompareAndSwap(false, true) {
				panic("synthetic block panic")
			}
			return nil
		},
	})
	if e != nil {
		t.Fatal(e)
	}
	report, e := m.BakeAll(context.Background(), false)
	if e == nil || e.Code != project.CodeBake {
		t.Fatalf("expected injected E_BAKE, got %v", e)
	}
	if len(report.Failures) != 1 || report.Failures[0].BlockID != "B_0_0" {
		t.Fatalf("unexpected failures: %+v", report.Failures)
	}
	if m.Artifact("B_0_0") != nil {
		t.Fatal("failed block was committed")
	}
	neighborHashes := artifactBytes(t, m, []string{"B_1_0", "B_0_1", "B_1_1"})
	for _, st := range m.Status().States {
		if st.BlockID == "B_0_0" && st.Status != "failed" {
			t.Fatalf("failed block status = %s", st.Status)
		}
	}
	retry, e := m.RetryFailed(context.Background())
	if e != nil {
		t.Fatalf("retry: %v", e)
	}
	if len(retry.Affected) != 1 || retry.Affected[0] != "B_0_0" {
		t.Fatalf("retry affected = %v", retry.Affected)
	}
	if !reflect.DeepEqual(neighborHashes, artifactBytes(t, m, []string{"B_1_0", "B_0_1", "B_1_1"})) {
		t.Fatal("retry polluted neighbor artifacts")
	}
	diff, e := m.CompareToFull(context.Background())
	if e != nil || !diff.Equal {
		t.Fatalf("post-retry diff: %+v %v", diff, e)
	}
}

func TestCancelDoesNotCommitPartialBatch(t *testing.T) {
	dataDir := testutil.CopyProject(t)
	p, e := project.Load(dataDir)
	if e != nil {
		t.Fatal(e)
	}
	g, e := bake.NewGraph(p)
	if e != nil {
		t.Fatal(e)
	}
	base := t.TempDir()
	params := bake.ParamsFromManifest(p.Manifest)
	m, e := bake.NewManager(p, g, bake.ManagerOptions{
		CacheDir: filepath.Join(base, "cache"), OutputDir: filepath.Join(base, "build"), Params: params,
	})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, e := m.BakeAll(ctx, false)
	if e == nil || e.Code != project.CodeCancel {
		t.Fatalf("expected E_CANCEL, got %v", e)
	}
	if !report.Canceled || len(report.Committed) != 0 {
		t.Fatalf("canceled batch committed data: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(base, "cache", "snapshot.json")); !os.IsNotExist(err) {
		t.Fatalf("cancel created a snapshot: %v", err)
	}
	_, _, m2 := reloadManager(t, dataDir, 4, filepath.Join(base, "cache"), filepath.Join(base, "build"))
	mustBakeAll(t, m2)
	if m2.Status().ArtifactCount != 6 {
		t.Fatal("restart after cancel did not produce complete artifacts")
	}
}

func TestParallelAndSerialBakesMatchCommitOrderAndBytes(t *testing.T) {
	serial := newManagerFixture(t, 1)
	parallel := newManagerFixture(t, 4)
	r1 := mustBakeAll(t, serial.Manager)
	r2 := mustBakeAll(t, parallel.Manager)
	if !reflect.DeepEqual(r1.CommitOrder, r2.CommitOrder) {
		t.Fatalf("initial commit order differs: %v / %v", r1.CommitOrder, r2.CommitOrder)
	}
	er1, e := serial.Manager.EditHeight(context.Background(), "B_0_0", 11, 8, 2.5)
	if e != nil {
		t.Fatal(e)
	}
	er2, e := parallel.Manager.EditHeight(context.Background(), "B_0_0", 11, 8, 2.5)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(er1.CommitOrder, er2.CommitOrder) || !reflect.DeepEqual(er1.Affected, er2.Affected) {
		t.Fatalf("edit commit order differs: %v / %v", er1.CommitOrder, er2.CommitOrder)
	}
	mr1, e := serial.Manager.EditMaterial(context.Background(), "B_1_1", 6, 7, 2, 6500)
	if e != nil {
		t.Fatal(e)
	}
	mr2, e := parallel.Manager.EditMaterial(context.Background(), "B_1_1", 6, 7, 2, 6500)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(mr1.CommitOrder, mr2.CommitOrder) {
		t.Fatalf("material edit commit order differs: %v / %v", mr1.CommitOrder, mr2.CommitOrder)
	}
	a := artifactBytes(t, serial.Manager, serial.Manager.SortedBlockIDs())
	b := artifactBytes(t, parallel.Manager, parallel.Manager.SortedBlockIDs())
	if !reflect.DeepEqual(a, b) {
		t.Fatal("parallel and serial final artifacts differ")
	}
	s1, s2 := serial.Manager.Status(), parallel.Manager.Status()
	if s1.CacheSnapshotHash != s2.CacheSnapshotHash {
		t.Fatalf("parallel and serial snapshots differ: %s / %s", s1.CacheSnapshotHash, s2.CacheSnapshotHash)
	}
}

func TestSnapshotEntryOrderAndDependencySets(t *testing.T) {
	f := newManagerFixture(t, 4)
	mustBakeAll(t, f.Manager)
	var snap bake.Snapshot
	data, err := os.ReadFile(filepath.Join(f.CacheDir, "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snap.CommitOrder, []string{"B_0_0", "B_1_0", "B_0_1", "B_1_1", "B_3_0", "B_3_1"}) {
		t.Fatalf("snapshot commit order = %v", snap.CommitOrder)
	}
	for _, entry := range snap.Entries {
		want := f.Graph.BlockDependencies(entry.BlockID)
		if !reflect.DeepEqual(entry.Dependencies, want) {
			t.Fatalf("dependencies for %s = %v, want %v", entry.BlockID, entry.Dependencies, want)
		}
	}
}

func TestGlobalRenderManifestAndArtifactFiles(t *testing.T) {
	f := newManagerFixture(t, 4)
	mustBakeAll(t, f.Manager)
	var manifest bake.RenderManifest
	testutil.ReadJSON(t, filepath.Join(f.OutDir, "manifest.json"), &manifest)
	if manifest.SchemaVersion != 1 || len(manifest.Blocks) != 6 {
		t.Fatalf("unexpected render manifest: %+v", manifest)
	}
	var order []string
	for _, block := range manifest.Blocks {
		order = append(order, block.BlockID)
		data, err := os.ReadFile(filepath.Join(f.OutDir, block.ArtifactFile))
		if err != nil {
			t.Fatalf("missing artifact file for %s: %v", block.BlockID, err)
		}
		if project.HashBytes(data) != block.ArtifactSHA256 {
			t.Fatalf("artifact file hash mismatch for %s", block.BlockID)
		}
	}
	if !reflect.DeepEqual(order, f.Graph.OrderAll()) {
		t.Fatalf("render manifest order = %v", order)
	}
}

func TestCompareToFullReturnsDiffWithFirstBlockAndType(t *testing.T) {
	f := newManagerFixture(t, 4)
	mustBakeAll(t, f.Manager)
	a := f.Manager.Artifact("B_0_0")
	a.Normals[0].X += 0.5
	diff, e := f.Manager.CompareToFull(context.Background())
	if e == nil || e.Code != project.CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", e)
	}
	if diff.FirstBlock != "B_0_0" || diff.ArtifactType != "normals" {
		t.Fatalf("wrong diff location: %+v", diff)
	}
	if e.BlockID != "B_0_0" || e.Field != "artifactType:normals" {
		t.Fatalf("wrong E_DIFF fields: %+v", e)
	}
}
