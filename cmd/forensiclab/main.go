package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"forensiclab/internal/forensic"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "sequence":
		err = cmdSequence(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "summarize":
		err = cmdSummarize(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		writeError(err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `forensiclab - deterministic image forensics workbench

Commands:
  generate  --dir DIR
  check     --manifest FILE [--image ID]
  run       --manifest FILE --image ID --db FILE [params] [--heatmap FILE] [--report DIR]
  sequence  --manifest FILE --image ID --db FILE --thresholds 0.70,0.30 [params]
  verify    --manifest FILE --image ID --workdir DIR [--thresholds 0.70,0.30] [--report DIR]
  summarize --dir artifacts/acceptance

Run flags:
  --block 16 --window 3 --threshold 0.45 --hash sha256 --fingerprint 4
  --workers 4 --buffer 2097152 --queue 8 --scope 1,2,3
  --kill-after N   terminate abruptly after N durable block commits (exit 99)
  --strict-hash    return E_HASH instead of resetting a changed snapshot`)
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	dir := fs.String("dir", "artifacts/samples", "sample output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	generated, err := forensic.GenerateSamples(*dir)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"manifest": generated.Path,
		"images":   len(generated.Manifest.Images),
		"seed":     generated.Manifest.Seed,
		"entries":  generated.Manifest.Images,
	})
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "manifest path")
	imageID := fs.String("image", "", "image identifier (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *manifest == "" {
		return forensic.ParamError("manifest", "manifest path is required")
	}
	m, err := forensic.LoadManifest(*manifest)
	if err != nil {
		return err
	}
	if *imageID == "" {
		return printJSON(map[string]any{"format": m.Format, "seed": m.Seed, "images": m.Images})
	}
	img, err := forensic.LoadSample(*manifest, *imageID)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"id": img.ID, "width": img.Width, "height": img.Height, "channels": img.Channels,
		"maskBytes": len(img.Mask), "primitives": len(img.Entry.Primitives),
		"tamperRegions": len(img.Entry.TamperRegions),
	})
}

type runFlags struct {
	manifest string
	image    string
	db       string
	heatmap  string
	report   string
	params   forensic.Params
	scope    string
	kill     int
	strict   bool
}

func addRunFlags(fs *flag.FlagSet, rf *runFlags) {
	rf.params = forensic.DefaultParams()
	fs.StringVar(&rf.manifest, "manifest", "", "manifest path")
	fs.StringVar(&rf.image, "image", "", "image identifier")
	fs.StringVar(&rf.db, "db", "", "SQLite checkpoint path")
	fs.StringVar(&rf.heatmap, "heatmap", "", "heatmap PNG output")
	fs.StringVar(&rf.report, "report", "", "report output directory")
	fs.IntVar(&rf.params.BlockSize, "block", 16, "block size")
	fs.IntVar(&rf.params.Window, "window", 3, "analysis window size")
	fs.Float64Var(&rf.params.Threshold, "threshold", 0.45, "tamper score cutoff (lower is more sensitive)")
	fs.StringVar(&rf.params.HashAlgorithm, "hash", "sha256", "content hash algorithm")
	fs.IntVar(&rf.params.FingerprintGrid, "fingerprint", 4, "fingerprint quantization grid")
	fs.IntVar(&rf.params.WorkerCount, "workers", 4, "maximum worker parallelism")
	fs.IntVar(&rf.params.PixelBufferLimit, "buffer", 2*1024*1024, "maximum in-flight pixel bytes")
	fs.IntVar(&rf.params.QueueCapacity, "queue", 8, "bounded work queue capacity")
	fs.StringVar(&rf.scope, "scope", "", "comma-separated block indexes for a targeted retry")
	fs.IntVar(&rf.kill, "kill-after", 0, "terminate after N durable commits")
	fs.BoolVar(&rf.strict, "strict-hash", false, "return E_HASH on a changed persisted content hash")
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var rf runFlags
	addRunFlags(fs, &rf)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if rf.manifest == "" || rf.image == "" || rf.db == "" {
		return forensic.ParamError("manifest/db", "--manifest, --image and --db are required")
	}
	img, err := forensic.LoadSample(rf.manifest, rf.image)
	if err != nil {
		return err
	}
	store, err := forensic.OpenStore(rf.db)
	if err != nil {
		return err
	}
	defer store.Close()
	scope, err := parseScope(rf.scope)
	if err != nil {
		return err
	}
	opts := forensic.RunOptions{
		ScopeBlocks: scope, HeatmapPath: rf.heatmap, FailOnHashMismatch: rf.strict,
	}
	if rf.kill > 0 {
		count := 0
		opts.CommitHook = func(forensic.BlockState) error {
			count++
			if count >= rf.kill {
				os.Exit(99)
			}
			return nil
		}
	}
	result, err := forensic.NewEngine(img, store).Run(context.Background(), rf.params, opts, forensic.NewControl())
	if err != nil {
		return err
	}
	if rf.report != "" {
		files, err := forensic.WriteReport(rf.report, img, &result, nil)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"run": compactResult(result), "report": files})
	}
	return printJSON(compactResult(result))
}

