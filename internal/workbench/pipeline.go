// Package workbench 编排实时逐窗口、断点恢复与一次性全量重放三条执行路径，
// 并对结果做逐窗口自动比对，任何不一致都返回 E_DIVERGE 并定位首个差异。
package workbench

import (
	"context"
	"reflect"

	"occlbench/internal/core"
	"occlbench/internal/culling"
	"occlbench/internal/persist"
	"occlbench/internal/scene"
	"occlbench/internal/streaming"
)

// PathName 三条执行路径名称。
type PathName string

const (
	PathRealtime PathName = "realtime" // 实时逐窗口
	PathResume   PathName = "resume"   // 断点恢复
	PathReplay   PathName = "replay"   // 一次性全量重放
)

// Divergence 首个不一致窗口的定位信息。
type Divergence struct {
	WindowIndex int    `json:"windowIndex"`
	ChunkID     int    `json:"chunkId"`
	Field       string `json:"field"`
	PathA       string `json:"pathA"`
	PathB       string `json:"pathB"`
	ValueA      any    `json:"valueA"`
	ValueB      any    `json:"valueB"`
}

// RunReplay 一次性全量重放（不读写检查点），作为参考基线。
func RunReplay(w *scene.World, cfg streaming.Config) ([]*streaming.Window, error) {
	eng, err := streaming.NewEngine(w, cfg)
	if err != nil {
		return nil, err
	}
	return eng.RunAll()
}

// RunRealtime 实时逐窗口：每提交一个窗口即串行写入 SQLite，模拟可暂停/退出的真实加载。
func RunRealtime(ctx context.Context, w *scene.World, cfg streaming.Config, store *persist.Store) ([]*streaming.Window, error) {
	id := persist.BuildIdentity(w, cfg.CullParams, cfg.Budget.Slots, cfg.Budget.Memory, cfg.WindowSize)
	// 实时路径负责建立全新且版本匹配的快照。
	if err := store.Reset(ctx); err != nil {
		return nil, err
	}
	if err := store.SaveIdentity(ctx, id); err != nil {
		return nil, err
	}

	eng, err := streaming.NewEngine(w, cfg)
	if err != nil {
		return nil, err
	}
	cullResult, err := culling.NewEngine().Run(w, cfg.CullParams)
	if err != nil {
		return nil, err
	}
	windows := buildWindowsFor(w, cfg, cullResult.Samples)
	resident := []int{}
	for _, win := range windows {
		if err := stepWindow(eng, win, w, cfg, &resident, cullResult.Samples); err != nil {
			return windows, err
		}
		if err := store.CommitWindow(ctx, win); err != nil {
			return windows, err
		}
	}
	return windows, nil
}

// RunResume 断点恢复：校验身份完全匹配后从已提交窗口之后继续；
// 任一版本字段失配则整体作废并全量重算，绝不部分混用旧窗口。
func RunResume(ctx context.Context, w *scene.World, cfg streaming.Config, store *persist.Store) ([]*streaming.Window, []string, error) {
	want := persist.BuildIdentity(w, cfg.CullParams, cfg.Budget.Slots, cfg.Budget.Memory, cfg.WindowSize)
	got, ok, err := store.LoadIdentity(ctx)
	invalidateReasons := []string{}
	if err != nil {
		return nil, invalidateReasons, err
	}
	invalidated := false
	if !ok {
		invalidateReasons = append(invalidateReasons, "no-identity")
		invalidated = true
	} else if diffs := got.DiffFields(want); len(diffs) > 0 {
		invalidateReasons = append(invalidateReasons, diffs...)
		invalidated = true
	}
	if invalidated {
		// 整体作废：清空全部旧窗口后按实时路径全量重算。
		if err := store.Reset(ctx); err != nil {
			return nil, invalidateReasons, err
		}
		windows, err := RunRealtime(ctx, w, cfg, store)
		return windows, invalidateReasons, err
	}

	// 身份匹配：载入已提交窗口，仅从其后的未提交窗口继续。
	committed, err := store.LoadWindows(ctx)
	if err != nil {
		return nil, invalidateReasons, err
	}
	eng, err := streaming.NewEngine(w, cfg)
	if err != nil {
		return nil, invalidateReasons, err
	}
	cullResult, err := culling.NewEngine().Run(w, cfg.CullParams)
	if err != nil {
		return nil, invalidateReasons, err
	}
	allWindows := buildWindowsFor(w, cfg, cullResult.Samples)

	// 用已提交窗口恢复驻留集合，并校验顺序连续。
	resident := []int{}
	for i, saved := range committed {
		if i >= len(allWindows) || saved.Index != allWindows[i].Index {
			return nil, invalidateReasons, core.NewError(core.EDiverge, "checkpoint window sequence mismatch",
				map[string]any{"expected": i, "got": saved.Index})
		}
		frozen := *saved
		allWindows[i] = &frozen
		resident = append(resident[:0], saved.Resident...)
	}

	for i := len(committed); i < len(allWindows); i++ {
		win := allWindows[i]
		if err := stepWindow(eng, win, w, cfg, &resident, cullResult.Samples); err != nil {
			return allWindows, invalidateReasons, err
		}
		if err := store.CommitWindow(ctx, win); err != nil {
			return allWindows, invalidateReasons, err
		}
	}
	return allWindows, invalidateReasons, nil
}

