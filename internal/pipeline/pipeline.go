// Package pipeline 编排分帧分析、声场重建、渲染、误差报告、状态机与检查点，
// 并保证实时逐帧、断点恢复、持久化全量复算三条路径逐样本一致。
package pipeline

import (
	"context"
	"sync"

	"spatialir/internal/analyzer"
	"spatialir/internal/checkpoint"
	"spatialir/internal/errorsx"
	"spatialir/internal/loader"
	"spatialir/internal/model"
	"spatialir/internal/renderer"
	"spatialir/internal/soundfield"
	"spatialir/internal/statemachine"
)

// RunMode 标识三条执行路径。
type RunMode string

const (
	ModeRealtime RunMode = "realtime" // 实时逐帧处理并写检查点
	ModeResume   RunMode = "resume"   // 断点恢复：复用已提交帧，仅处理未提交帧
	ModeReplay   RunMode = "replay"   // 基于已持久化参数的全量复算
)

// Options 是一次流水线运行的输入。
type Options struct {
	Dataset   *loader.Dataset
	Params    model.FrameParams
	Store     *checkpoint.Store
	Mode      RunMode
	LayoutMap map[string]string
	// PauseAt > 0 时，提交到该帧后暂停返回（可退出后续用 resume 续跑）。
	PauseAt int
}

// Result 是流水线输出，包含逐帧快照、合并波形与阻塞事件。
type Result struct {
	Report     *model.RunReport
	PoolEvents []PoolEvent
	Resumed    bool
}

// Engine 持有跨运行的状态机与资源池。
type Engine struct {
	ds     *loader.Dataset
	params model.FrameParams
	frames []analyzer.Frame
	sm     *statemachine.Machine
	pool   *Pool
}

func NewEngine(ds *loader.Dataset, params model.FrameParams) *Engine {
	frames := analyzer.BuildFrames(ds.Manifest.LengthSamples, params)
	return &Engine{
		ds:     ds,
		params: params,
		frames: frames,
		sm:     statemachine.New(len(frames)),
		pool:   NewPool(params.MaxWorkers, params.BufferCap),
	}
}

func (e *Engine) Frames() []analyzer.Frame       { return e.frames }
func (e *Engine) Machine() *statemachine.Machine { return e.sm }

// Run 执行一条完整路径。replay 模式要求检查点中存在全部已提交帧。
func (e *Engine) Run(ctx context.Context, opts Options) (*Result, error) {
	hash, err := InputHash(opts.Dataset.Manifest, opts.Dataset.Signals, opts.Dataset.Dry)
	if err != nil {
		return nil, err
	}
	resumed, err := opts.Store.InitSnapshot(hash, opts.Params, len(e.frames))
	if err != nil {
		return nil, err
	}

	snapshots := make([]*model.FrameSnapshot, len(e.frames))
	commitSeq := make([]int, 0, len(e.frames))

	switch opts.Mode {
	case ModeReplay:
		if err := e.loadReplay(opts.Store, snapshots); err != nil {
			return nil, err
		}
		for i := range snapshots {
			commitSeq = append(commitSeq, i)
		}
	case ModeRealtime, ModeResume:
		if err := e.runIncremental(ctx, opts, resumed, snapshots, &commitSeq); err != nil {
			return nil, err
		}
	default:
		return nil, model.NewError(model.CodePara, "unknown run mode").WithField("mode")
	}

	frameSnaps := make([]model.FrameSnapshot, len(snapshots))
	frameParts := make([][][]float64, len(snapshots))
	ch := opts.Dataset.Manifest.Layout.Channels
	for i, s := range snapshots {
		if s == nil {
			s = &model.FrameSnapshot{Index: i, Status: model.StatusPending}
		}
		frameSnaps[i] = *s
		if s.Rendered != nil {
			frameParts[i] = s.Rendered
		} else {
			frameParts[i] = zeroFrames(ch, opts.Dataset.Manifest.LengthSamples)
		}
	}
	rendered := renderer.Merge(frameParts, ch, opts.Dataset.Manifest.LengthSamples)
	report := &model.RunReport{
		Mode:         string(opts.Mode),
		Frames:       len(e.frames),
		Committed:    len(commitSeq),
		FrameReports: frameSnaps,
		Rendered:     rendered,
		Original:     opts.Dataset.Signals,
		Layout:       opts.Dataset.Manifest.Layout,
		SampleRate:   opts.Dataset.Manifest.SampleRate,
		CommitSeq:    commitSeq,
	}
	return &Result{Report: report, PoolEvents: e.pool.Events(), Resumed: resumed}, nil
}