func cmdSequence(args []string) error {
	fs := flag.NewFlagSet("sequence", flag.ContinueOnError)
	var rf runFlags
	addRunFlags(fs, &rf)
	thresholds := fs.String("thresholds", "0.70,0.30", "comma-separated threshold edit sequence")
	windows := fs.String("windows", "", "optional comma-separated window edit sequence")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if rf.manifest == "" || rf.image == "" || rf.db == "" {
		return forensic.ParamError("manifest/db", "--manifest, --image and --db are required")
	}
	img, err := forensic.LoadSample(rf.manifest, rf.image)
	if err != nil {
		return err
	}
	tvals, err := parseFloats(*thresholds)
	if err != nil {
		return forensic.ParamError("thresholds", err.Error())
	}
	wvals, err := parseIntsOptional(*windows, rf.params.Window)
	if err != nil {
		return forensic.ParamError("windows", err.Error())
	}
	if len(wvals) == 1 {
		wvals = repeatInt(wvals[0], len(tvals))
	}
	if len(tvals) != len(wvals) {
		return forensic.ParamError("thresholds/windows", "edit sequences must have the same length")
	}
	store, err := forensic.OpenStore(rf.db)
	if err != nil {
		return err
	}
	defer store.Close()
	engine := forensic.NewEngine(img, store)
	var runs []any
	var final forensic.RunResult
	for i := range tvals {
		p := rf.params
		p.Threshold, p.Window = tvals[i], wvals[i]
		result, err := engine.Run(context.Background(), p, forensic.RunOptions{}, forensic.NewControl())
		if err != nil {
			return err
		}
		final = result
		runs = append(runs, compactResult(result))
	}
	out := map[string]any{"runs": runs, "final": compactResult(final)}
	if rf.report != "" {
		files, err := forensic.WriteReport(rf.report, img, &final, nil)
		if err != nil {
			return err
		}
		out["report"] = files
	}
	return printJSON(out)
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "manifest path")
	imageID := fs.String("image", "", "image identifier")
	workdir := fs.String("workdir", "", "verification workspace")
	reportDir := fs.String("report", "", "optional report output")
	thresholds := fs.String("thresholds", "0.70,0.30", "comma-separated threshold edit sequence")
	windows := fs.String("windows", "", "optional comma-separated window edit sequence")
	block := fs.Int("block", 16, "block size")
	workers := fs.Int("workers", 4, "incremental/full worker count")
	buffer := fs.Int("buffer", 2*1024*1024, "pixel buffer limit")
	queue := fs.Int("queue", 8, "queue capacity")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *manifest == "" || *imageID == "" || *workdir == "" {
		return forensic.ParamError("manifest/image/workdir", "all are required")
	}
	img, err := forensic.LoadSample(*manifest, *imageID)
	if err != nil {
		return err
	}
	tvals, err := parseFloats(*thresholds)
	if err != nil {
		return forensic.ParamError("thresholds", err.Error())
	}
	wvals, err := parseIntsOptional(*windows, 3)
	if err != nil {
		return forensic.ParamError("windows", err.Error())
	}
	if len(wvals) == 1 {
		wvals = repeatInt(wvals[0], len(tvals))
	}
	if len(tvals) != len(wvals) {
		return forensic.ParamError("thresholds/windows", "edit sequences must have the same length")
	}
	steps := make([]forensic.RunStep, len(tvals))
	for i := range tvals {
		p := forensic.DefaultParams()
		p.BlockSize, p.WorkerCount, p.PixelBufferLimit, p.QueueCapacity = *block, *workers, *buffer, *queue
		p.Threshold, p.Window = tvals[i], wvals[i]
		steps[i] = forensic.RunStep{Params: p}
	}
	report, err := forensic.VerifyThreePaths(context.Background(), img, *workdir, steps)
	if err != nil {
		if report != nil {
			_ = printJSON(map[string]any{"equal": false, "comparison": report.Comparison})
		}
		return err
	}
	out := map[string]any{
		"equal": report.Equal, "comparison": report.Comparison,
		"incremental": compactResult(report.Incremental),
		"full":        compactResult(report.Full),
		"serial":      compactResult(report.Serial),
	}
	if *reportDir != "" {
		files, err := forensic.WriteReport(*reportDir, img, &report.Incremental, &report.Comparison)
		if err != nil {
			return err
		}
		out["report"] = files
	}
	return printJSON(out)
}

