package acceptance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"cadpipe/internal/cad"
	"cadpipe/internal/pipeline"
	"cadpipe/internal/render"
	"cadpipe/internal/sample"
	"cadpipe/internal/schema"
	"cadpipe/internal/store"
	"cadpipe/internal/tessellate"
)

type Options struct {
	Seed    int64
	WorkDir string
	Workers int
}

func Run(opts Options) *Report {
	if opts.Seed == 0 {
		opts.Seed = sample.DefaultSeed
	}
	if opts.WorkDir == "" {
		opts.WorkDir = "artifacts/acceptance"
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	_ = os.RemoveAll(opts.WorkDir)
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		return &Report{OK: false, Seed: opts.Seed, GeneratedAt: time.Now().UTC(), Checks: []Check{{
			Name: "prepare_workdir", OK: false, Error: err.Error(),
		}}}
	}
	r := &Report{OK: true, Seed: opts.Seed, GeneratedAt: time.Now().UTC()}

	r.add("schema_three_version_roundtrip", func() (any, error) { return checkSchema(opts.Seed) })
	r.add("analytic_tessellation_and_bounds", func() (any, error) { return checkTruth(opts.Seed) })
	r.add("clip_probes_and_e_clip", func() (any, error) { return checkClip(opts.Seed) })
	r.add("tile_hash_and_parallel_stability", func() (any, error) { return checkHashStability(opts.Seed, opts.Workers) })
	r.add("control_point_precise_invalidation", func() (any, error) {
		return checkEditScenario(opts, "control.db", func(p *pipeline.Pipeline) (*pipeline.InvalidationPlan, error) {
			patch := p.Scene.Patch("surface-alpha")
			next := patch.ControlPoints[1][1]
			next.Z += cad.Millimetre(3)
			return p.UpdateControlPoint("surface-alpha", 1, 1, next)
		}, nil)
	})
	r.add("material_precise_invalidation", func() (any, error) {
		return checkEditScenario(opts, "material.db", func(p *pipeline.Pipeline) (*pipeline.InvalidationPlan, error) {
			return p.UpdateMaterial("surface-beta", cad.Material{
				Name: "edited-material", Color: cad.RGB{R: 0.91, G: 0.22, B: 0.73},
				Metallic: 0.72, Roughness: 0.18,
			})
		}, nil)
	})
	r.add("subdivision_precise_invalidation", func() (any, error) {
		return checkEditScenario(opts, "subdivision.db", func(p *pipeline.Pipeline) (*pipeline.InvalidationPlan, error) {
			return p.UpdateSubdivision("surface-gamma", cad.Subdivision{
				Density: 12, CurvatureSensitivity: 0.8, MaxDensity: 48,
			})
		}, nil)
	})
	r.add("clip_flip_set_invalidation", func() (any, error) { return checkClipEdit(opts) })
	r.add("sqlite_cancel_recovery_and_full_match", func() (any, error) { return checkRecovery(opts) })
	r.add("sqlite_version_mismatch_purges_snapshot", func() (any, error) { return checkSnapshotMismatch(opts) })
	r.add("buffer_pool_backpressure_recorded", checkBufferPool)
	r.add("e_diff_locates_first_pixel_and_field", func() (any, error) { return checkDiffLocator(opts.Seed) })
	return r
}

func checkSchema(seed int64) (any, error) {
	b, err := sample.Generate(seed)
	if err != nil {
		return nil, err
	}
	lossCounts := map[string]int{}
	for _, data := range [][]byte{b.SchemaV1, b.SchemaV2, b.SchemaV3} {
		loaded, err := schema.Load(data)
		if err != nil {
			return nil, err
		}
		for _, target := range []string{schema.V1, schema.V2, schema.V3} {
			first, loss, err := schema.Export(loaded.Scene, target)
			if err != nil {
				return nil, err
			}
			second, _, err := schema.Export(loaded.Scene, target)
			if err != nil {
				return nil, err
			}
			if string(first) != string(second) {
				return nil, fmt.Errorf("non-deterministic export %s", target)
			}
			reloaded, err := schema.Load(first)
			if err != nil {
				return nil, err
			}
			if err := compareSceneGeometry(loaded.Scene, reloaded.Scene); err != nil {
				return nil, err
			}
			lossCounts[target] += len(loss.Entries)
		}
	}
	_, unknownErr := schema.Load([]byte(`{"schema":"cad.scene/v404"}`))
	ce, ok := cad.AsError(unknownErr)
	if !ok || ce.Code != cad.CodeVersion || ce.Version != "cad.scene/v404" {
		return nil, fmt.Errorf("unknown version did not return E_VER: %w", unknownErr)
	}
	return map[string]any{"loss_entries": lossCounts, "unknown_version_path": ce.Path}, nil
}

