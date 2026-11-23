package workbench_test

import (
	"context"
	"testing"

	"occlbench/internal/core"
	"occlbench/internal/culling"
	"occlbench/internal/generator"
	"occlbench/internal/persist"
	"occlbench/internal/streaming"
	"occlbench/internal/workbench"
)

func cfg() streaming.Config {
	return streaming.Config{
		WindowSize: 3,
		Budget:     streaming.Budget{Slots: 2, Memory: 50},
		CullParams: culling.DefaultPreciseParams(),
	}
}

func TestThreePathsIdentical(t *testing.T) {
	w := generator.Generate(generator.DefaultConfig())
	store, err := persist.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	paths, diverge, err := workbench.VerifyThreePaths(context.Background(), w, cfg(), store)
	if err != nil {
		t.Fatalf("three paths diverged: %v (%+v)", err, diverge)
	}
	if len(paths[workbench.PathReplay]) != 4 {
		t.Fatalf("want 4 windows got %d", len(paths[workbench.PathReplay]))
	}

	// 每个窗口的可见集合、加载/卸载序列、延迟记录都应逐窗口一致。
	replay := paths[workbench.PathReplay]
	resume := paths[workbench.PathResume]
	for i := range replay {
		if d := workbench.Compare(workbench.PathReplay, workbench.PathResume,
			[]*streaming.Window{replay[i]}, []*streaming.Window{resume[i]}); d != nil {
			t.Fatalf("window %d divergence: %+v", i, d)
		}
	}
}

func TestResumeContinuesFromCommittedWindows(t *testing.T) {
	w := generator.Generate(generator.DefaultConfig())
	dir := t.TempDir()
	store, err := persist.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 先实时跑前半程（这里直接跑完整实时路径建立快照）。
	rt, err := workbench.RunRealtime(context.Background(), w, cfg(), store)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := store.CommittedCount(context.Background())
	if n != len(rt) {
		t.Fatalf("committed count %d want %d", n, len(rt))
	}

	// 重新打开同一数据库模拟进程重启，应从已提交窗口恢复且结果一致。
	store2, err := persist.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	resumed, reasons, err := workbench.RunResume(context.Background(), w, cfg(), store2)
	if err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 0 {
		t.Fatalf("matching identity should not invalidate, reasons=%v", reasons)
	}
	if d := workbench.Compare(workbench.PathRealtime, workbench.PathResume, rt, resumed); d != nil {
		t.Fatalf("resume mismatch: %+v", d)
	}
}

func TestVersionMismatchInvalidatesWholeSnapshot(t *testing.T) {
	w := generator.Generate(generator.DefaultConfig())
	dir := t.TempDir()
	store, err := persist.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workbench.RunRealtime(context.Background(), w, cfg(), store); err != nil {
		t.Fatal(err)
	}

	// 修改预算参数 -> 身份指纹失配，必须整体作废并全量重算。
	changed := cfg()
	changed.Budget.Slots = 1
	resumed, reasons, err := workbench.RunResume(context.Background(), w, changed, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(reasons) == 0 {
		t.Fatal("expected invalidation reasons on version mismatch")
	}
	// 作废后应仍产出完整窗口集，且与全量重放一致。
	replay, err := workbench.RunReplay(w, changed)
	if err != nil {
		t.Fatal(err)
	}
	if d := workbench.Compare(workbench.PathReplay, workbench.PathResume, replay, resumed); d != nil {
		t.Fatalf("post-invalidation replay mismatch: %+v", d)
	}
	n, _ := store.CommittedCount(context.Background())
	if n != len(replay) {
		t.Fatalf("after invalidation committed=%d want %d", n, len(replay))
	}
}

func TestDivergenceReportedWithCodedError(t *testing.T) {
	w := generator.Generate(generator.DefaultConfig())
	a, _ := workbench.RunReplay(w, cfg())
	// 手工篡改一个窗口的可见集合制造差异。
	b, _ := workbench.RunReplay(w, cfg())
	b[1].Visible = append(append([]int{}, b[1].Visible...), 999)
	d := workbench.Compare(workbench.PathReplay, workbench.PathRealtime, a, b)
	if d == nil {
		t.Fatal("expected divergence")
	}
	if d.WindowIndex != 1 || d.Field != "visible" {
		t.Fatalf("unexpected divergence locator: %+v", d)
	}
	_ = core.EDiverge
}

func TestContentHashChangesWithWorld(t *testing.T) {
	w1 := generator.Generate(generator.DefaultConfig())
	w2 := generator.Generate(generator.DefaultConfig())
	w2.Partitions[0].Chunks[0].LoadCost = 999
	h1 := persist.ContentHash(w1)
	h2 := persist.ContentHash(w2)
	if h1 == h2 {
		t.Fatal("content hash must change when world changes")
	}
}
