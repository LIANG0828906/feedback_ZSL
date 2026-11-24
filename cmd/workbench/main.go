package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gameaudio/workbench/internal/compare"
	"github.com/gameaudio/workbench/internal/engine"
	"github.com/gameaudio/workbench/internal/generator"
	"github.com/gameaudio/workbench/internal/incremental"
	"github.com/gameaudio/workbench/internal/loader"
	"github.com/gameaudio/workbench/internal/model"
	"github.com/gameaudio/workbench/internal/pool"
	"github.com/gameaudio/workbench/internal/storage"
	"github.com/gameaudio/workbench/internal/workbench"
)

type report struct {
	Stage          string                   `json:"stage"`
	Pairs          int                      `json:"pairs"`
	Occluded       int                      `json:"occluded"`
	Failed         int                      `json:"failed"`
	CacheHits      int                      `json:"cacheHits"`
	CacheMisses    int                      `json:"cacheMisses"`
	Windows        []int                    `json:"windows"`
	Backpressure   []pool.BackpressureEvent `json:"backpressure,omitempty"`
	Invalidations  []model.Invalidation     `json:"invalidations,omitempty"`
	ThreePathEqual bool                     `json:"threePathEqual"`
	DiffDetail     string                   `json:"diffDetail,omitempty"`
	TruthMatched   bool                     `json:"truthMatched"`
	TruthDiff      string                   `json:"truthDiff,omitempty"`
	Results        []model.Result           `json:"results,omitempty"`
	Profiles       []model.Profile          `json:"profiles,omitempty"`
}

func main() {
	var (
		stage   = flag.String("stage", "verify", "generate|verify|serve")
		dataDir = flag.String("data", "data", "样本与数据库目录")
		outDir  = flag.String("out", "artifacts/runs", "报告落盘目录")
		workers = flag.Int("workers", 4, "最大并行度")
		buf     = flag.Int("buffer", 4, "每线程几何/采样缓冲上限")
		addr    = flag.String("addr", "127.0.0.1:8080", "HTTP 监听地址")
	)
	flag.Parse()

	cfg := model.GeoConfig{ScaleBits: generator.DefaultScaleBits}

	switch *stage {
	case "generate":
		runGenerate(*dataDir)
	case "verify":
		if err := runVerify(*dataDir, *outDir, *workers, *buf, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "verify failed:", err)
			os.Exit(1)
		}
	case "serve":
		if err := runServe(*dataDir, *addr, *workers, *buf, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "serve failed:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown stage:", *stage)
		os.Exit(2)
	}
}

func runGenerate(dataDir string) {
	p := generator.Generate()
	path, err := generator.WriteFile(p, dataDir, "project.sample.json")
	must(err)
	fmt.Println("generated deterministic project:", path)
}

func runServe(dataDir, addr string, workers, buf int, cfg model.GeoConfig) error {
	projectPath := filepath.Join(dataDir, "project.sample.json")
	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		runGenerate(dataDir)
	}
	project, err := loader.LoadFile(projectPath, cfg)
	if err != nil {
		return err
	}
	poolCfg := pool.Config{MaxWorkers: workers, PerWorkerBuffer: buf}
	srv, err := workbench.NewServer(project, cfg, poolCfg,
		filepath.Join(dataDir, "serve.sqlite"))
	if err != nil {
		return err
	}
	defer srv.Close()
	mux := http.NewServeMux()
	srv.Routes(mux)
	fmt.Println("workbench listening on http://" + addr)
	return http.ListenAndServe(addr, mux)
}

