// audiobench 是游戏语音事件指纹标注桌面工作台的本地命令行入口，
// 提供样本生成、可重复端到端演示与本地 HTTP 工作台三种能力，全程离线。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"audiobench/internal/ingest"
	"audiobench/internal/model"
	"audiobench/internal/pipeline"
	"audiobench/internal/sample"
	"audiobench/internal/server"
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
	case "demo":
		err = cmdDemo(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`audiobench — 游戏语音事件指纹标注工作台（纯本地、离线、确定性）

用法:
  audiobench generate --data ./data --seed 42
  audiobench demo --data ./data --var ./var
  audiobench serve  --data ./data --var ./var --addr 127.0.0.1:8080`)
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	dataDir := fs.String("data", "./data", "样本输出目录")
	seed := fs.Int64("seed", 42, "确定性随机种子")
	sampleRate := fs.Int("rate", 16000, "采样率")
	secs := fs.Int("secs", 12, "每素材时长(秒)")
	events := fs.Int("events", 10, "普通素材事件数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opt := sample.Options{
		OutDir: *dataDir, Seed: *seed, SampleRate: *sampleRate,
		// 采样点容差取两个帧移量级，覆盖分帧量化造成的边界对齐误差。
		MaterialSecs: *secs, EventCount: *events, WindowSize: 1024,
	}
	dataset, err := sample.GenerateAll(opt)
	if err != nil {
		return err
	}
	fmt.Printf("[generate] 种子=%d 采样率=%d 输出目录=%s\n", dataset.Seed, dataset.SampleRate, *dataDir)
	for _, m := range dataset.Materials {
		fmt.Printf("  - %s\n", m)
	}
	fmt.Println("[generate] 类别参数: " + filepath.Join(*dataDir, "categories.json"))
	return nil
}

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	dataDir := fs.String("data", "./data", "样本目录")
	varDir := fs.String("var", "./var", "持久化目录(SQLite/报告)")
	seed := fs.Int64("seed", 42, "样本种子（目录为空时自动生成）")
	windowFrames := fs.Int("window-frames", 8, "每窗口帧数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*varDir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*dataDir, "dataset.json")); os.IsNotExist(err) {
		if _, err := sample.GenerateAll(sample.Options{
			OutDir: *dataDir, Seed: *seed, SampleRate: 16000,
			MaterialSecs: 12, EventCount: 10, WindowSize: 1024,
		}); err != nil {
			return err
		}
	}
	ctx := context.Background()
	report := runDemo(ctx, *dataDir, *varDir, *windowFrames)
	reportPath := filepath.Join(*varDir, "demo_report.json")
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(reportPath, data, 0o644); err != nil {
		return err
	}
	printSummary(report, reportPath)
	if !report.OverallConsistent || report.HasUnexpectedError {
		return fmt.Errorf("演示发现不一致或未预期错误，详见 %s", reportPath)
	}
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dataDir := fs.String("data", "./data", "样本目录")
	varDir := fs.String("var", "./var", "持久化目录")
	addr := fs.String("addr", "127.0.0.1:8080", "监听地址")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*varDir, 0o755); err != nil {
		return err
	}
	mgr := server.NewManager(*dataDir, *varDir)
	fmt.Printf("[serve] 工作台已启动（纯本地离线）: http://%s/\n", *addr)
	srv := &http.Server{Addr: *addr, Handler: mgr.Router(), ReadHeaderTimeout: 5 * time.Second}
	return srv.ListenAndServe()
}

// DemoReport 可重复演示的结构化结果。
type DemoReport struct {
	GeneratedAt        string            `json:"generated_at"`
	AlgorithmVersion   string            `json:"algorithm_version"`
	Materials          []MaterialDemo    `json:"materials"`
	OverallConsistent  bool              `json:"overall_consistent"`
	HasUnexpectedError bool              `json:"has_unexpected_error"`
	ErrorPathMatrix    map[string]string `json:"error_path_matrix"`
}

// MaterialDemo 单素材三路径结果与异常码。
type MaterialDemo struct {
	ID                string            `json:"id"`
	FrameCount        int               `json:"frame_count"`
	EventCounts       map[string]int    `json:"event_counts"`
	VoiceEnergyRatio  float64           `json:"voice_energy_ratio"`
	Consistent        bool              `json:"consistent"`
	ExpectedCode      string            `json:"expected_code"`
	ObservedCodes     map[string]string `json:"observed_codes"`
	Divergences       []*model.AppError `json:"divergences"`
	QualitySummary    map[string]any    `json:"quality_summary,omitempty"`
	ResumeInvalidated bool              `json:"resume_invalidated"`
	BlockEvents       int               `json:"block_events"`
}

