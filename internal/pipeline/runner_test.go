package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"audiobench/internal/dsp"
	"audiobench/internal/ingest"
	"audiobench/internal/model"
	"audiobench/internal/sample"
)

func loadCfg(t *testing.T, id string, windowFrames int) (RunConfig, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := sample.GenerateAll(sample.Options{
		OutDir: dir, Seed: 11, SampleRate: 16000,
		MaterialSecs: 8, EventCount: 6, WindowSize: 1024,
	}); err != nil {
		t.Fatal(err)
	}
	mat, err := ingest.LoadMaterial(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	samples, err := ingest.ReadPCM(mat.PCMFile)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := ingest.ContentHash(mat.PCMFile)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "cp.db")
	return RunConfig{
		Material: mat, Samples: samples, Params: model.DefaultParams(),
		ContentHash: hash, WindowFrames: windowFrames,
		Limits: DefaultLimits(), DBPath: dbPath,
	}, dir
}

func TestThreePathsConsistent(t *testing.T) {
	cfg, _ := loadCfg(t, "mat_main", 8)
	ctx := context.Background()

	full, err := RunFull(ctx, cfg)
	if err != nil {
		t.Fatalf("全量重算失败: %v", err)
	}
	rt, err := RunRealtime(ctx, cfg)
	if err != nil {
		t.Fatalf("实时标注失败: %v", err)
	}
	outcome, err := RunResume(ctx, cfg)
	if err != nil {
		t.Fatalf("断点恢复失败: %v", err)
	}
	if errs := CompareThree(rt, outcome.Annotation, full); len(errs) > 0 {
		t.Fatalf("三路径不一致: %+v", errs)
	}
	if len(full.Events) == 0 {
		t.Fatal("主素材应检出事件")
	}
	if full.Quality == nil || full.Quality.LabelAccuracy != 1.0 {
		t.Fatalf("类别正确率应为 1.0，实际 %+v", full.Quality)
	}
}

func TestCrashResumeUsesOnlyCommittedWindows(t *testing.T) {
	cfg, _ := loadCfg(t, "mat_main", 4)
	ctx, cancel := context.WithCancel(context.Background())

	// 启动实时标注后立即取消，模拟进程被强制终止：只能依赖已落盘窗口。
	go func() {
		_, _ = RunRealtime(ctx, cfg)
	}()
	cancel()

	// 新进程视角：RunResume 从已持久化窗口续跑并补全。
	outcome, err := RunResume(context.Background(), cfg)
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	full, err := RunFull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if errs := CompareThree(outcome.Annotation, outcome.Annotation, full); len(errs) > 0 {
		t.Fatalf("崩溃恢复与全量结果不一致: %+v", errs)
	}
	if len(outcome.Annotation.Fingerprints) != len(full.Fingerprints) {
		t.Fatalf("恢复后帧数 %d 与全量 %d 不符",
			len(outcome.Annotation.Fingerprints), len(full.Fingerprints))
	}
}

func TestParamChangeInvalidatesWholeSnapshot(t *testing.T) {
	cfg, _ := loadCfg(t, "mat_main", 8)
	ctx := context.Background()
	if _, err := RunRealtime(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// 参数变化后恢复，必须整体作废而非混用旧窗口。
	cfg.Params.FrameShift = 256
	if err := dsp.ValidateParams(cfg.Params); err != nil {
		t.Fatal(err)
	}
	outcome, err := RunResume(ctx, cfg)
	if err != nil {
		t.Fatalf("带新参数恢复失败: %v", err)
	}
	if !outcome.Invalidated {
		t.Fatal("参数版本不匹配时快照必须整体作废")
	}
	full, err := RunFull(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if errs := CompareThree(outcome.Annotation, outcome.Annotation, full); len(errs) > 0 {
		t.Fatalf("作废重算后结果应与全量一致: %+v", errs)
	}
}

func TestQuietReturnsEQuietWithMeasuredRatio(t *testing.T) {
	cfg, _ := loadCfg(t, "mat_quiet", 8)
	_, err := RunFull(context.Background(), cfg)
	appErr, ok := err.(*model.AppError)
	if !ok || appErr.Code != model.EQuiet {
		t.Fatalf("期望 E_QUIET，实际 %v", err)
	}
	if _, exists := appErr.Details["voice_energy_ratio"]; !exists {
		t.Fatal("E_QUIET 必须给出实测声能量占比")
	}
}

func TestEmptyMaterialReturnsEmptyEvents(t *testing.T) {
	cfg, _ := loadCfg(t, "mat_empty", 8)
	ann, err := RunFull(context.Background(), cfg)
	if err != nil {
		t.Fatalf("无命中应返回空列表而非错误，实际 %v", err)
	}
	if len(ann.Events) != 0 {
		t.Fatalf("期望空事件列表，实际 %d 个", len(ann.Events))
	}
}
