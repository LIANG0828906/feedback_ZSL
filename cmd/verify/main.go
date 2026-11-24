package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"edgeworkbench/internal/core"

	_ "modernc.org/sqlite"
)

type sampleReport struct {
	Name              string          `json:"name"`
	Width             int             `json:"width"`
	Height            int             `json:"height"`
	Channels          int             `json:"channels"`
	Noisy             bool            `json:"noisy"`
	LowContrast       bool            `json:"lowContrast"`
	Fingerprint       string          `json:"fingerprint"`
	References        int             `json:"references"`
	PreviewElapsedMS  float64         `json:"previewElapsedMs"`
	PreviewFactor     int             `json:"previewFactor"`
	PreviewPass       bool            `json:"previewPass"`
	Divergence        core.Divergence `json:"divergence"`
	RealtimeMS        float64         `json:"realtimeMs"`
	ResumeMS          float64         `json:"resumeMs"`
	FullMS            float64         `json:"fullMs"`
	ThreePathsEqual   bool            `json:"threePathsEqual"`
	ThreePathsDetail  string          `json:"threePathsDetail,omitempty"`
	CheckpointCommits int             `json:"checkpointCommits"`
	CheckpointRestore bool            `json:"checkpointRestore"`
	RestoredCommits   int             `json:"restoredCommits"`
	CorruptionHandled bool            `json:"corruptionHandled"`
	FaultIsolation    bool            `json:"faultIsolation"`
	RetryEqual        bool            `json:"retryEqual"`
	PoolMaxParallel   int             `json:"poolMaxParallel"`
	PoolWaits         map[string]int  `json:"poolWaits"`
	Pass              bool            `json:"pass"`
	Errors            []string        `json:"errors,omitempty"`
}

type report struct {
	GeneratedAt     string         `json:"generatedAt"`
	Algorithm       string         `json:"algorithm"`
	GoVersion       string         `json:"goVersion"`
	Samples         []sampleReport `json:"samples"`
	Pass            bool           `json:"pass"`
	TotalDurationMS int64          `json:"totalDurationMs"`
}

func main() {
	outDir := flag.String("out", filepath.Join("artifacts", "verification"), "report output directory")
	sampleRoot := flag.String("samples", filepath.Join("artifacts", "samples"), "deterministic sample output directory")
	flag.Parse()
	started := time.Now()
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(*sampleRoot, 0o755); err != nil {
		fatal(err)
	}
	rep := report{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Algorithm:   core.AlgorithmVersion,
		GoVersion:   runtime.Version(),
		Pass:        true,
	}
	for _, spec := range core.DefaultSampleSpecs() {
		sr := verifySample(*sampleRoot, spec)
		rep.Samples = append(rep.Samples, sr)
		if !sr.Pass {
			rep.Pass = false
		}
		fmt.Printf("%-26s pass=%-5v preview=%6.1fms factor=%d paths=%v checkpoint=%v fault=%v\n",
			sr.Name, sr.Pass, sr.PreviewElapsedMS, sr.PreviewFactor, sr.ThreePathsEqual, sr.CheckpointRestore, sr.FaultIsolation)
	}
	rep.TotalDurationMS = time.Since(started).Milliseconds()
	raw, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "report.json"), append(raw, '\n'), 0o644); err != nil {
		fatal(err)
	}
	md := renderMarkdown(rep)
	if err := os.WriteFile(filepath.Join(*outDir, "REPORT.md"), []byte(md), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("\nreport: %s\n", filepath.Join(*outDir, "REPORT.md"))
	if !rep.Pass {
		os.Exit(1)
	}
}