func compareSceneGeometry(a, b *cad.Scene) error {
	if len(a.Patches) != len(b.Patches) {
		return fmt.Errorf("patch count mismatch")
	}
	for i := range a.Patches {
		x, y := a.Patches[i], b.Patches[i]
		if cad.HashJSON(x.ControlPoints) != cad.HashJSON(y.ControlPoints) ||
			cad.HashJSON(x.KnotsU) != cad.HashJSON(y.KnotsU) ||
			cad.HashJSON(x.KnotsV) != cad.HashJSON(y.KnotsV) ||
			cad.HashJSON(x.Weights) != cad.HashJSON(y.Weights) ||
			cad.HashJSON(x.Clip) != cad.HashJSON(y.Clip) {
			return fmt.Errorf("geometry mismatch for %s", x.ID)
		}
	}
	return nil
}

func checkTruth(seed int64) (any, error) {
	b, err := sample.Generate(seed)
	if err != nil {
		return nil, err
	}
	maxAxis := 0.0
	for _, point := range b.Truth.SurfacePoints {
		got := tessellate.Evaluate(b.Scene.Patch(point.PatchID), point.U, point.V)
		want := cad.Vec3{X: point.X, Y: point.Y, Z: point.Z}
		maxAxis = maxFloat(maxAxis, axisDelta(got, want))
	}
	if maxAxis > 1e-9 {
		return nil, fmt.Errorf("analytic sample max axis error %g", maxAxis)
	}
	for _, bound := range b.Truth.Bounds {
		mesh, err := tessellate.Tessellate(b.Scene.Patch(bound.PatchID), nil)
		if err != nil {
			return nil, err
		}
		for _, v := range mesh.Vertices {
			if !bound.Bounds.Contains(v.Position, 1e-9) {
				return nil, fmt.Errorf("%s vertex %+v outside bounds %+v", bound.PatchID, v.Position, bound.Bounds)
			}
		}
	}
	return map[string]any{"truth_points": len(b.Truth.SurfacePoints), "max_axis_error": maxAxis}, nil
}

func checkClip(seed int64) (any, error) {
	b, err := sample.Generate(seed)
	if err != nil {
		return nil, err
	}
	for _, probe := range b.Truth.ClipProbes {
		patch := b.Scene.Patch(probe.PatchID)
		got := cad.PointInsideClip(patch.Clip, cad.Vec2{U: probe.U, V: probe.V})
		if got != probe.Inside {
			return nil, fmt.Errorf("probe %s inside=%t want %t", probe.ID, got, probe.Inside)
		}
	}
	selfIntersect := cad.ClipBoundary{Loops: [][]cad.Vec2{{
		{U: 0.1, V: 0.1}, {U: 0.9, V: 0.9}, {U: 0.9, V: 0.1}, {U: 0.1, V: 0.9},
	}}}
	err = cad.ValidateClip("bad-self", selfIntersect)
	ce, ok := cad.AsError(err)
	if !ok || ce.Code != cad.CodeClip || ce.PatchID != "bad-self" || ce.Point == nil {
		return nil, fmt.Errorf("self intersection error = %v", err)
	}
	outOfRange := cad.ClipBoundary{Loops: [][]cad.Vec2{{
		{U: -0.2, V: 0.2}, {U: 0.8, V: 0.2}, {U: 0.5, V: 0.9},
	}}}
	if err := cad.ValidateClip("bad-range", outOfRange); err == nil {
		return nil, fmt.Errorf("out-of-range clip was accepted")
	}
	return map[string]any{"probes": len(b.Truth.ClipProbes), "self_intersection_point": ce.Point}, nil
}

func checkHashStability(seed int64, workers int) (any, error) {
	b, err := sample.Generate(seed)
	if err != nil {
		return nil, err
	}
	single, err := pipeline.New(b.Scene, b.Camera, b.Truth.ClipProbes, pipeline.Options{Workers: 1, BufferCount: 1})
	if err != nil {
		return nil, err
	}
	if _, err := single.RunFull(context.Background()); err != nil {
		return nil, err
	}
	parallel, err := pipeline.New(b.Scene, b.Camera, b.Truth.ClipProbes, pipeline.Options{Workers: workers, BufferCount: workers})
	if err != nil {
		return nil, err
	}
	if _, err := parallel.RunFull(context.Background()); err != nil {
		return nil, err
	}
	if err := pipeline.CompareTileMaps(single.Cache, parallel.Cache, b.Camera.TileCount()); err != nil {
		return nil, err
	}
	coveredExpectations := 0
	for _, expected := range b.Tiles.Tiles {
		tile := single.Cache[expected.TileID]
		if tile == nil {
			return nil, fmt.Errorf("expected analytic tile %d missing from cache", expected.TileID)
		}
		if expected.InsideSampleCount > 0 && tile.Coverage <= 0 {
			return nil, fmt.Errorf("expected analytic tile %d to have coverage", expected.TileID)
		}
		if expected.ExpectedPixelSummary == "" {
			return nil, fmt.Errorf("tile %d missing independent summary", expected.TileID)
		}
		coveredExpectations++
	}
	return map[string]any{"tiles": b.Camera.TileCount(), "analytic_tile_expectations": coveredExpectations}, nil
}

