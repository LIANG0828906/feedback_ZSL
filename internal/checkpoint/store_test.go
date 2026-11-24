package checkpoint

import (
	"context"
	"path/filepath"
	"testing"

	"audiobench/internal/model"
	"audiobench/internal/window"
)

func TestCommitLoadAndInvalidate(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "cp.db")
	store, err := Open(ctx, dbPath, "mat_x")
	if err != nil {
		t.Fatalf("打开 SQLite 失败: %v", err)
	}
	defer store.Close()

	p := model.DefaultParams()
	meta := Meta{
		MaterialID: "mat_x", ContentHash: "hash-1",
		ParamsVersion: ParamsVersion(p), AlgorithmVersion: model.AlgorithmVersion,
		WindowFrames: 8, TotalFrames: 20,
	}
	if err := store.InitMeta(ctx, meta); err != nil {
		t.Fatal(err)
	}
	w0 := window.Window{Index: 0, StartFrame: 0, EndFrame: 8,
		State: window.StateCommitted, Params: p,
		Fingerprints: []model.Fingerprint{{Frame: 0, Quantized: []int{1, 2}}}}
	if err := store.CommitWindow(ctx, w0); err != nil {
		t.Fatalf("提交窗口失败: %v", err)
	}
	got, err := store.LoadWindows(ctx)
	if err != nil || len(got) != 1 || got[0].Index != 0 {
		t.Fatalf("恢复窗口异常: %v %v", got, err)
	}

	// 身份不匹配 → 整体作废。
	if err := store.Invalidate(ctx); err != nil {
		t.Fatal(err)
	}
	if meta2, _ := store.LoadMeta(ctx); meta2 != nil {
		t.Fatal("作废后 meta 应清空")
	}
	if rows, _ := store.LoadWindows(ctx); len(rows) != 0 {
		t.Fatalf("作废后窗口应清空，实际 %d", len(rows))
	}
}

func TestMetaMatch(t *testing.T) {
	p := model.DefaultParams()
	a := Meta{MaterialID: "m", ContentHash: "h", ParamsVersion: ParamsVersion(p),
		AlgorithmVersion: model.AlgorithmVersion, WindowFrames: 8, TotalFrames: 20}
	b := a
	if !MatchMeta(&a, &b) {
		t.Fatal("相同 meta 应匹配")
	}
	b.ContentHash = "changed"
	if MatchMeta(&a, &b) {
		t.Fatal("内容哈希变化必须不匹配")
	}
}