func verifySample(root string, spec core.SampleSpec) sampleReport {
	rep := sampleReport{Name: spec.Name, Width: spec.Width, Height: spec.Height, Channels: spec.Channels, Noisy: spec.Noisy, LowContrast: spec.LowContrast}
	fail := func(format string, args ...any) {
		rep.Pass = false
		rep.Errors = append(rep.Errors, fmt.Sprintf(format, args...))
	}
	sample, err := core.GenerateSample(root, spec)
	if err != nil {
		fail("generate: %v", err)
		return rep
	}
	loaded, err := core.LoadSample(sample.Dir)
	if err != nil {
		fail("load: %v", err)
		return rep
	}
	if err := core.ValidateReferences(loaded); err != nil {
		fail("references: %v", err)
	}
	rep.Fingerprint = loaded.Manifest.Fingerprint
	rep.References = len(loaded.Manifest.References)

	params := core.DefaultParams()
	preview, err := core.ComputePreview(loaded.Image, params, loaded.Manifest.Truth)
	if err != nil {
		fail("preview: %v", err)
		return rep
	}
	rep.PreviewElapsedMS = preview.ElapsedMS
	rep.PreviewFactor = preview.Policy.DownsampleFactor
	if preview.ElapsedMS > 100 {
		fail("preview exceeded 100ms: %.2f", preview.ElapsedMS)
	}
	exactStages, err := core.ComputeStages(loaded.Image, params)
	if err != nil {
		fail("exact: %v", err)
		return rep
	}
	exactMask := exactStages[len(exactStages)-1].Image()
	exactStats := core.AnalyzeGeometry(exactMask, loaded.Manifest.Truth)
	div := core.ComparePreviewExact(preview.Mask, exactMask, preview.Stats, exactStats, core.DefaultTolerance())
	rep.Divergence = div
	rep.PreviewPass = div.Pass
	if !div.Pass {
		fail("preview/exact divergence: coverage=%.4f geometry=%.3f pixels=%.4f", div.CoverageDeviation, div.GeometryDeviation, div.PixelDiffFraction)
	}

	dbDir, err := os.MkdirTemp("", "morphpath-"+spec.Name+"-")
	if err != nil {
		fail("temp: %v", err)
		return rep
	}
	defer os.RemoveAll(dbDir)
	store, err := core.OpenCheckpointStore(filepath.Join(dbDir, "checkpoint.db"))
	if err != nil {
		fail("checkpoint open: %v", err)
		return rep
	}
	defer store.Close()
	grid := core.BuildGrid(loaded.Image.Width, loaded.Image.Height, 96, 96)
	base := core.RunOptions{Grid: grid, Workers: 4, PerWorkerPixels: 512 * 1024, Checkpoint: store}
	base.Mode = core.RunRealtime
	realtime, err := core.RunPipeline(context.Background(), loaded.Image, params, loaded.Manifest.Truth, base)
	if err != nil {
		fail("realtime: %v", err)
		return rep
	}
	rep.RealtimeMS = realtime.ElapsedMS
	rep.PoolMaxParallel = realtime.PoolStats.MaxParallel
	rep.PoolWaits = realtime.PoolStats.WaitReasons
	if realtime.Status != "complete" {
		fail("realtime failed: %v", realtime.FailedBlocks)
	}
	rep.CheckpointCommits, err = store.CommitCount(context.Background(), realtime.ImageFingerprint)
	if err != nil {
		fail("commit count: %v", err)
	}
	base.Mode = core.RunResume
	resumed, err := core.RunPipeline(context.Background(), loaded.Image, params, loaded.Manifest.Truth, base)
	if err != nil {
		fail("resume: %v", err)
		return rep
	}
	rep.ResumeMS = resumed.ElapsedMS
	rep.RestoredCommits = resumed.RestoredCommits
	rep.CheckpointRestore = resumed.Status == "complete" && resumed.RestoredCommits == len(core.StageOrder)*len(grid.Blocks)
	if !rep.CheckpointRestore {
		fail("checkpoint restore incomplete: status=%s restored=%d", resumed.Status, resumed.RestoredCommits)
	}
	base.Mode = core.RunFull
	base.Checkpoint = nil
	full, err := core.RunPipeline(context.Background(), loaded.Image, params, loaded.Manifest.Truth, base)
	if err != nil {
		fail("full: %v", err)
		return rep
	}
	rep.FullMS = full.ElapsedMS
	if err := core.CompareRuns(realtime, resumed); err != nil {
		fail("realtime vs resume: %v", err)
		rep.ThreePathsDetail = err.Error()
	} else if err := core.CompareRuns(realtime, full); err != nil {
		fail("realtime vs full: %v", err)
		rep.ThreePathsDetail = err.Error()
	} else {
		rep.ThreePathsEqual = true
	}

	corruptionTest := corruptCheckpoint(store, realtime.ImageFingerprint, loaded, grid)
	rep.CorruptionHandled = corruptionTest
	if !corruptionTest {
		fail("checkpoint corruption was not invalidated")
	}

	faultKey := core.CommitKey{Stage: core.StageThin, Block: min(2, len(grid.Blocks)-1)}
	faultOpts := core.RunOptions{
		Mode: core.RunFull, Grid: grid, Workers: 2, PerWorkerPixels: 128 * 1024,
		Faults: map[core.CommitKey]string{faultKey: "verification isolated fault"},
	}
	faulted, err := core.RunPipeline(context.Background(), loaded.Image, params, loaded.Manifest.Truth, faultOpts)
	if err != nil {
		fail("fault run: %v", err)
	} else {
		rep.FaultIsolation = faultIsolated(faulted, faultKey)
		if !rep.FaultIsolation {
			fail("fault isolation failed")
		}
	}
	faultOpts.Faults = nil
	retried, err := core.RunPipeline(context.Background(), loaded.Image, params, loaded.Manifest.Truth, faultOpts)
	if err != nil {
		fail("retry: %v", err)
	} else if err := core.CompareRuns(full, retried); err != nil {
		fail("retry comparison: %v", err)
	} else {
		rep.RetryEqual = true
	}
	rep.Pass = len(rep.Errors) == 0
	return rep
}