func checkEditScenario(opts Options, dbName string, edit func(*pipeline.Pipeline) (*pipeline.InvalidationPlan, error), validate func(*pipeline.Pipeline, *pipeline.InvalidationPlan) error) (any, error) {
	b, err := sample.Generate(opts.Seed)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(opts.WorkDir, dbName))
	if err != nil {
		return nil, err
	}
	defer st.Close()
	p, err := pipeline.New(b.Scene, b.Camera, b.Truth.ClipProbes, pipeline.Options{
		Workers: 2, BufferCount: 2, Store: st,
	})
	if err != nil {
		return nil, err
	}
	if _, err := p.RunFull(context.Background()); err != nil {
		return nil, err
	}
	before := snapshotTiles(p)
	plan, err := edit(p)
	if err != nil {
		return nil, err
	}
	if plan == nil || len(plan.TileIDs) == 0 {
		return nil, fmt.Errorf("edit did not invalidate any tile")
	}
	report, err := p.RunPlan(context.Background(), plan)
	if err != nil {
		return nil, err
	}
	invalid := map[int]bool{}
	for _, id := range plan.TileIDs {
		invalid[id] = true
	}
	unaffected := 0
	for id := 0; id < b.Camera.TileCount(); id++ {
		if invalid[id] {
			continue
		}
		unaffected++
		if p.Cache[id].ContentHash != before[id].Hash || string(p.Cache[id].Color) != string(before[id].Color) {
			return nil, fmt.Errorf("unaffected tile %d changed", id)
		}
	}
	want, err := pipeline.NewWithVersions(b.Scene, b.Camera, b.Truth.ClipProbes,
		pipeline.Options{Workers: 1, BufferCount: 1}, p.Versions.Clone())
	if err != nil {
		return nil, err
	}
	if _, err := want.RunFull(context.Background()); err != nil {
		return nil, err
	}
	if err := p.CompareWith(want); err != nil {
		return nil, err
	}
	if validate != nil {
		if err := validate(p, plan); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"invalid_tiles": plan.TileIDs, "unaffected_tiles_checked": unaffected,
		"recomputed": report.Recomputed, "topology_nodes": len(plan.TopologicalOrder),
		"manifest": plan.Tiles,
	}, nil
}

type tileSnapshot struct {
	Hash  string
	Color []byte
}

func snapshotTiles(p *pipeline.Pipeline) map[int]tileSnapshot {
	out := make(map[int]tileSnapshot, len(p.Cache))
	for id, tile := range p.Cache {
		out[id] = tileSnapshot{Hash: tile.ContentHash, Color: append([]byte(nil), tile.Color...)}
	}
	return out
}

func checkClipEdit(opts Options) (any, error) {
	return checkEditScenario(opts, "clip.db", func(p *pipeline.Pipeline) (*pipeline.InvalidationPlan, error) {
		return p.UpdateClip("surface-alpha", cad.ClipBoundary{Loops: [][]cad.Vec2{{
			{U: 0.18, V: 0.18}, {U: 0.82, V: 0.18},
			{U: 0.82, V: 0.64}, {U: 0.64, V: 0.82}, {U: 0.18, V: 0.82},
		}}})
	}, func(_ *pipeline.Pipeline, plan *pipeline.InvalidationPlan) error {
		var probes []string
		for _, tile := range plan.Tiles {
			probes = append(probes, tile.FlippedProbes...)
		}
		if len(probes) != 1 || probes[0] != "surface-alpha-upper-right-inside" {
			return fmt.Errorf("flip set does not match truth: %v", probes)
		}
		return nil
	})
}

