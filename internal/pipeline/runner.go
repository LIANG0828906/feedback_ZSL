package pipeline

import (
	"context"

	"audiobench/internal/checkpoint"
	"audiobench/internal/compare"
	"audiobench/internal/dsp"
	"audiobench/internal/match"
	"audiobench/internal/model"
	"audiobench/internal/window"
)

// PathName 三条执行路径的稳定名称。
const (
	PathRealtime = "realtime" // 实时增量标注
	PathResume   = "resume"   // 断点恢复标注
	PathFull     = "full"     // 一次性全量重算
)

// RunConfig 一次运行的输入。
type RunConfig struct {
	Material     *model.Material
	Samples      []float64
	Params       model.Params
	ContentHash  string
	WindowFrames int
	Limits       Limits
	DBPath       string
}

// Annotation 一次完整标注的产物。
type Annotation struct {
	Path         string
	Fingerprints []model.Fingerprint
	Events       []model.Event
	Quality      *model.QualityReport
	VoiceRatio   float64
	Plan         *window.Plan
	BlockEvents  []BlockEvent
	Invalidated  bool
	QualityErr   *model.AppError
}

// ensureQuiet 静音占比检查，超阈值返回 E_QUIET 并给出实测声能量占比。
func ensureQuiet(samples []float64, p model.Params) (float64, *model.AppError) {
	ratio, _ := dsp.VoiceEnergyRatio(samples)
	if ratio < p.QuietRatio {
		return ratio, model.NewErr(model.EQuiet,
			"素材静音占比过高，实测声能量占比 %.4f 低于阈值 %.4f",
			ratio, p.QuietRatio).
			With("voice_energy_ratio", ratio).
			With("threshold", p.QuietRatio)
	}
	return ratio, nil
}

func finalize(samples []float64, mat *model.Material, p model.Params,
	frames []model.Fingerprint, plan *window.Plan, blocks []BlockEvent,
	path string, ratio float64) *Annotation {
	templates := match.BuildTemplates(mat.Categories, mat.SampleRate, p)
	events := match.Aggregate(frames, templates, p)
	quality, qErr := match.ValidateQuality(mat.Events, events, mat.SampleTolerance)
	ann := &Annotation{
		Path:         path,
		Fingerprints: frames,
		Events:       events,
		Quality:      quality,
		VoiceRatio:   ratio,
		Plan:         plan,
		BlockEvents:  blocks,
	}
	if qErr != nil {
		if appErr, ok := qErr.(*model.AppError); ok {
			ann.QualityErr = appErr
		}
	}
	return ann
}

// RunFull 一次性全量重算：顺序提取全部帧，不依赖检查点。
func RunFull(ctx context.Context, cfg RunConfig) (*Annotation, error) {
	if err := dsp.ValidateParams(cfg.Params); err != nil {
		return nil, err
	}
	ratio, qErr := ensureQuiet(cfg.Samples, cfg.Params)
	if qErr != nil {
		return nil, qErr
	}
	frames := dsp.ExtractAll(cfg.Samples, cfg.Params)
	totalFrames := dsp.FrameCount(len(cfg.Samples), cfg.Params.FrameLength, cfg.Params.FrameShift)
	plan := window.BuildPlan(totalFrames, cfg.WindowFrames, cfg.Params)
	for i := range plan.Windows {
		plan.Windows[i].State = window.StateCommitted
		plan.Windows[i].Fingerprints = frames[plan.Windows[i].StartFrame:plan.Windows[i].EndFrame]
	}
	return finalize(cfg.Samples, cfg.Material, cfg.Params, frames, plan, nil, PathFull, ratio), nil
}

// RunRealtime 实时增量路径：窗口并行提取、串行提交到 SQLite，直至完成。
func RunRealtime(ctx context.Context, cfg RunConfig) (*Annotation, error) {
	if err := dsp.ValidateParams(cfg.Params); err != nil {
		return nil, err
	}
	ratio, qErr := ensureQuiet(cfg.Samples, cfg.Params)
	if qErr != nil {
		return nil, qErr
	}
	totalFrames := dsp.FrameCount(len(cfg.Samples), cfg.Params.FrameLength, cfg.Params.FrameShift)
	plan := window.BuildPlan(totalFrames, cfg.WindowFrames, cfg.Params)

	store, err := checkpoint.Open(ctx, cfg.DBPath, cfg.Material.ID)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	meta := checkpoint.Meta{
		MaterialID: cfg.Material.ID, ContentHash: cfg.ContentHash,
		ParamsVersion:    checkpoint.ParamsVersion(cfg.Params),
		AlgorithmVersion: model.AlgorithmVersion,
		WindowFrames:     cfg.WindowFrames, TotalFrames: totalFrames,
	}
	if err := store.InitMeta(ctx, meta); err != nil {
		return nil, err
	}
	engine := NewEngine(plan, cfg.Samples, cfg.Params, cfg.Material.Categories,
		cfg.Material.SampleRate, store, cfg.Limits)
	if err := engine.RunToEnd(ctx); err != nil && ctx.Err() == nil {
		return nil, err
	}
	return finalize(cfg.Samples, cfg.Material, cfg.Params, engine.AllFingerprints(),
		plan, engine.BlockEvents(), PathRealtime, ratio), nil
}

