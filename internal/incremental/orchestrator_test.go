package incremental_test

import (
	"path/filepath"
	"testing"

	"github.com/gameaudio/workbench/internal/compare"
	"github.com/gameaudio/workbench/internal/engine"
	"github.com/gameaudio/workbench/internal/generator"
	"github.com/gameaudio/workbench/internal/incremental"
	"github.com/gameaudio/workbench/internal/loader"
	"github.com/gameaudio/workbench/internal/model"
	"github.com/gameaudio/workbench/internal/pool"
	"github.com/gameaudio/workbench/internal/storage"
)

func testProject(t *testing.T) *model.Project {
	t.Helper()
	p := generator.Generate()
	cfg := model.GeoConfig{ScaleBits: generator.DefaultScaleBits}
	if err := loader.Validate(p, cfg); err != nil {
		t.Fatal(err)
	}
	return p
}

func newOrch(t *testing.T, p *model.Project, dbName string,
) (*incremental.Orchestrator, *storage.Store) {
	t.Helper()
	cfg := model.GeoConfig{ScaleBits: generator.DefaultScaleBits}
	store, err := storage.Open(filepath.Join(t.TempDir(), dbName))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg := pool.Config{MaxWorkers: 3, PerWorkerBuffer: 2}
	orch := incremental.New(p, eng, store, cfg, poolCfg)
	if err := orch.SaveSnapshotMeta(); err != nil {
		t.Fatal(err)
	}
	return orch, store
}

func TestFullRecomputeMatchesTruth(t *testing.T) {
	p := testProject(t)
	orch, _ := newOrch(t, p, "a.sqlite")
	results, _, err := orch.FullRecompute()
	if err != nil {
		t.Fatal(err)
	}
	truth := map[string]model.Truth{}
	for _, tr := range p.Truths {
		truth[tr.SourceID+"->"+tr.ListenerID] = tr
	}
	for _, r := range results {
		if d := compare.Truth(r, truth[r.SourceID+"->"+r.ListenerID]); d != nil {
			t.Fatalf("truth mismatch: %s", d.String())
		}
	}
}

func TestIncrementalEqualsFullAfterParamEdit(t *testing.T) {
	p := testProject(t)

	// 基准：编辑后的工程从空缓存全量重算。
	edited := cloneForTest(p)
	for i := range edited.Regions {
		if edited.Regions[i].ID == "R2" {
			edited.Regions[i].ReverbTime = 0.9
			edited.Regions[i].ParamVersion++
		}
	}
	baseOrch, _ := newOrch(t, edited, "base.sqlite")
	baseResults, baseProfiles, err := baseOrch.FullRecompute()
	if err != nil {
		t.Fatal(err)
	}

	// 增量：先在原工程上全量建缓存，再局部失效 R2。
	incOrch, incStore := newOrch(t, p, "inc.sqlite")
	if _, _, err := incOrch.FullRecompute(); err != nil {
		t.Fatal(err)
	}
	inv := incOrch.InvalidateRegion("R2", "reverbTime edited")
	if len(inv) == 0 {
		t.Fatal("expected invalidations after region edit")
	}
	// R1 的对不应被失效。
	for _, v := range inv {
		if v.Kind == "pair" && pairBelongsToRegion(p, v.ObjectID, "R1") {
			t.Fatalf("R1 pair must not be invalidated: %s", v.ObjectID)
		}
	}

	cfg := model.GeoConfig{ScaleBits: generator.DefaultScaleBits}
	eng2, err := engine.New(edited, cfg)
	if err != nil {
		t.Fatal(err)
	}
	incOrch2 := incremental.New(edited, eng2, incStore, cfg,
		pool.Config{MaxWorkers: 3, PerWorkerBuffer: 2})
	cache, hashes := incOrch.ExportCache()
	incOrch2.SeedCache(cache, hashes)
	incResults, incProfiles, err := incOrch2.Incremental()
	if err != nil {
		t.Fatal(err)
	}

	if d := compare.Results(incResults, baseResults); d != nil {
		t.Fatalf("incremental vs full mismatch: %s", d.String())
	}
	if d := compare.Profiles(incProfiles, baseProfiles); d != nil {
		t.Fatalf("profile mismatch: %s", d.String())
	}

	hits, misses, _ := incOrch2.CacheStats()
	if hits == 0 {
		t.Fatal("unaffected pairs must be served from cache")
	}
	totalPairs := len(p.Sources) * len(p.Listeners)
	if hits+misses != totalPairs {
		t.Fatalf("hits+misses=%d want %d", hits+misses, totalPairs)
	}
}