func runDemo(ctx context.Context, dataDir, varDir string, windowFrames int) *DemoReport {
	report := &DemoReport{
		GeneratedAt:       time.Now().Format(time.RFC3339),
		AlgorithmVersion:  model.AlgorithmVersion,
		OverallConsistent: true,
		ErrorPathMatrix:   map[string]string{},
	}
	descs, _ := filepath.Glob(filepath.Join(dataDir, "mat_*.json"))
	for _, desc := range descs {
		md := runOneMaterial(ctx, desc, varDir, windowFrames)
		report.Materials = append(report.Materials, md)
		if !md.Consistent {
			report.OverallConsistent = false
		}
		// 预期异常码矩阵：静音素材必须稳定产生 E_QUIET，其余无异常。
		for path, code := range md.ObservedCodes {
			report.ErrorPathMatrix[md.ID+"/"+path] = code
			if md.ExpectedCode == "" && code != "" {
				report.HasUnexpectedError = true
			}
			if md.ExpectedCode != "" && code != md.ExpectedCode {
				report.HasUnexpectedError = true
			}
		}
	}
	return report
}

func runOneMaterial(ctx context.Context, desc, varDir string, windowFrames int) MaterialDemo {
	mat, err := ingest.LoadMaterial(desc)
	md := MaterialDemo{ObservedCodes: map[string]string{}, EventCounts: map[string]int{}}
	if err != nil {
		md.Consistent = false
		md.ObservedCodes["load"] = model.ErrCode(err)
		return md
	}
	md.ID = mat.ID
	if mat.Quiet {
		md.ExpectedCode = model.EQuiet
	}
	samples, err := ingest.ReadPCM(mat.PCMFile)
	if err != nil {
		md.ObservedCodes["pcm"] = model.ErrCode(err)
		return md
	}
	hash, err := ingest.ContentHash(mat.PCMFile)
	if err != nil {
		md.ObservedCodes["hash"] = model.ErrCode(err)
		return md
	}
	cfg := pipeline.RunConfig{
		Material: mat, Samples: samples, Params: model.DefaultParams(),
		ContentHash: hash, WindowFrames: windowFrames,
		Limits: pipeline.DefaultLimits(),
		DBPath: filepath.Join(varDir, "checkpoint_"+mat.ID+".db"),
	}
	// 干净演示：移除旧检查点，确保实时路径从零开始。
	_ = os.Remove(cfg.DBPath)
	_ = os.Remove(cfg.DBPath + "-wal")
	_ = os.Remove(cfg.DBPath + "-shm")

	rt, err := pipeline.RunRealtime(ctx, cfg)
	if err != nil {
		md.ObservedCodes[pipeline.PathRealtime] = model.ErrCode(err)
	}
	rs, err := pipeline.RunResume(ctx, cfg)
	if err != nil {
		md.ObservedCodes[pipeline.PathResume] = model.ErrCode(err)
	}
	full, err := pipeline.RunFull(ctx, cfg)
	if err != nil {
		md.ObservedCodes[pipeline.PathFull] = model.ErrCode(err)
	}
	// E_QUIET 等预期错误会让三条路径同时短路，视为一致行为。
	if rt == nil || rs == nil || full == nil {
		md.Consistent = codesUniform(md.ObservedCodes)
		return md
	}
	md.FrameCount = len(full.Fingerprints)
	md.EventCounts[pipeline.PathRealtime] = len(rt.Events)
	md.EventCounts[pipeline.PathResume] = len(rs.Annotation.Events)
	md.EventCounts[pipeline.PathFull] = len(full.Events)
	md.VoiceEnergyRatio = full.VoiceRatio
	md.ResumeInvalidated = rs.Invalidated
	md.BlockEvents = len(rt.BlockEvents)
	errs := pipeline.CompareThree(rt, rs.Annotation, full)
	md.Divergences = errs
	md.Consistent = len(errs) == 0
	if full.Quality != nil {
		q := full.Quality
		md.QualitySummary = map[string]any{
			"label_accuracy":  q.LabelAccuracy,
			"matched":         q.Matched,
			"mean_start_bias": q.MeanStartBias,
			"mean_end_bias":   q.MeanEndBias,
			"mismatches":      len(q.Mismatches),
		}
	}
	return md
}

func codesUniform(codes map[string]string) bool {
	var first string
	for _, c := range codes {
		if first == "" {
			first = c
		} else if c != first {
			return false
		}
	}
	return first != ""
}

func printSummary(r *DemoReport, reportPath string) {
	fmt.Println("================ 演示结果汇总 ================")
	for _, m := range r.Materials {
		fmt.Printf("素材 %-9s 帧=%-5d 事件(实/恢/全)=%v 声能占比=%.4f 一致=%v 预期码=%s 观察码=%v\n",
			m.ID, m.FrameCount, m.EventCounts, m.VoiceEnergyRatio,
			m.Consistent, emptyAsDash(m.ExpectedCode), m.ObservedCodes)
		if len(m.Divergences) > 0 {
			for _, d := range m.Divergences {
				fmt.Printf("   E_DIV: %v\n", d)
			}
		}
		if m.QualitySummary != nil {
			fmt.Printf("   质量: %v\n", m.QualitySummary)
		}
	}
	fmt.Printf("总体三路径一致: %v；异常码矩阵已写入: %s\n", r.OverallConsistent, reportPath)
}

func emptyAsDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
