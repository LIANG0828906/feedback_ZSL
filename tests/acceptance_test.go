package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fixedsampler/internal/checkpoint"
	"fixedsampler/internal/engine"
	"fixedsampler/internal/loader"
	"fixedsampler/internal/model"
	"fixedsampler/internal/samplegen"
	"fixedsampler/internal/verifier"
)

func generate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := samplegen.GenerateAll(dir, samplegen.DefaultSeed); err != nil {
		t.Fatalf("generate samples: %v", err)
	}
	return dir
}

func loadProject(t *testing.T, dir, name string) *model.Project {
	t.Helper()
	p, err := loader.LoadProject(filepath.Join(dir, name, "project.json"))
	if err != nil {
		t.Fatalf("load project %s: %v", name, err)
	}
	return p
}

func loadTruth(t *testing.T, dir, name string) *model.TruthManifest {
	t.Helper()
	truth, err := loader.LoadTruth(filepath.Join(dir, name, "truth.json"))
	if err != nil {
		t.Fatalf("load truth: %v", err)
	}
	return truth
}

func runMemory(t *testing.T, p *model.Project, chunkSize, workers, buffer int, prev map[int]*engine.PreviousChunk) *engine.RunResult {
	t.Helper()
	res, err := engine.Run(context.Background(), p, engine.Options{
		ChunkSize: chunkSize, Workers: workers, BufferLimit: buffer, Previous: prev,
	})
	if err != nil {
		t.Fatalf("engine run: %v", err)
	}
	return res
}