func TestBoundaryDragInvalidation(t *testing.T) {
	p := testProject(t)
	orch, _ := newOrch(t, p, "drag.sqlite")
	if _, _, err := orch.FullRecompute(); err != nil {
		t.Fatal(err)
	}
	inv := orch.InvalidateRegion("R1", "boundary dragged")
	reason := ""
	for _, v := range inv {
		if v.Kind == "pair" {
			reason = v.Reason
		}
	}
	if reason == "" {
		t.Fatal("expected pair invalidations")
	}
}

func TestSnapshotVersionMismatchDiscards(t *testing.T) {
	p := testProject(t)
	orch, store := newOrch(t, p, "snap.sqlite")
	if _, _, err := orch.FullRecompute(); err != nil {
		t.Fatal(err)
	}
	windows, _ := store.LoadedWindows()
	if len(windows) == 0 {
		t.Fatal("expected committed windows")
	}

	// 修改工程使内容哈希变化，重开编排器做恢复，应整体作废并重算。
	edited := cloneForTest(p)
	edited.Regions[0].ReverbTime = 1.5
	edited.Regions[0].ParamVersion++
	eng2, _ := engine.New(edited, model.GeoConfig{ScaleBits: generator.DefaultScaleBits})
	recOrch := incremental.New(edited, eng2, store,
		model.GeoConfig{ScaleBits: generator.DefaultScaleBits},
		pool.Config{MaxWorkers: 2, PerWorkerBuffer: 2})
	_, _, reason, err := recOrch.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if reason == "ok" {
		t.Fatal("stale snapshot must be discarded, not reused")
	}
	after, _ := store.LoadedWindows()
	if len(after) != len(windows) {
		t.Fatalf("window count after full rebuild mismatch: %d vs %d",
			len(after), len(windows))
	}
}

func TestRecoveryReusesCompatibleSnapshot(t *testing.T) {
	p := testProject(t)
	orch, store := newOrch(t, p, "rec.sqlite")
	if _, _, err := orch.FullRecompute(); err != nil {
		t.Fatal(err)
	}
	baseResults, _, err := orch.FullRecompute()
	if err != nil {
		t.Fatal(err)
	}

	// 模拟重启：同工程、同版本重开库。
	eng2, _ := engine.New(p, model.GeoConfig{ScaleBits: generator.DefaultScaleBits})
	recOrch := incremental.New(p, eng2, store,
		model.GeoConfig{ScaleBits: generator.DefaultScaleBits},
		pool.Config{MaxWorkers: 2, PerWorkerBuffer: 2})
	recResults, _, reason, err := recOrch.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if reason != "ok" {
		t.Fatalf("compatible snapshot must be reused, reason=%s", reason)
	}
	if d := compare.Results(recResults, baseResults); d != nil {
		t.Fatalf("recovery mismatch: %s", d.String())
	}
}

func cloneForTest(p *model.Project) *model.Project {
	cp := *p
	cp.Regions = append([]model.Region(nil), p.Regions...)
	cp.Sources = append([]model.Source(nil), p.Sources...)
	cp.Listeners = append([]model.Listener(nil), p.Listeners...)
	cp.Walls = append([]model.Wall(nil), p.Walls...)
	cp.Truths = append([]model.Truth(nil), p.Truths...)
	for i := range cp.Regions {
		cp.Regions[i].Vertices = append([]model.Point(nil), p.Regions[i].Vertices...)
	}
	return &cp
}

func pairBelongsToRegion(p *model.Project, pairKey, regionID string) bool {
	for _, s := range p.Sources {
		for _, l := range p.Listeners {
			if s.ID+"->"+l.ID == pairKey {
				return s.RegionID == regionID
			}
		}
	}
	return false
}