func corruptCheckpoint(store *core.CheckpointStore, imageFP string, sample core.LoadedSample, grid core.Grid) bool {
	db, err := sql.Open("sqlite", "file:"+store.Path()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return false
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE snapshots SET content_hash='verify-corruption' WHERE commit_seq=(SELECT MIN(commit_seq) FROM snapshots WHERE image_fp=?)`, imageFP); err != nil {
		return false
	}
	snapshot, err := store.LoadSnapshot(context.Background(), imageFP, core.DefaultParams(), core.AlgorithmVersion, sample.Image.Width, sample.Image.Height, sample.Image.Channels, grid)
	return err == nil && snapshot != nil && snapshot.Invalidated
}

func faultIsolated(run core.RunResult, key core.CommitKey) bool {
	for _, matrix := range run.BlockStates {
		if matrix.Stage != key.Stage {
			continue
		}
		if key.Block < 0 || key.Block >= len(matrix.States) || matrix.States[key.Block] != core.BlockFailed {
			return false
		}
		for i, state := range matrix.States {
			if i != key.Block && state != core.BlockCommitted {
				return false
			}
		}
		return true
	}
	return false
}

func renderMarkdown(rep report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# MorphEdge Studio 验收报告\n\n")
	fmt.Fprintf(&b, "- 生成时间：`%s`\n- 算法版本：`%s`\n- 总耗时：`%d ms`\n- 总体结论：**%s**\n\n", rep.GeneratedAt, rep.Algorithm, rep.TotalDurationMS, passText(rep.Pass))
	fmt.Fprintf(&b, "| 样本 | 分辨率 | 通道 | 噪声/低对比 | 预览 ms | factor | 预览一致 | 三路径一致 | 恢复块 | 损坏隔离 | 故障隔离 | 结论 |\n")
	fmt.Fprintf(&b, "|---|---:|---:|---|---:|---:|---|---|---:|---|---|---|\n")
	for _, s := range rep.Samples {
		fmt.Fprintf(&b, "| %s | %d×%d | %d | %s/%s | %.2f | %d | %s | %s | %d | %s | %s | **%s** |\n",
			s.Name, s.Width, s.Height, s.Channels, boolText(s.Noisy), boolText(s.LowContrast),
			s.PreviewElapsedMS, s.PreviewFactor, passText(s.PreviewPass), passText(s.ThreePathsEqual),
			s.RestoredCommits, passText(s.CorruptionHandled), passText(s.FaultIsolation), passText(s.Pass))
	}
	b.WriteString("\n## 一致性指标\n\n")
	for _, s := range rep.Samples {
		fmt.Fprintf(&b, "### %s\n\n", s.Name)
		fmt.Fprintf(&b, "- 内容指纹：`%s`\n", s.Fingerprint)
		fmt.Fprintf(&b, "- 预览降采样：`%d×`；覆盖率偏差：`%.6f`；几何偏差：`%.3f px`；逐像素差异：`%.6f`\n", s.PreviewFactor, s.Divergence.CoverageDeviation, s.Divergence.GeometryDeviation, s.Divergence.PixelDiffFraction)
		if s.Divergence.FirstMismatch != nil {
			fmt.Fprintf(&b, "- 首差异像素：`(%d,%d,c%d)`\n", s.Divergence.FirstMismatch.X, s.Divergence.FirstMismatch.Y, s.Divergence.FirstMismatch.Channel)
		}
		fmt.Fprintf(&b, "- 实时/恢复/全量：`%.1f / %.1f / %.1f ms`；检查点提交：`%d`\n", s.RealtimeMS, s.ResumeMS, s.FullMS, s.CheckpointCommits)
		fmt.Fprintf(&b, "- 并行峰值：`%d`；缓冲/线程等待：`%v`\n", s.PoolMaxParallel, s.PoolWaits)
		if len(s.Errors) > 0 {
			fmt.Fprintf(&b, "- 错误：`%s`\n", strings.Join(s.Errors, "; "))
		}
	}
	b.WriteString("\n## 复现命令\n\n```bash\ngo test ./...\ngo run ./cmd/verify\n```\n")
	return b.String()
}

func boolText(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

func passText(v bool) string {
	if v {
		return "PASS"
	}
	return "FAIL"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