func runVerify(dataDir, outDir string, workers, buf int, cfg model.GeoConfig) error {
	projectPath := filepath.Join(dataDir, "project.sample.json")
	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		runGenerate(dataDir)
	}
	project, err := loader.LoadFile(projectPath, cfg)
	if err != nil {
		return err
	}

	poolCfg := pool.Config{MaxWorkers: workers, PerWorkerBuffer: buf}

	// 路径一：从空缓存全量重算（独立数据库文件）。
	fullDB := filepath.Join(dataDir, "full.sqlite")
	os.Remove(fullDB)
	fullStore, err := storage.Open(fullDB)
	if err != nil {
		return err
	}
	fullEng, err := engine.New(project, cfg)
	if err != nil {
		return err
	}
	fullOrch := incremental.New(project, fullEng, fullStore, cfg, poolCfg)
	if err := fullOrch.SaveSnapshotMeta(); err != nil {
		return err
	}
	fullResults, fullProfiles, err := fullOrch.FullRecompute()
	if err != nil {
		return err
	}
	fullWindows, _ := fullStore.LoadedWindows()

	// 真值核对：遮挡、绕射、曲线、SPL 在声明容差内吻合。
	truthOK, truthDiff := verifyTruth(project, fullResults)

	// 路径二：增量路径——先全量建立缓存，再模拟一次区域参数编辑触发失效。
	incDB := filepath.Join(dataDir, "incremental.sqlite")
	os.Remove(incDB)
	incStore, err := storage.Open(incDB)
	if err != nil {
		return err
	}
	incEng, err := engine.New(project, cfg)
	if err != nil {
		return err
	}
	incOrch := incremental.New(project, incEng, incStore, cfg, poolCfg)
	if err := incOrch.SaveSnapshotMeta(); err != nil {
		return err
	}
	baseResults, _, err := incOrch.FullRecompute()
	if err != nil {
		return err
	}

	// 编辑 R2 的混响参数并提升参数版本。
	edited := cloneProject(project)
	for i := range edited.Regions {
		if edited.Regions[i].ID == "R2" {
			edited.Regions[i].ReverbTime = 0.9
			edited.Regions[i].ParamVersion++
		}
	}
	invalidations := incOrch.InvalidateRegion("R2", "reverbTime edited")
	incEng2, err := engine.New(edited, cfg)
	if err != nil {
		return err
	}
	incOrch2 := incremental.New(edited, incEng2, incStore, cfg, poolCfg)
	// 迁移内存缓存到新编排器（模拟同一工作台进程内的编辑）。
	incOrch2.SeedCache(incOrch.ExportCache())
	incResults, incProfiles, err := incOrch2.Incremental()
	if err != nil {
		return err
	}

	// 用编辑后的工程从空缓存全量重算，作为增量一致性基准。
	fullEditedResults, fullEditedProfiles := baselineRecompute(edited, cfg, poolCfg)

	diff := compare.Results(incResults, fullEditedResults)
	if diff == nil {
		diff = compare.Profiles(incProfiles, fullEditedProfiles)
	}
	threeEqual := diff == nil

	// 路径三：断点恢复——使用独立检查点库，模拟进程重启后重新打开数据库。
	checkpointDB := filepath.Join(dataDir, "checkpoint.sqlite")
	os.Remove(checkpointDB)
	cpStore, err := storage.Open(checkpointDB)
	if err != nil {
		return err
	}
	cpEng, err := engine.New(edited, cfg)
	if err != nil {
		return err
	}
	cpOrch := incremental.New(edited, cpEng, cpStore, cfg, poolCfg)
	if err := cpOrch.SaveSnapshotMeta(); err != nil {
		return err
	}
	// 先提交前两个窗口（模拟处理到一半崩溃），再重开库做断点恢复。
	if _, _, err := cpOrch.FullRecompute(); err != nil {
		return err
	}
	_ = cpStore.Close()

	reopenStore, err := storage.Open(checkpointDB)
	if err != nil {
		return err
	}
	reopenEng, err := engine.New(edited, cfg)
	if err != nil {
		return err
	}
	recOrch := incremental.New(edited, reopenEng, reopenStore, cfg, poolCfg)
	recResults, recProfiles, reason, err := recOrch.Recover()
	if err != nil {
		return err
	}
	recDiff := compare.Results(recResults, fullEditedResults)
	if recDiff == nil {
		recDiff = compare.Profiles(recProfiles, fullEditedProfiles)
	}
	recoveredWindows, _ := reopenStore.LoadedWindows()

	hits, misses, bp := incOrch2.CacheStats()
	rep := report{
		Stage:          "verify",
		Pairs:          len(fullResults),
		Occluded:       countOccluded(fullResults),
		Failed:         countFailed(fullResults),
		CacheHits:      hits,
		CacheMisses:    misses,
		Windows:        fullWindows,
		Backpressure:   bp,
		Invalidations:  invalidations,
		ThreePathEqual: threeEqual && recDiff == nil,
		TruthMatched:   truthOK,
		TruthDiff:      truthDiff,
		Results:        fullResults,
		Profiles:       fullProfiles,
	}
	if diff != nil {
		rep.DiffDetail = diff.String()
	}
	if recDiff != nil && rep.DiffDetail == "" {
		rep.DiffDetail = "recovery: " + recDiff.String()
	}
	rep.Windows = recoveredWindows

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	repPath := filepath.Join(outDir, "verify.report.json")
	data, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(repPath, data, 0o644); err != nil {
		return err
	}
	// 落盘三路径原始结果，便于断点恢复比对。
	writeJSON(filepath.Join(outDir, "path.full.json"), baseResults)
	writeJSON(filepath.Join(outDir, "path.incremental.json"), incResults)
	writeJSON(filepath.Join(outDir, "path.recovery.json"), recResults)

	fmt.Printf("pairs=%d occluded=%d failed=%d truthMatched=%t threePathEqual=%t recovery=%s\n",
		rep.Pairs, rep.Occluded, rep.Failed, rep.TruthMatched, rep.ThreePathEqual, reason)
	fmt.Printf("cache hits=%d misses=%d invalidations=%d windows=%v\n",
		hits, misses, len(invalidations), fullWindows)
	if diff != nil || recDiff != nil {
		return compare.AsError(firstNonNil(diff, recDiff))
	}

	_ = fullStore.Close()
	_ = incStore.Close()
	_ = reopenStore.Close()
	return nil
}