func zeroFrames(channels, length int) [][]float64 {
	out := make([][]float64, channels)
	for i := range out {
		out[i] = make([]float64, length)
	}
	return out
}

func (e *Engine) runIncremental(ctx context.Context, opts Options, resumed bool,
	snapshots []*model.FrameSnapshot, commitSeq *[]int) error {
	// 恢复模式总是装载已持久化的已提交帧并重建冻结态；
	// resumed=false 仅出现在同进程重建 Store 等场景，已提交帧仍必须复用。
	if opts.Mode == ModeResume {
		loaded, err := opts.Store.LoadFrames()
		if err != nil {
			return err
		}
		for _, snap := range loaded {
			if snap.Index >= len(snapshots) {
				continue
			}
			cp := snap
			snapshots[snap.Index] = &cp
			if e.sm.IsFrozen(snap.Index) {
				// 同一引擎内已冻结帧直接复用，不重复迁移状态机。
				*commitSeq = append(*commitSeq, snap.Index)
				continue
			}
			if err := e.sm.Transition(snap.Index, model.StatusEstimated); err != nil {
				return err
			}
			if err := e.sm.Transition(snap.Index, model.StatusCommitted); err != nil {
				return err
			}
			*commitSeq = append(*commitSeq, snap.Index)
		}
	}

	var cumulative model.ErrorMetric
	var wg sync.WaitGroup
	var mu sync.Mutex
	var abortErr error

	for idx := range e.frames {
		if e.sm.IsFrozen(idx) {
			cumulative = accumulateCommitted(snapshots[idx], cumulative)
			continue
		}
		// 重试场景：旧的失败快照必须被重新估计覆盖。
		if snapshots[idx] != nil && snapshots[idx].Status == model.StatusFailed &&
			e.sm.Status(idx) == model.StatusPending {
			snapshots[idx] = nil
		}
		fr := e.frames[idx]
		if err := e.pool.Acquire(ctx, idx, fr.End-fr.Start); err != nil {
			return err
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer e.pool.Release(e.frames[idx].End - e.frames[idx].Start)
			snap, err := e.processFrame(idx, opts)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if isFrameFault(err) {
					// 帧级隔离：只置该帧失败，允许后续重试，不污染其他帧与已提交结果。
					_ = e.sm.Transition(idx, model.StatusFailed)
					snapshots[idx] = e.failedSnapshot(idx, err)
					return
				}
				if abortErr == nil {
					abortErr = err
				}
				return
			}
			if err := e.sm.Transition(idx, model.StatusEstimated); err != nil {
				if abortErr == nil {
					abortErr = err
				}
				return
			}
			snapshots[idx] = snap
		}(idx)
	}
	wg.Wait()
	if abortErr != nil {
		return abortErr
	}

	// 串行提交：严格按帧序写 SQLite，保证帧序稳定且写入不交错。
	for idx := range e.frames {
		if opts.PauseAt > 0 && len(*commitSeq) >= opts.PauseAt {
			break
		}
		if e.sm.IsFrozen(idx) || snapshots[idx] == nil {
			continue
		}
		snap := snapshots[idx]
		if snap.Status == model.StatusFailed {
			continue
		}
		nextCum, err := errorsx.CheckAndAccumulate(cumulative, snap.Bound.PerFrame,
			measuredOf(snap), e.params.BoundScale, idx)
		if err != nil {
			// E_BOUND 同样帧级隔离：标记失败并保留指标名，其余帧继续提交。
			if err := e.sm.Transition(idx, model.StatusFailed); err != nil {
				return err
			}
			snapshots[idx] = e.failedSnapshot(idx, err)
			continue
		}
		cumulative = nextCum
		snap.Bound.Cumulative = cumulative
		// pending（重试或首次估计）与 estimated 两种来源都收敛到 committed。
		switch e.sm.Status(idx) {
		case model.StatusEstimated:
			if err := e.sm.Transition(idx, model.StatusCommitted); err != nil {
				return err
			}
		case model.StatusPending:
			if err := e.sm.Transition(idx, model.StatusEstimated); err != nil {
				return err
			}
			if err := e.sm.Transition(idx, model.StatusCommitted); err != nil {
				return err
			}
		default:
			return model.NewError(model.CodeState, "unexpected state before commit").WithFrame(idx)
		}
		snap.Status = model.StatusCommitted
		if err := opts.Store.CommitFrame(*snap); err != nil {
			return err
		}
		*commitSeq = append(*commitSeq, idx)
	}
	return nil
}