// ResumeOutcome 恢复路径输出，区分正常恢复与快照整体作废。
type ResumeOutcome struct {
	Annotation  *Annotation
	Invalidated bool
	Reason      string
}

// RunResume 断点恢复路径：加载 SQLite 快照，身份不匹配则整体作废后全量重算，
// 匹配则仅从首个未提交窗口续跑，禁止混用旧窗口。
func RunResume(ctx context.Context, cfg RunConfig) (*ResumeOutcome, error) {
	if err := dsp.ValidateParams(cfg.Params); err != nil {
		return nil, err
	}
	ratio, qErr := ensureQuiet(cfg.Samples, cfg.Params)
	if qErr != nil {
		return nil, qErr
	}
	totalFrames := dsp.FrameCount(len(cfg.Samples), cfg.Params.FrameLength, cfg.Params.FrameShift)
	store, err := checkpoint.Open(ctx, cfg.DBPath, cfg.Material.ID)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	expected := checkpoint.Meta{
		MaterialID: cfg.Material.ID, ContentHash: cfg.ContentHash,
		ParamsVersion:    checkpoint.ParamsVersion(cfg.Params),
		AlgorithmVersion: model.AlgorithmVersion,
		WindowFrames:     cfg.WindowFrames, TotalFrames: totalFrames,
	}
	meta, err := store.LoadMeta(ctx)
	if err != nil {
		return nil, err
	}
	outcome := &ResumeOutcome{}
	plan := window.BuildPlan(totalFrames, cfg.WindowFrames, cfg.Params)

	if meta == nil || !metaMatch(meta, &expected) {
		// 版本/哈希不匹配：整体快照作废，禁止部分混用。
		if err := store.Invalidate(ctx); err != nil {
			return nil, err
		}
		if err := store.InitMeta(ctx, expected); err != nil {
			return nil, err
		}
		outcome.Invalidated = true
		outcome.Reason = mismatchReason(meta, &expected)
	} else {
		saved, err := store.LoadWindows(ctx)
		if err != nil {
			return nil, err
		}
		for _, w := range saved {
			if w.Index < 0 || w.Index >= len(plan.Windows) {
				continue
			}
			target := &plan.Windows[w.Index]
			if target.State == window.StatePending {
				*target = w
				target.State = window.StateCommitted
			}
		}
	}

	engine := NewEngine(plan, cfg.Samples, cfg.Params, cfg.Material.Categories,
		cfg.Material.SampleRate, store, cfg.Limits)
	if err := engine.RunToEnd(ctx); err != nil && ctx.Err() == nil {
		return nil, err
	}
	ann := finalize(cfg.Samples, cfg.Material, cfg.Params, engine.AllFingerprints(),
		plan, engine.BlockEvents(), PathResume, ratio)
	ann.Invalidated = outcome.Invalidated
	outcome.Annotation = ann
	return outcome, nil
}

func metaMatch(a, b *checkpoint.Meta) bool {
	return checkpoint.MatchMeta(a, b)
}

func mismatchReason(got, want *checkpoint.Meta) string {
	if got == nil {
		return "无历史快照，初始化全量计算"
	}
	switch {
	case got.ContentHash != want.ContentHash:
		return "素材内容哈希不匹配"
	case got.ParamsVersion != want.ParamsVersion:
		return "参数版本不匹配"
	case got.AlgorithmVersion != want.AlgorithmVersion:
		return "算法版本不匹配"
	case got.TotalFrames != want.TotalFrames || got.WindowFrames != want.WindowFrames:
		return "窗口/总帧结构不匹配"
	default:
		return "快照身份不匹配"
	}
}

// CompareThree 对三条路径产物做自动比对，任一不一致返回 E_DIV。
func CompareThree(rt, rs, full *Annotation) []*model.AppError {
	toResults := func(a *Annotation) compare.Results {
		return compare.Results{Fingerprints: a.Fingerprints, Events: a.Events, Quality: a.Quality}
	}
	var errs []*model.AppError
	if err := compare.Compare(PathRealtime, PathResume, toResults(rt), toResults(rs)); err != nil {
		errs = append(errs, err)
	}
	if err := compare.Compare(PathRealtime, PathFull, toResults(rt), toResults(full)); err != nil {
		errs = append(errs, err)
	}
	if err := compare.Compare(PathResume, PathFull, toResults(rs), toResults(full)); err != nil {
		errs = append(errs, err)
	}
	return errs
}