func checkRecovery(opts Options) (any, error) {
	b, err := sample.Generate(opts.Seed)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(opts.WorkDir, "recovery.db")
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	p, err := pipeline.New(b.Scene, b.Camera, b.Truth.ClipProbes, pipeline.Options{Workers: 4, BufferCount: 4, Store: st})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.CommitHook = func(_ int, committed int) {
		if committed == 3 {
			cancel()
		}
	}
	_, runErr := p.RunFull(ctx)
	if !errors.Is(runErr, context.Canceled) {
		_ = st.Close()
		return nil, fmt.Errorf("expected cancellation, got %v", runErr)
	}
	if err := st.Close(); err != nil {
		return nil, err
	}

	st2, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	defer st2.Close()
	recovered, err := pipeline.New(b.Scene, b.Camera, b.Truth.ClipProbes, pipeline.Options{Workers: 2, Store: st2})
	if err != nil {
		return nil, err
	}
	n, err := recovered.Recover(context.Background())
	if err != nil {
		return nil, err
	}
	if n != 3 {
		return nil, fmt.Errorf("recovered %d tiles, want exactly 3 committed tiles", n)
	}
	resume, err := recovered.ResumeMissing(context.Background())
	if err != nil {
		return nil, err
	}
	want, err := pipeline.New(b.Scene, b.Camera, b.Truth.ClipProbes, pipeline.Options{Workers: 1, BufferCount: 1})
	if err != nil {
		return nil, err
	}
	if _, err := want.RunFull(context.Background()); err != nil {
		return nil, err
	}
	if err := recovered.CompareWith(want); err != nil {
		return nil, err
	}
	count, _ := st2.Count(context.Background())
	return map[string]any{"recovered": n, "resumed": len(resume.Recomputed), "checkpoint_rows": count}, nil
}

func checkSnapshotMismatch(opts Options) (any, error) {
	b, _ := sample.Generate(opts.Seed)
	path := filepath.Join(opts.WorkDir, "mismatch.db")
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	p, err := pipeline.New(b.Scene, b.Camera, b.Truth.ClipProbes, pipeline.Options{Workers: 2, Store: st})
	if err != nil {
		return nil, err
	}
	if _, err := p.RunFull(context.Background()); err != nil {
		return nil, err
	}
	if err := st.Close(); err != nil {
		return nil, err
	}
	st2, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	defer st2.Close()
	versions := pipeline.NewVersions(b.Scene)
	versions.SceneStructure = 999
	p2, err := pipeline.NewWithVersions(b.Scene, b.Camera, b.Truth.ClipProbes, pipeline.Options{Store: st2}, versions)
	if err != nil {
		return nil, err
	}
	_, err = p2.Recover(context.Background())
	var mismatch *store.SnapshotMismatchError
	if !errors.As(err, &mismatch) {
		return nil, fmt.Errorf("expected SnapshotMismatchError, got %v", err)
	}
	count, err := st2.Count(context.Background())
	if err != nil {
		return nil, err
	}
	if count != 0 {
		return nil, fmt.Errorf("mismatched snapshot was not fully purged: %d rows", count)
	}
	return map[string]any{"reason": mismatch.Reason, "purged_rows": count}, nil
}

func checkBufferPool() (any, error) {
	pool := pipeline.NewBufferPool(1, 256)
	first, err := pool.Acquire(1, 256)
	if err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		second, err := pool.Acquire(2, 256)
		if err != nil {
			done <- err
			return
		}
		second.Release()
		done <- nil
	}()
	time.Sleep(20 * time.Millisecond)
	first.Release()
	if err := <-done; err != nil {
		return nil, err
	}
	waits := pool.SnapshotWaits()
	if len(waits) == 0 || waits[0].WaitedFor <= 0 {
		return nil, fmt.Errorf("expected blocking wait record")
	}
	if _, err := pool.Acquire(3, 512); err == nil {
		return nil, fmt.Errorf("buffer cap violation was accepted")
	}
	return map[string]any{"wait_records": len(waits), "first_wait": waits[0].WaitedFor.String()}, nil
}

func checkDiffLocator(seed int64) (any, error) {
	b, _ := sample.Generate(seed)
	p, err := pipeline.New(b.Scene, b.Camera, b.Truth.ClipProbes, pipeline.Options{Workers: 1})
	if err != nil {
		return nil, err
	}
	if _, err := p.RunFull(context.Background()); err != nil {
		return nil, err
	}
	want := make(map[int]*render.TileResult, len(p.Cache))
	got := make(map[int]*render.TileResult, len(p.Cache))
	for id, tile := range p.Cache {
		want[id] = tile
		copyTile := *tile
		copyTile.Color = append([]byte(nil), tile.Color...)
		got[id] = &copyTile
	}
	got[0].Color[0] ^= 0xff
	err = pipeline.CompareTileMaps(want, got, b.Camera.TileCount())
	ce, ok := cad.AsError(err)
	if !ok || ce.Code != cad.CodeDiff || ce.TileID != 0 || ce.Pixel == nil || ce.Field == "" {
		return nil, fmt.Errorf("diff locator error = %v", err)
	}
	return map[string]any{"tile": ce.TileID, "pixel": ce.Pixel, "field": ce.Field}, nil
}

func axisDelta(a, b cad.Vec3) float64 {
	return maxFloat(absFloat(a.X-b.X), maxFloat(absFloat(a.Y-b.Y), absFloat(a.Z-b.Z)))
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