// processFrame 对单帧做估计→声场→渲染→误差，所有计算仅依赖输入与参数，
// 不依赖并发执行顺序，因此三条路径可逐样本复现。
func (e *Engine) processFrame(idx int, opts Options) (*model.FrameSnapshot, error) {
	fr := e.frames[idx]
	estimates, err := analyzer.AnalyzeFrame(e.ds, fr, e.params)
	if err != nil {
		return nil, err
	}
	itd := soundfield.Recompute(estimates, e.ds.Manifest.Reference)
	layoutMap, err := soundfield.LayoutMap(e.ds.Manifest.Layout, opts.LayoutMap)
	if err != nil {
		return nil, err
	}
	rendered, err := renderer.RenderFrame(e.ds, fr, estimates, e.params, layoutMap)
	if err != nil {
		return nil, err
	}
	energy := renderer.EnergyStats(e.ds.Signals, rendered, fr)
	measured := errorsx.Measured(e.ds, fr, e.params.WindowLen, estimates, itd, rendered, e.ds.Signals)
	perBound := errorsx.StructuralBound(e.params, e.ds.Manifest.SampleRate)
	return &model.FrameSnapshot{
		Index:     idx,
		Status:    model.StatusEstimated,
		Start:     fr.Start,
		End:       fr.End,
		Estimates: estimates,
		ITD:       itd,
		Measured:  measured,
		Bound: model.ErrorBound{
			PerFrame: perBound,
		},
		Energy:   energy,
		Rendered: rendered,
	}, nil
}

// measuredOf 返回估计阶段计算并随快照持久化的逐帧实测误差。
func measuredOf(snap *model.FrameSnapshot) model.ErrorMetric {
	return snap.Measured
}

func (e *Engine) failedSnapshot(idx int, err error) *model.FrameSnapshot {
	fr := e.frames[idx]
	ae := model.AsAppError(err)
	snap := &model.FrameSnapshot{Index: idx, Status: model.StatusFailed, Start: fr.Start, End: fr.End}
	if ae != nil {
		snap.FailCode = ae.Code
		snap.FailReason = ae.Error()
	} else {
		snap.FailReason = err.Error()
	}
	return snap
}

// isFrameFault 判定可在帧内隔离、可重试的业务异常码。
func isFrameFault(err error) bool {
	ae := model.AsAppError(err)
	if ae == nil {
		return false
	}
	switch ae.Code {
	case model.CodeOrder, model.CodeClip, model.CodeFrame, model.CodeBound:
		return true
	default:
		return false
	}
}

func (e *Engine) loadReplay(store *checkpoint.Store, snapshots []*model.FrameSnapshot) error {
	loaded, err := store.LoadFrames()
	if err != nil {
		return err
	}
	if len(loaded) != len(e.frames) {
		return model.NewError(model.CodeState, "replay requires all frames committed")
	}
	for _, snap := range loaded {
		if snap.Index < 0 || snap.Index >= len(snapshots) {
			return model.NewError(model.CodeFrame, "persisted frame index out of range").WithFrame(snap.Index)
		}
		cp := snap
		snapshots[snap.Index] = &cp
	}
	for i := range snapshots {
		if snapshots[i] == nil || snapshots[i].Status != model.StatusCommitted {
			return model.NewError(model.CodeState, "replay snapshot incomplete").WithFrame(i)
		}
	}
	return nil
}

// RetryFailed 把失败帧重置为待估计，允许重新估计，不触碰已提交帧。
func (e *Engine) RetryFailed(frames ...int) error {
	for _, idx := range frames {
		if e.sm.Status(idx) != model.StatusFailed {
			return model.NewError(model.CodeState, "only failed frames can be retried").WithFrame(idx)
		}
		if err := e.sm.Transition(idx, model.StatusPending); err != nil {
			return err
		}
	}
	return nil
}

func accumulateCommitted(snap *model.FrameSnapshot, cumulative model.ErrorMetric) model.ErrorMetric {
	if snap == nil {
		return cumulative
	}
	cumulative.PeakQuantization = snap.Bound.Cumulative.PeakQuantization
	cumulative.ITDEstimation = snap.Bound.Cumulative.ITDEstimation
	cumulative.RenderInterp = snap.Bound.Cumulative.RenderInterp
	cumulative.EnergyTruncation = snap.Bound.Cumulative.EnergyTruncation
	return cumulative
}