func openStore(t *testing.T) *checkpoint.Store {
	t.Helper()
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func runStore(t *testing.T, p *model.Project, store *checkpoint.Store, chunkSize, workers, buffer int) *engine.RunResult {
	t.Helper()
	prev, err := store.LoadPrevious(context.Background(), p.ProjectID)
	if err != nil && !errors.Is(err, checkpoint.ErrSnapshotInvalid) {
		t.Fatalf("load previous: %v", err)
	}
	res, err := engine.Run(context.Background(), p, engine.Options{
		ChunkSize: chunkSize, Workers: workers, BufferLimit: buffer, Previous: prev,
		Commit: store.CommitChunk,
	})
	if err != nil {
		t.Fatalf("engine store run: %v", err)
	}
	if err := store.MarkComplete(context.Background(), p.ProjectID, res.ConfigFP, res.Grid.Count()); err != nil {
		t.Fatalf("mark complete: %v", err)
	}
	return res
}

func verifierRows(t *testing.T, store *checkpoint.Store, projectID string) map[int]verifier.PersistedRow {
	t.Helper()
	prev, err := store.LoadPrevious(context.Background(), projectID)
	if err != nil {
		t.Fatalf("load previous for verify: %v", err)
	}
	rows := map[int]verifier.PersistedRow{}
	for idx, pc := range prev {
		rows[idx] = verifier.PersistedRow{
			Artifact: *pc.Artifact, ConfigFP: pc.ConfigFP,
			ParamVersion: model.ParamVersion, AlgoVersion: model.AlgoVersion,
			ContentHashes: pc.ContentHashes,
		}
	}
	return rows
}

func TestGeneratedSamplesAllVerify(t *testing.T) {
	dir := generate(t)
	for _, name := range []string{"blocks420", "rampcolor444", "mixed422"} {
		t.Run(name, func(t *testing.T) {
			p := loadProject(t, dir, name)
			truth := loadTruth(t, dir, name)
			if len(truth.ErrorBounds) != 4*len(p.Config) {
				t.Fatalf("truth error bounds=%d want %d", len(truth.ErrorBounds), 4*len(p.Config))
			}
			store := openStore(t)
			runStore(t, p, store, 8, 3, 2)
			rows := verifierRows(t, store, p.ProjectID)
			result, err := verifier.Verify(p, truth, rows, loader.ConfigFingerprint(p))
			if err != nil {
				t.Fatalf("independent verify failed: %v", err)
			}
			if !result.OK || len(result.Deviations) == 0 {
				t.Fatalf("unexpected verify result: %+v", result)
			}
			for _, d := range result.Deviations {
				if !d.Within {
					t.Fatalf("point %s deviation %d > tolerance %d", d.PointID, d.DeviationFX, d.ToleranceFX)
				}
			}
		})
	}
}

func TestDeterminismAcrossWorkersAndScheduling(t *testing.T) {
	dir := generate(t)
	p := loadProject(t, dir, "blocks420")
	a := runMemory(t, p, 4, 1, 1, nil)
	b := runMemory(t, p, 4, 8, 8, nil)
	if loc := engine.CompareChunkMaps(a.Artifacts, b.Artifacts, a.CommitSequence, b.CommitSequence); loc != nil {
		t.Fatalf("nondeterministic result: %s", loc.Error())
	}
	if len(a.CommitSequence) != a.Grid.Count() || len(b.CommitSequence) != b.Grid.Count() {
		t.Fatalf("commit sequence missing chunks: %d/%d", len(a.CommitSequence), a.Grid.Count())
	}
	report := engine.BuildErrorReport(p, a, 1, 1)
	for i := 1; i < len(report.Cumulative); i++ {
		prev, cur := report.Cumulative[i-1], report.Cumulative[i]
		if cur.WeightNormFX < prev.WeightNormFX || cur.AccumTruncFX < prev.AccumTruncFX ||
			cur.ColorQuantFX < prev.ColorQuantFX || cur.BitDepthErrorFX < prev.BitDepthErrorFX {
			t.Fatalf("cumulative bounds decreased: prev=%+v cur=%+v", prev, cur)
		}
	}
}

func TestIncrementalBitDepthAndFactorMatchFull(t *testing.T) {
	cases := []struct {
		name       string
		editSeq    int
		edit       func(*model.Project)
		invalidCnt int
		invalidSeq map[int]bool
	}{
		{
			name: "last_quant_level_only", editSeq: 3,
			edit:       func(p *model.Project) { p.Config[2].BitDepth = 10 },
			invalidCnt: 24, invalidSeq: map[int]bool{3: true},
		},
		{
			name: "middle_factor_propagates_downstream", editSeq: 2,
			edit: func(p *model.Project) {
				p.Config[1].Factor = 4
				p.Config[1].FactorX = 4
				p.Config[1].FactorY = 4
				p.Config[1].Taps = 16
			},
			invalidCnt: 48, invalidSeq: map[int]bool{2: true, 3: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := generate(t)
			original := loadProject(t, dir, "blocks420")
			store := openStore(t)
			runStore(t, original, store, 8, 2, 1)
			prev, err := store.LoadPrevious(context.Background(), original.ProjectID)
			if err != nil {
				t.Fatal(err)
			}

			edited := loadProject(t, dir, "blocks420")
			tc.edit(edited)
			if err := loader.ValidateProject(edited); err != nil {
				t.Fatalf("edited project invalid: %v", err)
			}
			inc := runMemory(t, edited, 8, 2, 1, prev)
			if len(inc.Invalidations) != tc.invalidCnt {
				t.Fatalf("invalidations=%d want %d: %+v", len(inc.Invalidations), tc.invalidCnt, inc.Invalidations)
			}
			for _, inv := range inc.Invalidations {
				if !tc.invalidSeq[inv.StageSeq] {
					t.Fatalf("unexpected invalidation stage %d in %+v", inv.StageSeq, inv)
				}
				if inv.Reason == "" {
					t.Fatalf("invalidation missing reason: %+v", inv)
				}
			}

			full := runMemory(t, edited, 8, 1, 1, nil)
			if loc := engine.CompareChunkMaps(inc.Artifacts, full.Artifacts, inc.CommitSequence, full.CommitSequence); loc != nil {
				t.Fatalf("incremental/full mismatch: %s", loc.Error())
			}
		})
	}
}

func TestCheckpointCancelRecoveryAndTamperPurge(t *testing.T) {
	dir := generate(t)
	p := loadProject(t, dir, "blocks420")
	store := openStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := engine.Run(ctx, p, engine.Options{
		ChunkSize: 8, Workers: 2, BufferLimit: 1,
		Commit: store.CommitChunk,
		OnChunkCommitted: func(_ int, committed int) {
			if committed == 2 {
				cancel()
			}
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	dbPath := store.Path()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	prev, err := reopened.LoadPrevious(context.Background(), p.ProjectID)
	if err != nil {
		t.Fatalf("reload durable chunks: %v", err)
	}
	if len(prev) != 2 {
		t.Fatalf("durable chunks=%d want 2", len(prev))
	}
	res, err := engine.Run(context.Background(), p, engine.Options{
		ChunkSize: 8, Workers: 2, BufferLimit: 1, Previous: prev, Commit: reopened.CommitChunk,
	})
	if err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	if want := (res.Grid.Count() - 2) * len(p.Config); len(res.Invalidations) != want {
		t.Fatalf("recovery invalidations=%d want %d", len(res.Invalidations), want)
	}
	truth := loadTruth(t, dir, "blocks420")
	rows := verifierRowsWithStore(t, reopened, p.ProjectID)
	if _, err := verifier.Verify(p, truth, rows, loader.ConfigFingerprint(p)); err != nil {
		t.Fatalf("post-recovery verify: %v", err)
	}

	if err := reopened.TamperVersion(context.Background(), p.ProjectID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.LoadPrevious(context.Background(), p.ProjectID); !errors.Is(err, checkpoint.ErrSnapshotInvalid) {
		t.Fatalf("expected snapshot invalid after version tamper, got %v", err)
	}
}

func verifierRowsWithStore(t *testing.T, store *checkpoint.Store, projectID string) map[int]verifier.PersistedRow {
	t.Helper()
	prev, err := store.LoadPrevious(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[int]verifier.PersistedRow{}
	for idx, pc := range prev {
		rows[idx] = verifier.PersistedRow{
			Artifact: *pc.Artifact, ConfigFP: pc.ConfigFP, ParamVersion: model.ParamVersion,
			AlgoVersion: model.AlgoVersion, ContentHashes: pc.ContentHashes,
		}
	}
	return rows
}

func TestValidationErrorCodes(t *testing.T) {
	dir := generate(t)
	valid := loadProject(t, dir, "blocks420")

	missingPath := filepath.Join(t.TempDir(), "missing.json")
	if err := writeFile(missingPath, []byte(`{"project_id":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := loader.LoadProject(missingPath); !hasCode(err, model.CodeSchema) {
		t.Fatalf("missing fields expected E_SCHEMA, got %v", err)
	}

	cases := []struct {
		name string
		code string
		edit func(*model.Project)
	}{
		{"chroma dims", model.CodeDim, func(p *model.Project) { p.Planes[1].SubsampleX = 1 }},
		{"factor divisibility", model.CodeDim, func(p *model.Project) { p.Config[1].Factor = 3; p.Config[1].FactorX = 3; p.Config[1].FactorY = 3 }},
		{"sequence", model.CodeSchema, func(p *model.Project) { p.Config[1].Seq = 99 }},
		{"bit depth", model.CodeSchema, func(p *model.Project) { p.Config[2].BitDepth = 7 }},
		{"kernel", model.CodeKernel, func(p *model.Project) { p.Config[0].Kernel = "bicubic" }},
		{"scale", model.CodeKernel, func(p *model.Project) { p.Config[0].FixedScale = 16 }},
		{"conversion", model.CodeConv, func(p *model.Project) {
			ramp := loadProject(t, dir, "rampcolor444")
			*p = *ramp
			p.Config[1].TargetSpace = "CMYK"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := cloneProject(valid)
			tc.edit(p)
			err := loader.ValidateProject(p)
			if !hasCode(err, tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, err)
			}
			if tc.code == model.CodeDim {
				var pe *model.PipelineError
				if errors.As(err, &pe) && pe.Plane == "" {
					t.Fatalf("E_DIM must identify plane: %+v", pe)
				}
			}
			if tc.code == model.CodeKernel || tc.code == model.CodeConv {
				var pe *model.PipelineError
				if errors.As(err, &pe) && pe.ConfigSeq == nil {
					t.Fatalf("%s must identify config seq: %+v", tc.code, pe)
				}
			}
			if tc.code == model.CodeConv {
				var pe *model.PipelineError
				if errors.As(err, &pe) && (pe.SourceSpace == "" || pe.TargetSpace == "") {
					t.Fatalf("E_CONV must identify spaces: %+v", pe)
				}
			}
		})
	}
}

func TestVerifierLocatesFieldMismatch(t *testing.T) {
	dir := generate(t)
	p := loadProject(t, dir, "blocks420")
	res := runMemory(t, p, 8, 2, 2, nil)
	rows := map[int]verifier.PersistedRow{}
	target := -1
	for idx, chunk := range res.Artifacts {
		hashes := make([]string, 0, len(chunk.Levels))
		for _, level := range chunk.Levels {
			hashes = append(hashes, engine.HashTiles(level.Tiles))
		}
		rows[idx] = verifier.PersistedRow{
			Artifact: *chunk, ConfigFP: res.ConfigFP, ParamVersion: model.ParamVersion,
			AlgoVersion: model.AlgoVersion, ContentHashes: hashes,
		}
		if target < 0 {
			for _, level := range chunk.Levels {
				if level.Seq == 1 && len(level.Tiles) > 0 && len(level.Tiles[0].Values) > 0 {
					target = idx
				}
			}
		}
	}
	if target < 0 {
		t.Fatal("no target tile")
	}
	row := rows[target]
	row.Artifact.Levels[1].Tiles[0].Values[0]++
	row.ContentHashes[1] = engine.HashTiles(row.Artifact.Levels[1].Tiles)
	rows[target] = row
	truth := loadTruth(t, dir, "blocks420")
	_, err := verifier.Verify(p, truth, rows, res.ConfigFP)
	var loc *model.DiffLocation
	if !errors.As(err, &loc) {
		t.Fatalf("expected E_DIFF location, got %v", err)
	}
	if loc.StageSeq != 1 || !strings.Contains(loc.Field, "values[") {
		t.Fatalf("unexpected location: %+v", loc)
	}
}

func TestToleranceTighteningCountMonotonic(t *testing.T) {
	dir := generate(t)
	p := loadProject(t, dir, "blocks420")
	truth := loadTruth(t, dir, "blocks420")
	res := runMemory(t, p, 8, 2, 2, nil)
	rows := map[int]verifier.PersistedRow{}
	for idx, chunk := range res.Artifacts {
		hashes := []string{}
		for _, level := range chunk.Levels {
			hashes = append(hashes, engine.HashTiles(level.Tiles))
		}
		rows[idx] = verifier.PersistedRow{
			Artifact: *chunk, ConfigFP: res.ConfigFP,
			ParamVersion: model.ParamVersion, AlgoVersion: model.AlgoVersion,
			ContentHashes: hashes,
		}
	}
	result, err := verifier.Verify(p, truth, rows, res.ConfigFP)
	if err != nil {
		t.Fatal(err)
	}
	tolerances := []int64{1_000_000, 100_000, 10_000, 1_000, 0}
	previous := len(result.Deviations)
	for _, tol := range tolerances {
		count := 0
		for _, d := range result.Deviations {
			if d.DeviationFX <= tol {
				count++
			}
		}
		if count > previous {
			t.Fatalf("accepted count increased at tolerance %d: %d > %d", tol, count, previous)
		}
		previous = count
	}
}

func hasCode(err error, code string) bool {
	var pe *model.PipelineError
	return errors.As(err, &pe) && pe.Code == code
}

func cloneProject(p *model.Project) *model.Project {
	cp := *p
	cp.Planes = make([]*model.Plane, 0, len(p.Planes))
	for _, plane := range p.Planes {
		planeCopy := *plane
		cp.Planes = append(cp.Planes, &planeCopy)
	}
	cp.Config = append([]model.StageConfig(nil), p.Config...)
	cp.SupportedDepths = append([]int(nil), p.SupportedDepths...)
	return &cp
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
