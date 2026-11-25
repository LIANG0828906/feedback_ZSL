package acousticfield

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newTestEngine(t *testing.T, sample *Sample, dbPath string, options Options) *Engine {
	t.Helper()
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e, err := NewEngine(sample.Level, &sample.Truth, options, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEngineFullBuildAndCheckpoint(t *testing.T) {
	sample, err := Generate(101)
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	e := newTestEngine(t, sample, filepath.Join(t.TempDir(), "full.db"), opts)
	report, err := e.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Conservation.OK {
		t.Fatalf("conservation failed: %+v", report.Conservation)
	}
	if report.TruthDiff != nil {
		t.Fatalf("truth diff: %s", report.TruthDiff)
	}
	if len(report.Results) != len(sample.Level.Chunks) {
		t.Fatalf("got %d results, want %d", len(report.Results), len(sample.Level.Chunks))
	}
	if report.CacheHits != 0 || report.Recomputed != len(sample.Level.Chunks) {
		t.Fatalf("unexpected cache counts hits=%d recomputed=%d", report.CacheHits, report.Recomputed)
	}
	loaded, err := e.store.LoadResults(context.Background(), e.SnapshotID())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != len(report.Results) {
		t.Fatalf("persisted %d results, want %d", len(loaded), len(report.Results))
	}
	log, err := e.store.LoadCommitLog(context.Background(), e.SnapshotID(), report.RoundID)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != len(loaded) {
		t.Fatalf("commit log %d entries, want %d", len(log), len(loaded))
	}
	for i := range log {
		if log[i].Sequence != i {
			t.Fatalf("commit sequence[%d]=%d", i, log[i].Sequence)
		}
	}
	// External report copies cannot mutate the frozen committed result.
	wantFrozen := canonicalHash(report.Results[0].Paths)
	first := report.Results[0]
	first.Paths[0].Energy[0].Energy += 123
	internal, ok := e.ResultForChunk(first.ChunkID)
	if !ok {
		t.Fatal("missing internal result")
	}
	if got := canonicalHash(internal.Paths); got != wantFrozen {
		t.Fatal("committed chunk was mutated through a report copy")
	}
}

func TestIncrementalInvalidationAndCacheReuse(t *testing.T) {
	sample, err := Generate(202)
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	e := newTestEngine(t, sample, filepath.Join(t.TempDir(), "inc.db"), opts)
	first, err := e.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := make(map[int]string)
	for _, res := range first.Results {
		before[res.ChunkID] = resultFieldDigest(res)
	}
	invalidated, err := e.ApplyEdit(Edit{
		Kind: EditAbsorption, ChunkID: 17, BandID: "500Hz", Coefficient: 30_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(invalidated) == 0 {
		t.Fatal("edit invalidated no chunks")
	}
	if len(invalidated) == len(sample.Level.Chunks) {
		t.Fatalf("edit over-invalidated all %d chunks", len(invalidated))
	}
	second, err := e.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.CacheHits == 0 {
		t.Fatal("expected unaffected chunks to reuse cache")
	}
	if !second.Conservation.OK || second.TruthDiff != nil {
		t.Fatalf("incremental result invalid: cons=%+v diff=%v", second.Conservation, second.TruthDiff)
	}
	for _, res := range second.Results {
		if _, affected := findInvalidated(invalidated, res.ChunkID); affected {
			continue
		}
		if before[res.ChunkID] != resultFieldDigest(res) {
			t.Fatalf("unaffected chunk %d changed", res.ChunkID)
		}
	}
}

func findInvalidated(v []InvalidatedChunk, id int) (InvalidatedChunk, bool) {
	for _, item := range v {
		if item.ChunkID == id {
			return item, true
		}
	}
	return InvalidatedChunk{}, false
}

func TestResumeAfterAbortMatchesFull(t *testing.T) {
	sample, err := Generate(303)
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	dir := t.TempDir()
	fullEngine := newTestEngine(t, sample, filepath.Join(dir, "full.db"), opts)
	full, err := fullEngine.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fullDigest := digestResults(full.Results)

	resumePath := filepath.Join(dir, "resume.db")
	partialEngine := newTestEngine(t, sample, resumePath, opts)
	_, err = partialEngine.Rebuild(context.Background(), BuildOptions{AbortAfterCommits: 7})
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("expected ErrAborted, got %v", err)
	}
	store, err := OpenStore(resumePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	partial, err := store.LoadResults(context.Background(), SnapshotID(sample.Level, opts))
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 7 {
		t.Fatalf("persisted %d chunks after abort, want 7", len(partial))
	}
	resumedEngine, err := NewEngine(sample.Level, &sample.Truth, opts, store)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := resumedEngine.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(restore.Loaded) != 7 {
		t.Fatalf("restored %d chunks, want 7", len(restore.Loaded))
	}
	resumed, err := resumedEngine.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.CacheHits != 7 {
		t.Fatalf("resume cache hits=%d, want 7", resumed.CacheHits)
	}
	if !sameDigestResults(digestResults(resumed.Results), fullDigest) {
		t.Fatal("resumed result does not match one-shot full result")
	}
	if !sameSubmissions(full.Submissions, resumed.Submissions) {
		t.Fatalf("submission sequence differs:\nfull=%v\nresumed=%v", full.Submissions, resumed.Submissions)
	}
}

func digestResults(results []ChunkResult) map[int]string {
	out := make(map[int]string, len(results))
	for _, res := range results {
		out[res.ChunkID] = resultFieldDigest(res)
	}
	return out
}

func sameDigestResults(a, b map[int]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func sameSubmissions(a, b []Submission) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ChunkID != b[i].ChunkID || a[i].Sequence != b[i].Sequence {
			return false
		}
	}
	return true
}

func applyDemoEdits(t *testing.T, e *Engine) []InvalidatedChunk {
	t.Helper()
	var all []InvalidatedChunk
	steps := []Edit{
		{Kind: EditAbsorption, ChunkID: 7, BandID: "125Hz", Coefficient: 27_000},
		{Kind: EditReflector, ChunkID: 8, Reflector: &Reflector{
			ID: "c008-left", A: Point{X: 4_100_000, Y: 2_100_000},
			B: Point{X: 4_100_000, Y: 3_100_000}, Normal: Vec{X: Q16}, Material: "edited-wall",
		}},
		{Kind: EditSourceEmission, SourceID: "src-009", Emission: []BandEnergy{
			{BandID: "125Hz", Energy: 3_300_000},
			{BandID: "2000Hz", Energy: 4_300_000},
			{BandID: "500Hz", Energy: 4_100_000},
		}},
	}
	for _, step := range steps {
		inv, err := e.ApplyEdit(step)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, inv...)
	}
	return all
}

func TestThreePathsMatch(t *testing.T) {
	sample, err := Generate(404)
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	dir := t.TempDir()

	// One-shot full rebuild: edits are applied to a fresh level copy and all
	// chunks are forced through the solver.
	fullLevel := CloneLevel(sample.Level)
	fullLevel = editedLevelForFull(t, fullLevel)
	fullTruth := ComputeTruth(fullLevel, opts)
	fullSample := &Sample{Level: fullLevel, Truth: fullTruth}
	fullEngine := newTestEngine(t, fullSample, filepath.Join(dir, "full.db"), opts)
	full, err := fullEngine.Rebuild(context.Background(), BuildOptions{ForceFull: true})
	if err != nil {
		t.Fatal(err)
	}

	// Incremental path.
	incEngine := newTestEngine(t, sample, filepath.Join(dir, "inc.db"), opts)
	if _, err := incEngine.Rebuild(context.Background(), BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	applyDemoEdits(t, incEngine)
	inc, err := incEngine.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Abort/recover path.
	resumePath := filepath.Join(dir, "resume.db")
	resumeEngine := newTestEngine(t, sample, resumePath, opts)
	if _, err := resumeEngine.Rebuild(context.Background(), BuildOptions{AbortAfterCommits: 5}); !errors.Is(err, ErrAborted) {
		t.Fatalf("expected abort, got %v", err)
	}
	store, err := OpenStore(resumePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recovered, err := NewEngine(sample.Level, &sample.Truth, opts, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	applyDemoEdits(t, recovered)
	resumed, err := recovered.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}

	fullDigest := digestResults(full.Results)
	for name, run := range map[string]*RunReport{"incremental": inc, "resume": resumed} {
		if len(run.Results) != len(full.Results) {
			t.Fatalf("%s result count %d, want %d", name, len(run.Results), len(full.Results))
		}
		for id, want := range fullDigest {
			if got := resultFieldDigest(findResult(t, run.Results, id)); got != want {
				t.Fatalf("%s chunk %d differs from full rebuild", name, id)
			}
		}
		if !sameSubmissions(full.Submissions, run.Submissions) {
			t.Fatalf("%s commit sequence differs", name)
		}
		if !run.Conservation.OK || run.TruthDiff != nil {
			t.Fatalf("%s failed validation: %+v %v", name, run.Conservation, run.TruthDiff)
		}
	}
}

func TestRestartAfterEditKeepsUnaffectedCheckpointRows(t *testing.T) {
	sample, err := Generate(414)
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	dbPath := filepath.Join(t.TempDir(), "restart-edit.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(sample.Level, &sample.Truth, opts, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Rebuild(context.Background(), BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	invalidated, err := engine.ApplyEdit(Edit{Kind: EditAbsorption, ChunkID: 17, BandID: "500Hz", Coefficient: 30_000})
	if err != nil {
		t.Fatal(err)
	}
	editedLevel := engine.Level()
	editedTruth := engine.Truth()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	recovered, err := NewEngine(editedLevel, &editedTruth, opts, store2)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := recovered.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if restore.InvalidSnapshot {
		t.Fatalf("unaffected snapshot was invalidated: %s", restore.Reason)
	}
	wantLoaded := len(sample.Level.Chunks) - len(invalidated)
	if len(restore.Loaded) != wantLoaded {
		t.Fatalf("loaded %d rows, want %d unaffected rows", len(restore.Loaded), wantLoaded)
	}
	run, err := recovered.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if run.CacheHits != wantLoaded {
		t.Fatalf("cache hits=%d, want %d", run.CacheHits, wantLoaded)
	}
	if !run.Conservation.OK || run.TruthDiff != nil {
		t.Fatalf("recovered edit run invalid: %+v %v", run.Conservation, run.TruthDiff)
	}
}

func TestSingleChunkFailureIsIsolatedAndRetryable(t *testing.T) {
	sample, err := Generate(515)
	if err != nil {
		t.Fatal(err)
	}
	failureLevel := CloneLevel(sample.Level)
	c0, _ := chunkByID(failureLevel, 0)
	for i := 0; i < 10; i++ {
		x := c0.Bounds.MinX + 100_000 + int64(i)*100_000
		c0.Reflectors = append(c0.Reflectors, Reflector{
			ID:     "extra-" + string(rune('a'+i)),
			A:      Point{X: x, Y: c0.Bounds.MinY + 100_000},
			B:      Point{X: x, Y: c0.Bounds.MaxY - 100_000},
			Normal: Vec{X: Q16},
		})
	}
	truth := ComputeTruth(failureLevel, DefaultOptions())
	failing := &Sample{Level: failureLevel, Truth: truth}
	opts := DefaultOptions()
	e := newTestEngine(t, failing, filepath.Join(t.TempDir(), "isolate.db"), opts)
	report, err := e.Rebuild(context.Background(), BuildOptions{})
	if err == nil {
		t.Fatal("expected the oversized chunk path buffer to fail")
	}
	if len(report.Results) != len(failureLevel.Chunks)-1 {
		t.Fatalf("successful chunks=%d, want %d", len(report.Results), len(failureLevel.Chunks)-1)
	}
	if e.StateSnapshot().Chunks[0] != ChunkFailed {
		t.Fatalf("chunk 0 state=%s, want failed", e.StateSnapshot().Chunks[0])
	}
	for id, state := range e.StateSnapshot().Chunks {
		if id != 0 && state != ChunkCommitted {
			t.Fatalf("chunk %d state=%s, want committed", id, state)
		}
	}
	// A second rebuild retries the failed unit without poisoning others.
	report2, err := e.Rebuild(context.Background(), BuildOptions{})
	if err == nil {
		t.Fatal("retry should still fail while the bad geometry remains")
	}
	if len(report2.Results) != len(failureLevel.Chunks)-1 {
		t.Fatalf("retry lost committed chunks: %d", len(report2.Results))
	}
	if report2.CacheHits != len(failureLevel.Chunks)-1 {
		t.Fatalf("retry cache hits=%d, want %d", report2.CacheHits, len(failureLevel.Chunks)-1)
	}
}

func TestInvalidEditIsTransactional(t *testing.T) {
	sample, err := Generate(616)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(sample.Level, &sample.Truth, DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeLevel := LevelContentHash(engine.Level())
	beforeTruth := engine.Truth().Fingerprint
	_, err = engine.ApplyEdit(Edit{
		Kind: EditSourceEmission, SourceID: "src-000",
		Emission: []BandEnergy{{BandID: "125Hz", Energy: 1}},
	})
	if !IsCode(err, CodeSchema) {
		t.Fatalf("expected missing-band E_SCHEMA, got %v", err)
	}
	if after := LevelContentHash(engine.Level()); after != beforeLevel {
		t.Fatal("invalid edit mutated the level")
	}
	if after := engine.Truth().Fingerprint; after != beforeTruth {
		t.Fatal("invalid edit mutated the truth")
	}
}

func findResult(t *testing.T, results []ChunkResult, id int) ChunkResult {
	t.Helper()
	for _, r := range results {
		if r.ChunkID == id {
			return r
		}
	}
	t.Fatalf("missing result for chunk %d", id)
	return ChunkResult{}
}

func editedLevelForFull(t *testing.T, level *Level) *Level {
	t.Helper()
	c7, _ := chunkByID(level, 7)
	for i := range c7.Absorption {
		if c7.Absorption[i].BandID == "125Hz" {
			c7.Absorption[i].Coefficient = 27_000
		}
	}
	c8, _ := chunkByID(level, 8)
	for i := range c8.Reflectors {
		if c8.Reflectors[i].ID == "c008-left" {
			c8.Reflectors[i] = Reflector{
				ID: "c008-left", A: Point{X: 4_100_000, Y: 2_100_000},
				B: Point{X: 4_100_000, Y: 3_100_000}, Normal: Vec{X: Q16}, Material: "edited-wall",
			}
		}
	}
	src, _ := sourceByID(level, "src-009")
	src.Emission = []BandEnergy{
		{BandID: "125Hz", Energy: 3_300_000},
		{BandID: "2000Hz", Energy: 4_300_000},
		{BandID: "500Hz", Energy: 4_100_000},
	}
	if err := ValidateLevel(level); err != nil {
		t.Fatal(err)
	}
	return level
}
