package ortho

import (
	"context"
	"image"
	"testing"
)

func runSetup(t *testing.T) (context.Context, *StageParams, *Truth, image.Image, string) {
	t.Helper()
	params := DefaultStageParams()
	src := genTerrainRGB(SampleSeed + 1)
	truth := genTruth(SampleSeed+2, BuildStages(params), params)
	return context.Background(), params, truth, src, ContentHash(src)
}

func TestIncrementalInvalidationList(t *testing.T) {
	ctx, params, _, src, srcHash := runSetup(t)
	stages := BuildStages(params)
	pipe := NewPipeline(stages, testCfg(), nil)
	if _, err := pipe.Run(ctx, src, srcHash); err != nil {
		t.Fatal(err)
	}
	// 调整第二阶段参数：仅该阶段及下游失效。
	params2 := *params
	params2.Radial.K1 += 0.05
	pipe2 := NewPipeline(BuildStages(&params2), testCfg(), pipe.Cache)
	res, err := pipe2.Run(ctx, src, srcHash)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		reused bool
		reason string
	}{
		{true, ""},
		{false, "params_or_input_changed"},
		{false, "upstream_invalidated"},
	}
	for i, w := range want {
		r := res.Reports[i]
		if r.Reused != w.reused || (!w.reused && r.Reason != w.reason) {
			t.Fatalf("stage %s: reused=%v reason=%q, want reused=%v reason=%q",
				r.Stage, r.Reused, r.Reason, w.reused, w.reason)
		}
	}
}

func TestIncrementalEqualsFull(t *testing.T) {
	ctx, params, _, src, srcHash := runSetup(t)
	pipe := NewPipeline(BuildStages(params), testCfg(), nil)
	if _, err := pipe.Run(ctx, src, srcHash); err != nil {
		t.Fatal(err)
	}
	// 改一次参数再改回，制造增量路径。
	params2 := *params
	params2.Affine.Tx += 5
	if _, err := NewPipeline(BuildStages(&params2), testCfg(), pipe.Cache).Run(ctx, src, srcHash); err != nil {
		t.Fatal(err)
	}
	inc, err := NewPipeline(BuildStages(params), testCfg(), pipe.Cache).Run(ctx, src, srcHash)
	if err != nil {
		t.Fatal(err)
	}
	full, err := NewPipeline(BuildStages(params), testCfg(), nil).Run(ctx, src, srcHash)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"homography", "radial", "affine"}
	if derr := CompareRuns(order, inc.Outputs, full.Outputs); derr != nil {
		t.Fatalf("incremental != full: %v", derr)
	}
}

func TestDiffLocatesFirstMismatch(t *testing.T) {
	ctx, params, _, src, srcHash := runSetup(t)
	a, err := NewPipeline(BuildStages(params), testCfg(), nil).Run(ctx, src, srcHash)
	if err != nil {
		t.Fatal(err)
	}
	params2 := *params
	params2.Affine.Tx += 1
	b, err := NewPipeline(BuildStages(&params2), testCfg(), nil).Run(ctx, src, srcHash)
	if err != nil {
		t.Fatal(err)
	}
	derr := CompareRuns([]string{"homography", "radial", "affine"}, a.Outputs, b.Outputs)
	if derr == nil || derr.Code != EDiff {
		t.Fatalf("expected E_DIFF, got %v", derr)
	}
	if derr.Stage != "affine" {
		t.Fatalf("E_DIFF stage = %s, want affine", derr.Stage)
	}
	if derr.X < 0 || derr.Y < 0 {
		t.Fatalf("E_DIFF missing coordinates: %v", derr)
	}
}

func TestCommitGuardNoSilentOverwrite(t *testing.T) {
	c := NewCache()
	e1 := &CacheEntry{Key: "k1"}
	if err := c.Commit("radial", e1); err != nil {
		t.Fatal(err)
	}
	if err := c.Commit("radial", &CacheEntry{Key: "k2"}); err == nil || err.Code != EState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	c.MarkInvalid("radial")
	if err := c.Commit("radial", &CacheEntry{Key: "k2"}); err != nil {
		t.Fatalf("commit after invalidation should succeed: %v", err)
	}
}

func TestStageFailureRetryKeepsOtherCaches(t *testing.T) {
	ctx, params, _, src, srcHash := runSetup(t)
	pipe := NewPipeline(BuildStages(params), testCfg(), nil)
	if _, err := pipe.Run(ctx, src, srcHash); err != nil {
		t.Fatal(err)
	}
	// 第三阶段参数越界：该阶段失败，前两级缓存保留。
	bad := *params
	bad.Affine.Sx = 100
	pipeBad := NewPipeline(BuildStages(&bad), testCfg(), pipe.Cache)
	res, err := pipeBad.Run(ctx, src, srcHash)
	if err == nil || err.Code != EParam {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	if res.Reports[2].Status != "failed" {
		t.Fatalf("stage status = %q, want failed", res.Reports[2].Status)
	}
	if res.Reports[0].Status != "reused" || res.Reports[1].Status != "reused" {
		t.Fatal("upstream stages should be reused, not polluted")
	}
	if _, ok := pipe.Cache.Get("homography"); !ok {
		t.Fatal("homography cache lost after downstream failure")
	}
	// 修正参数后重试成功。
	pipeFix := NewPipeline(BuildStages(params), testCfg(), pipe.Cache)
	if _, err := pipeFix.Run(ctx, src, srcHash); err != nil {
		t.Fatalf("retry after fix failed: %v", err)
	}
}

func TestCommitOrderStableParallelVsSerial(t *testing.T) {
	ctx, params, _, src, srcHash := runSetup(t)
	par := testCfg()
	ser := testCfg()
	ser.Workers = 1
	resPar, err := NewPipeline(BuildStages(params), par, nil).Run(ctx, src, srcHash)
	if err != nil {
		t.Fatal(err)
	}
	resSer, err := NewPipeline(BuildStages(params), ser, nil).Run(ctx, src, srcHash)
	if err != nil {
		t.Fatal(err)
	}
	if len(resPar.CommitOrder) != len(resSer.CommitOrder) {
		t.Fatal("commit order length differs")
	}
	for i := range resPar.CommitOrder {
		if resPar.CommitOrder[i] != resSer.CommitOrder[i] {
			t.Fatalf("commit order differs at %d: %v vs %v", i, resPar.CommitOrder, resSer.CommitOrder)
		}
	}
	if derr := CompareRuns([]string{"homography", "radial", "affine"}, resPar.Outputs, resSer.Outputs); derr != nil {
		t.Fatal(derr)
	}
}