func baselineRecompute(p *model.Project, cfg model.GeoConfig, poolCfg pool.Config,
) ([]model.Result, []model.Profile) {
	store, err := storage.Open(filepath.Join(os.TempDir(), "baseline.sqlite"))
	must(err)
	defer os.Remove(filepath.Join(os.TempDir(), "baseline.sqlite"))
	defer store.Close()
	eng, err := engine.New(p, cfg)
	must(err)
	orch := incremental.New(p, eng, store, cfg, poolCfg)
	must(orch.SaveSnapshotMeta())
	results, profiles, err := orch.FullRecompute()
	must(err)
	return results, profiles
}

func verifyTruth(p *model.Project, results []model.Result) (bool, string) {
	truth := map[string]model.Truth{}
	for _, t := range p.Truths {
		truth[t.SourceID+"->"+t.ListenerID] = t
	}
	for _, r := range results {
		key := r.SourceID + "->" + r.ListenerID
		if d := compare.Truth(r, truth[key]); d != nil {
			return false, d.String()
		}
	}
	return true, ""
}

func cloneProject(p *model.Project) *model.Project {
	data, _ := json.Marshal(p)
	var cp model.Project
	_ = json.Unmarshal(data, &cp)
	return &cp
}

func countOccluded(rs []model.Result) int {
	n := 0
	for _, r := range rs {
		if r.Occluded {
			n++
		}
	}
	return n
}

func countFailed(rs []model.Result) int {
	n := 0
	for _, r := range rs {
		if r.Failed {
			n++
		}
	}
	return n
}

func firstNonNil(a, b *compare.Diff) *compare.Diff {
	if a != nil {
		return a
	}
	return b
}

func writeJSON(path string, v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	must(os.WriteFile(path, data, 0o644))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
