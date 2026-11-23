// demo 演示三阶段正射校正链：全量/增量重算、真值校验、
// 快照持久化与异常链路。仅使用本地确定性样本。
package main

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"

	"ortho"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "demo failed:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	outDir := "out"
	cacheDir := filepath.Join(outDir, "cache")
	fullDir := filepath.Join(outDir, "cache_full")

	fmt.Println("== 1. 生成确定性样本 (seed=42) ==")
	set, err := ortho.GenerateSamples("samples")
	if err != nil {
		return err
	}
	fmt.Println("  gray:", set.GrayPath)
	fmt.Println("  rgb: ", set.RGBPath)
	fmt.Println("  params:", set.ParamsPath)
	fmt.Println("  truth: ", set.TruthPath)

	params, err := ortho.LoadStageParams(set.ParamsPath)
	if err != nil {
		return err
	}
	truth, err := ortho.LoadTruth(set.TruthPath)
	if err != nil {
		return err
	}
	src, derr := ortho.LoadImageFile(set.RGBPath)
	if derr != nil {
		return derr
	}
	srcHash := ortho.ContentHash(src)
	fmt.Printf("  source: %dx%d channels=%d hash=%s...\n",
		src.Bounds().Dx(), src.Bounds().Dy(), ortho.Channels(src), srcHash[:16])

	rcfg := ortho.ResampleConfig{
		InterpOrder:  params.InterpOrder,
		TileSize:     params.TileSize,
		Band:         params.Band,
		Workers:      4,
		BufferPixels: 1 << 20,
	}

	fmt.Println("\n== 2. 全量运行（空缓存） ==")
	stages := ortho.BuildStages(params)
	pipe := ortho.NewPipeline(stages, rcfg, nil)
	res, perr := pipe.Run(ctx, src, srcHash)
	if perr != nil {
		return perr
	}
	printReports(res.Reports)
	fmt.Println("  commit order:", strings.Join(res.CommitOrder, " -> "))

	fmt.Println("\n== 3. 真值几何校验 ==")
	geom, gerr := ortho.CheckGeometry(stages, truth, params.Tolerance)
	for _, g := range geom {
		fmt.Printf("  stage=%-10s points=%d median=%.6f max=%.6f tol=%.3f\n",
			g.Stage, g.Count, g.Median, g.Max, params.Tolerance)
	}
	if gerr != nil {
		return gerr
	}
	fmt.Println("  geometry check: PASS")

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	for _, st := range stages {
		p := filepath.Join(outDir, "stage_"+st.ID()+".png")
		if err := os.WriteFile(p, mustPNG(res.Outputs[st.ID()]), 0o644); err != nil {
			return err
		}
		fmt.Println("  wrote", p)
	}
	overlay := ortho.DrawControlPoints(res.Final, truth.Stages["affine"])
	if err := os.WriteFile(filepath.Join(outDir, "overlay_control_points.png"), mustPNG(overlay), 0o644); err != nil {
		return err
	}
	fmt.Println("  wrote", filepath.Join(outDir, "overlay_control_points.png"))

	fmt.Println("\n== 4. 快照持久化与重启恢复 ==")
	store := ortho.NewStore(cacheDir)
	snap := ortho.BuildSnapshot(pipe.Version(), srcHash, stages, pipe.Cache)
	if err := store.Save(snap, ortho.CollectImages(stages, pipe.Cache)); err != nil {
		return err
	}
	fmt.Println("  snapshot saved:", filepath.Join(cacheDir, "snapshot.json"))

	// 模拟重启：全新缓存，从快照恢复。
	pipe2 := ortho.NewPipeline(stages, rcfg, nil)
	loaded, imgs, err := store.Load(pipe2.Version(), srcHash)
	if err != nil {
		return err
	}
	if loaded == nil {
		return fmt.Errorf("expected snapshot to load")
	}
	ortho.RestoreCache(loaded, imgs, pipe2.Cache)
	res2, perr := pipe2.Run(ctx, src, srcHash)
	if perr != nil {
		return perr
	}
	printReports(res2.Reports)
	fmt.Println("  restart: all stages reused from persisted snapshot")

	fmt.Println("\n== 5. 增量更新：调整径向畸变 K1 ==")
	params2 := *params
	params2.Radial.K1 = 0.20
	stages2 := ortho.BuildStages(&params2)
	pipe3 := ortho.NewPipeline(stages2, rcfg, pipe2.Cache)
	res3, perr := pipe3.Run(ctx, src, srcHash)
	if perr != nil {
		return perr
	}
	printReports(res3.Reports)
	fmt.Println("  commit order:", strings.Join(res3.CommitOrder, " -> "))

	fmt.Println("\n== 6. 参数偏离真值时的几何校验（预期 E_GEOM） ==")
	_, gerr = ortho.CheckGeometry(stages2, truth, params.Tolerance)
	if gerr != nil {
		fmt.Println("  got expected error:", gerr)
	} else {
		return fmt.Errorf("expected E_GEOM for tampered K1")
	}

	fmt.Println("\n== 7. 恢复参数后：增量 vs 全量逐像素比对 ==")
	pipeInc := ortho.NewPipeline(stages, rcfg, pipe2.Cache)
	resA, perr := pipeInc.Run(ctx, src, srcHash)
	if perr != nil {
		return perr
	}
	printReports(resA.Reports)

	_ = fullDir // 全量路径使用全新空缓存（无快照）
	pipeFull := ortho.NewPipeline(ortho.BuildStages(params), rcfg, nil)
	resB, perr := pipeFull.Run(ctx, src, srcHash)
	if perr != nil {
		return perr
	}
	order := []string{"homography", "radial", "affine"}
	if derr := ortho.CompareRuns(order, resA.Outputs, resB.Outputs); derr != nil {
		return derr
	}
	fmt.Println("  incremental == full recompute: pixel-identical OK")
	heat, hasDiff := ortho.Heatmap(resA.Final, resB.Final)
	if err := os.WriteFile(filepath.Join(outDir, "heatmap_diff.png"), mustPNG(heat), 0o644); err != nil {
		return err
	}
	fmt.Printf("  wrote %s (diff=%v)\n", filepath.Join(outDir, "heatmap_diff.png"), hasDiff)

	fmt.Println("\n== 8. 并行 vs 串行一致性 ==")
	serCfg := rcfg
	serCfg.Workers = 1
	pipeSer := ortho.NewPipeline(ortho.BuildStages(params), serCfg, nil)
	resS, perr := pipeSer.Run(ctx, src, srcHash)
	if perr != nil {
		return perr
	}
	if derr := ortho.CompareRuns(order, resB.Outputs, resS.Outputs); derr != nil {
		return derr
	}
	fmt.Println("  parallel(workers=4) == serial(workers=1): pixel-identical OK")
	fmt.Println("  parallel commit order:", strings.Join(resB.CommitOrder, " -> "))
	fmt.Println("  serial   commit order:", strings.Join(resS.CommitOrder, " -> "))
	fmt.Println("  buffer wait events (parallel run):", pipeFull.WaitLog.Len())

	tinyCfg := rcfg
	tinyCfg.BufferPixels = rcfg.TileSize * rcfg.TileSize * 3 * 2 // 仅容纳 2 个块缓冲
	pipeTiny := ortho.NewPipeline(ortho.BuildStages(params), tinyCfg, nil)
	resT, perr := pipeTiny.Run(ctx, src, srcHash)
	if perr != nil {
		return perr
	}
	if derr := ortho.CompareRuns(order, resB.Outputs, resT.Outputs); derr != nil {
		return derr
	}
	fmt.Println("  tiny-buffer run: pixel-identical OK, buffer wait events:", pipeTiny.WaitLog.Len())

	fmt.Println("\n== 9. 版本不匹配时快照整体作废 ==")
	snap2, _, err := store.Load("homography/v9|radial/v9|affine/v9", srcHash)
	if err != nil {
		return err
	}
	if snap2 != nil {
		return fmt.Errorf("expected snapshot to be discarded on version mismatch")
	}
	entries, _ := os.ReadDir(cacheDir)
	fmt.Printf("  version mismatch -> snapshot discarded, cache dir entries=%d (full recompute required)\n", len(entries))

	fmt.Println("\n== 10. 异常链路演示 ==")
	bad := *params
	bad.Affine.Sx = 0.001
	if _, perr := ortho.NewPipeline(ortho.BuildStages(&bad), rcfg, nil).Run(ctx, src, srcHash); perr != nil {
		fmt.Println("  out-of-range param ->", perr)
	}
	if _, derr := ortho.DecodeImage(strings.NewReader("not a png")); derr != nil {
		fmt.Println("  corrupt input     ->", derr)
	}
	bandCfg := rcfg
	bandCfg.Band = 0
	if _, perr := ortho.NewPipeline(stages, bandCfg, nil).Run(ctx, src, srcHash); perr != nil {
		fmt.Println("  insufficient band ->", perr)
	}

	fmt.Println("\n== 完成：各阶段图像与报告见 out/ 目录 ==")
	return nil
}

func printReports(reps []ortho.StageReport) {
	for _, r := range reps {
		state := "reused"
		if r.Invalidated {
			state = "invalidated(" + r.Reason + ")"
		}
		fmt.Printf("  stage=%-10s key=%s... status=%-10s %s\n",
			r.Stage, r.Key[:12], r.Status, state)
	}
}

func mustPNG(img image.Image) []byte {
	b, err := ortho.EncodePNG(img)
	if err != nil {
		panic(err)
	}
	return b
}