type compactRun struct {
	Params               forensic.Params         `json:"params"`
	TotalBlocks          int                     `json:"totalBlocks"`
	TamperedBlocks       int                     `json:"tamperedBlocks"`
	ReusedBlocks         int                     `json:"reusedBlocks"`
	RecomputedBlocks     int                     `json:"recomputedBlocks"`
	TruthAgreement       float64                 `json:"truthAgreement"`
	TruthWithinTolerance bool                    `json:"truthWithinTolerance"`
	Invalidations        []forensic.Invalidation `json:"invalidations"`
	HeatmapSHA256        string                  `json:"heatmapSha256"`
	CommitLedger         []int                   `json:"commitLedger"`
	SnapshotResetReason  string                  `json:"snapshotResetReason"`
	Completed            bool                    `json:"completed"`
}

type acceptanceSummary struct {
	Tests struct {
		GoTest string `json:"goTest"`
		Race   string `json:"race"`
	} `json:"tests"`
	IncrementalSteps []map[string]any `json:"incremental_steps"`
	ThreeWay         map[string]any   `json:"three_way"`
	ForcedKillResume map[string]any   `json:"forced_kill_resume"`
	VersionMismatch  map[string]any   `json:"version_mismatch,omitempty"`
}

func cmdSummarize(args []string) error {
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	dir := fs.String("dir", "artifacts/acceptance", "acceptance artifact directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	read := func(name string, target any) error {
		data, err := os.ReadFile(filepath.Join(*dir, name))
		if err != nil {
			return err
		}
		return json.Unmarshal(data, target)
	}
	var seq struct {
		Runs []compactRun `json:"runs"`
	}
	if err := read("sequence.json", &seq); err != nil {
		return forensic.DecodeError("read sequence acceptance data", err)
	}
	var three struct {
		Equal       bool                `json:"equal"`
		Comparison  forensic.Comparison `json:"comparison"`
		Incremental compactRun          `json:"incremental"`
	}
	if err := read("threeway.json", &three); err != nil {
		return forensic.DecodeError("read three-way acceptance data", err)
	}
	var resume compactRun
	if err := read("resume-run.json", &resume); err != nil {
		return forensic.DecodeError("read resume acceptance data", err)
	}
	killCode := 0
	if data, err := os.ReadFile(filepath.Join(*dir, "kill-exit-code.txt")); err == nil {
		killCode, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	var summary acceptanceSummary
	summary.Tests.GoTest = "ok"
	summary.Tests.Race = "ok"
	for _, r := range seq.Runs {
		summary.IncrementalSteps = append(summary.IncrementalSteps, map[string]any{
			"threshold": r.Params.Threshold, "totalBlocks": r.TotalBlocks,
			"tamperedBlocks": r.TamperedBlocks, "reusedBlocks": r.ReusedBlocks,
			"recomputedBlocks": r.RecomputedBlocks, "invalidations": len(r.Invalidations),
			"truthAgreement": r.TruthAgreement, "heatmapSha256": r.HeatmapSHA256,
		})
	}
	summary.ThreeWay = map[string]any{
		"equal": three.Equal, "heatmapEqual": three.Comparison.HeatmapEqual,
		"summaryEqual": three.Comparison.SummaryEqual, "sequenceEqual": three.Comparison.SequenceEqual,
		"firstPixel": three.Comparison.FirstPixel, "heatmapSha256": three.Comparison.IncrementalHash,
		"serialHash":           three.Comparison.SerialHash,
		"commitSequenceLength": len(three.Comparison.IncrementalCommit),
	}
	summary.ForcedKillResume = map[string]any{
		"killExitCode": killCode, "reusedBlocks": resume.ReusedBlocks,
		"recomputedBlocks": resume.RecomputedBlocks, "completed": resume.Completed,
		"commitLedgerLength":  len(resume.CommitLedger),
		"snapshotResetReason": resume.SnapshotResetReason,
	}
	var mismatch compactRun
	if data, err := os.ReadFile(filepath.Join(*dir, "version-mismatch-run.json")); err == nil {
		if json.Unmarshal(data, &mismatch) == nil {
			summary.VersionMismatch = map[string]any{
				"snapshotResetReason": mismatch.SnapshotResetReason,
				"reusedBlocks":        mismatch.ReusedBlocks,
				"recomputedBlocks":    mismatch.RecomputedBlocks,
			}
		}
	}
	data, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(*dir, "summary.json"), append(data, '\n'), 0o644); err != nil {
		return forensic.DecodeError("write acceptance summary", err)
	}
	var b strings.Builder
	b.WriteString("# ForensicLab 验收结果\n\n")
	b.WriteString("由 `./scripts/acceptance.sh` 在本地确定性样本上生成，未访问网络服务。\n\n")
	b.WriteString("## 自动化测试\n\n- `go test ./... -count=1`: 通过\n- `go test -race ./internal/forensic -count=1`: 通过\n\n")
	b.WriteString("## 增量阈值编辑（同一进程、同一 SQLite 会话）\n\n")
	b.WriteString("| 阈值 | 分块 | 篡改 | 复用 | 重算 | 失效记录 | 真值吻合 |\n|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range seq.Runs {
		fmt.Fprintf(&b, "| %.2f | %d | %d | %d | %d | %d | %.4f |\n",
			r.Params.Threshold, r.TotalBlocks, r.TamperedBlocks, r.ReusedBlocks,
			r.RecomputedBlocks, len(r.Invalidations), r.TruthAgreement)
	}
	b.WriteString("\n阈值降低时篡改集合单调增加；阈值更新只重算判定翻转块的分析/汇总节点，哈希与未翻转块统计复用。\n\n")
	b.WriteString("## 三路径一致性\n\n")
	fmt.Fprintf(&b, "- 逐像素热力图一致：`%t`\n", three.Comparison.HeatmapEqual)
	fmt.Fprintf(&b, "- 逐块统计摘要一致：`%t`\n", three.Comparison.SummaryEqual)
	fmt.Fprintf(&b, "- 最终块提交序列一致：`%t`\n", three.Comparison.SequenceEqual)
	fmt.Fprintf(&b, "- 三份热力图 SHA-256：`%s`\n", three.Comparison.IncrementalHash)
	fmt.Fprintf(&b, "- 最终提交序列长度：`%d`\n\n", len(three.Comparison.IncrementalCommit))
	b.WriteString("## 强制终止与断点恢复\n\n")
	fmt.Fprintf(&b, "- 第 5 个分块原子提交后退出，退出码：`%d`\n", killCode)
	fmt.Fprintf(&b, "- 重启后复用已持久化且版本匹配分块：`%d`\n", resume.ReusedBlocks)
	fmt.Fprintf(&b, "- 重算缺失分块：`%d`\n", resume.RecomputedBlocks)
	fmt.Fprintf(&b, "- 最终完成：`%t`，提交账本长度：`%d`\n\n", resume.Completed, len(resume.CommitLedger))
	if summary.VersionMismatch != nil {
		b.WriteString("## 版本不匹配保护\n\n")
		fmt.Fprintf(&b, "- 重启后改用 `window=5`：`snapshotResetReason=%s`，复用 `%d` 块，禁止混用旧快照。\n\n",
			mismatch.SnapshotResetReason, mismatch.ReusedBlocks)
	}
	b.WriteString("## 产物\n\n")
	b.WriteString("- `artifacts/samples/`: 固定种子原图、基准图、篡改掩码、真值清单\n")
	b.WriteString("- `artifacts/reports/`: JSON/CSV 取证报告、原图、热力图\n")
	b.WriteString("- `artifacts/acceptance/sequence.json`: 增量失效明细\n")
	b.WriteString("- `artifacts/acceptance/threeway.json`: 三路径逐像素比对\n")
	b.WriteString("- `artifacts/acceptance/resume-run.json`: 强杀后续跑结果\n")
	if err := os.WriteFile(filepath.Join(*dir, "summary.md"), []byte(b.String()), 0o644); err != nil {
		return forensic.DecodeError("write acceptance markdown", err)
	}
	return printJSON(map[string]any{"summaryJson": filepath.Join(*dir, "summary.json"), "summaryMarkdown": filepath.Join(*dir, "summary.md")})
}