// Compare 逐窗口比对两条路径结果，返回首个不一致；完全一致返回 nil。
func Compare(pathA, pathB PathName, a, b []*streaming.Window) *Divergence {
	if len(a) != len(b) {
		return &Divergence{WindowIndex: minLen(a, b), Field: "windowCount",
			PathA: string(pathA), PathB: string(pathB), ValueA: len(a), ValueB: len(b)}
	}
	for i := range a {
		if d := compareWindow(pathA, pathB, a[i], b[i]); d != nil {
			return d
		}
	}
	return nil
}

func compareWindow(pathA, pathB PathName, a, b *streaming.Window) *Divergence {
	if d := compareIntField(pathA, pathB, a.Index, "visible", a.Visible, b.Visible); d != nil {
		return d
	}
	if d := compareIntField(pathA, pathB, a.Index, "occluded", a.Occluded, b.Occluded); d != nil {
		return d
	}
	if d := compareIntField(pathA, pathB, a.Index, "loadList", a.LoadList, b.LoadList); d != nil {
		return d
	}
	if d := compareIntField(pathA, pathB, a.Index, "unloadList", a.UnloadList, b.UnloadList); d != nil {
		return d
	}
	if d := compareIntField(pathA, pathB, a.Index, "resident", a.Resident, b.Resident); d != nil {
		return d
	}
	if !reflect.DeepEqual(a.Delayed, b.Delayed) {
		return &Divergence{WindowIndex: a.Index, Field: "delayed",
			PathA: string(pathA), PathB: string(pathB), ValueA: a.Delayed, ValueB: b.Delayed}
	}
	return nil
}

func compareIntField(pathA, pathB PathName, winIdx int, field string, a, b []int) *Divergence {
	if reflect.DeepEqual(a, b) {
		return nil
	}
	chunk := firstDiffChunk(a, b)
	return &Divergence{WindowIndex: winIdx, ChunkID: chunk, Field: field,
		PathA: string(pathA), PathB: string(pathB), ValueA: a, ValueB: b}
}

func firstDiffChunk(a, b []int) int {
	set := map[int]bool{}
	for _, x := range a {
		set[x] = true
	}
	for _, y := range b {
		if !set[y] {
			return y
		}
	}
	if len(a) > 0 {
		return a[0]
	}
	return -1
}

func minLen(a, b []*streaming.Window) int {
	if len(a) < len(b) {
		return len(a)
	}
	return len(b)
}

// VerifyThreePaths 运行三条路径并两两比对，任一不一致返回 E_DIVERGE。
func VerifyThreePaths(ctx context.Context, w *scene.World, cfg streaming.Config, store *persist.Store) (map[PathName][]*streaming.Window, *Divergence, error) {
	replay, err := RunReplay(w, cfg)
	if err != nil {
		return nil, nil, err
	}
	realtime, err := RunRealtime(ctx, w, cfg, store)
	if err != nil {
		return nil, nil, err
	}
	resume, _, err := RunResume(ctx, w, cfg, store)
	if err != nil {
		return nil, nil, err
	}
	paths := map[PathName][]*streaming.Window{
		PathReplay:   replay,
		PathRealtime: realtime,
		PathResume:   resume,
	}
	if d := Compare(PathReplay, PathRealtime, replay, realtime); d != nil {
		return paths, d, core.NewError(core.EDiverge, "replay vs realtime mismatch", divergenceFields(d))
	}
	if d := Compare(PathRealtime, PathResume, realtime, resume); d != nil {
		return paths, d, core.NewError(core.EDiverge, "realtime vs resume mismatch", divergenceFields(d))
	}
	if d := Compare(PathReplay, PathResume, replay, resume); d != nil {
		return paths, d, core.NewError(core.EDiverge, "replay vs resume mismatch", divergenceFields(d))
	}
	return paths, nil, nil
}

func divergenceFields(d *Divergence) map[string]any {
	return map[string]any{
		"window": d.WindowIndex, "chunk": d.ChunkID, "field": d.Field,
		"pathA": d.PathA, "pathB": d.PathB,
	}
}