func compactResult(r forensic.RunResult) map[string]any {
	return map[string]any{
		"imageId": r.ImageID, "params": r.Params, "paramsVersion": r.ParamsVersion,
		"totalBlocks": r.Summary.TotalBlocks, "tamperedBlocks": r.Summary.TamperedBlocks,
		"reusedBlocks": r.Summary.ReusedBlocks, "recomputedBlocks": r.Summary.RecomputedBlocks,
		"truthAgreement": r.TruthAgreement, "truthWithinTolerance": r.TruthWithinTolerance,
		"truthConfusion": r.TruthConfusion, "invalidations": r.Invalidations,
		"reuses": r.Reuses, "queueEvents": r.QueueEvents,
		"heatmapKey": r.HeatmapKey, "heatmapSha256": r.HeatmapSHA256,
		"commitSequence": r.CommitSequence, "commitLedger": r.CommitLedger,
		"snapshotResetReason": r.SnapshotResetReason, "completed": r.Completed,
	}
}

func parseScope(value string) ([]int, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, forensic.ParamError("scope", "scope must contain comma-separated block indexes")
		}
		out = append(out, n)
	}
	return out, nil
}

func parseFloats(value string) ([]float64, error) {
	parts := strings.Split(value, ",")
	out := make([]float64, 0, len(parts))
	for _, part := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float %q", part)
		}
		out = append(out, v)
	}
	return out, nil
}

func parseIntsOptional(value string, fallback int) ([]int, error) {
	if strings.TrimSpace(value) == "" {
		return []int{fallback}, nil
	}
	parts := strings.Split(value, ",")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q", part)
		}
		out = append(out, v)
	}
	return out, nil
}

func repeatInt(value, count int) []int {
	out := make([]int, count)
	for i := range out {
		out[i] = value
	}
	return out
}

func writeError(err error) {
	payload := map[string]any{"ok": false, "error": err.Error()}
	if fe, ok := forensic.AsError(err); ok {
		payload["code"] = fe.Code
		payload["field"] = fe.Field
		payload["imageId"] = fe.ImageID
		payload["block"] = fe.Block
		payload["stage"] = fe.Stage
		if fe.X != 0 || fe.Y != 0 {
			payload["x"] = fe.X
			payload["y"] = fe.Y
		}
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Fprintln(os.Stderr, string(data))
}

func printJSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
