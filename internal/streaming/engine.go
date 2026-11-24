package streaming

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"occlusionlab/internal/geometry"
	"occlusionlab/internal/model"
	"occlusionlab/internal/schema"
)

type Engine struct {
	mu        sync.Mutex
	Project   *model.Project
	Params    model.Params
	SceneHash string
	Windows   []*Window
	resident  []int
	pool      *Pool
	sink      SnapshotSink
}

func NewEngine(project *model.Project, params model.Params, sink SnapshotSink) (*Engine, *model.AppError) {
	if err := schema.ValidateParams(params); err != nil {
		return nil, err
	}
	if params.Quantization != project.Meta.Precision {
		return nil, model.ParamError("quantization",
			"运行时量化精度必须与场景工程声明一致；请重新生成场景")
	}
	pool, err := NewPool(params)
	if err != nil {
		return nil, err
	}
	sceneHash := project.ContentHash
	if sceneHash == "" {
		sceneHash = model.ProjectContentHash(project.Meta, project.World, project.Truth)
	}
	windows := make([]*Window, len(project.World.Cameras))
	for i := range windows {
		windows[i] = NewWindow(i)
	}
	return &Engine{
		Project:   project,
		Params:    params,
		SceneHash: sceneHash,
		Windows:   windows,
		resident:  []int{},
		pool:      pool,
		sink:      sink,
	}, nil
}

func (e *Engine) ProcessNext(ctx context.Context) (*WindowFrame, *model.AppError) {
	e.mu.Lock()
	defer e.mu.Unlock()
	index := -1
	for _, window := range e.Windows {
		if window.State == StateFailed {
			return nil, model.StateError(window.Index,
				"窗口处于失败状态；必须先重试该窗口，后续窗口保持待处理")
		}
		if window.State == StatePending {
			index = window.Index
			break
		}
	}
	if index < 0 {
		return nil, nil
	}
	window := e.Windows[index]
	if err := window.Transition(StateCulling); err != nil {
		return nil, err
	}
	frame, err := e.computeFrame(ctx, index)
	if err != nil {
		e.fail(window, err)
		return nil, err
	}
	if err := window.Transition(StateLoading); err != nil {
		e.fail(window, err)
		return nil, err
	}
	snapshot := Snapshot{
		Window:        index,
		Frame:         frame,
		Params:        e.Params,
		SceneHash:     e.SceneHash,
		ParamsHash:    e.Params.Hash(),
		ParamsVersion: e.Params.Version,
	}
	if e.sink != nil {
		if err := e.sink.AppendSnapshot(snapshot); err != nil {
			appErr := model.CheckpointError("窗口检查点串行写入失败")
			appErr.Window = index
			appErr.Detail = err.Error()
			appErr.Retryable = true
			if transitionErr := window.Transition(StateFailed); transitionErr == nil {
				window.LastError = appErr
			}
			return nil, appErr
		}
	}
	e.resident = append([]int(nil), frame.Resident...)
	if err := window.Freeze(frame); err != nil {
		return nil, err
	}
	return cloneFrame(frame), nil
}

func (e *Engine) computeFrame(ctx context.Context, window int) (*WindowFrame, *model.AppError) {
	eval, err := geometry.NewEvaluator(e.Project, e.Params, window)
	if err != nil {
		return nil, err
	}
	indices := make([]int, len(e.Project.World.Chunks))
	for i := range indices {
		indices[i] = i
	}
	verdicts, err := e.pool.EvaluateChunks(ctx, eval, window, indices)
	if err != nil {
		return nil, err
	}
	sort.Slice(verdicts, func(i, j int) bool { return verdicts[i].Chunk < verdicts[j].Chunk })

	frame := &WindowFrame{
		Window:        window,
		Time:          e.Project.World.Cameras[window].Time,
		CameraOffset:  int64(window + 1),
		ParamsHash:    e.Params.Hash(),
		ParamsVersion: e.Params.Version,
		SceneHash:     e.SceneHash,
		Chunks:        verdicts,
	}
	targetSet := map[int]bool{}
	for _, verdict := range verdicts {
		switch verdict.Status {
		case geometry.StatusVisible:
			frame.Visible = append(frame.Visible, verdict.Chunk)
		case geometry.StatusOccluded:
			frame.Occluded = append(frame.Occluded, verdict.Chunk)
		default:
			frame.Uncertain = append(frame.Uncertain, verdict.Chunk)
		}
		if verdict.Status == geometry.StatusVisible || verdict.Status == geometry.StatusUncertain {
			targetSet[verdict.Chunk] = true
		}
		if verdict.Error.ProjectionResidualQ > frame.Error.ProjectionResidualQ {
			frame.Error.ProjectionResidualQ = verdict.Error.ProjectionResidualQ
		}
		if verdict.Error.ProjectionErrorQ > frame.Error.ProjectionErrorQ {
			frame.Error.ProjectionErrorQ = verdict.Error.ProjectionErrorQ
		}
		if verdict.Error.FrustumPlaneErrorWorldQ > frame.Error.FrustumPlaneErrorWorldQ {
			frame.Error.FrustumPlaneErrorWorldQ = verdict.Error.FrustumPlaneErrorWorldQ
		}
		if verdict.Error.OcclusionCoverageErrorQ > frame.Error.CoverageErrorQ {
			frame.Error.CoverageErrorQ = verdict.Error.OcclusionCoverageErrorQ
		}
		if verdict.Error.DepthCompareErrorWorldQ > frame.Error.DepthCompareErrorWorldQ {
			frame.Error.DepthCompareErrorWorldQ = verdict.Error.DepthCompareErrorWorldQ
		}
		if verdict.Error.VisibilityUpperBoundQ > frame.Error.CumulativeUpperBoundQ {
			frame.Error.CumulativeUpperBoundQ = verdict.Error.VisibilityUpperBoundQ
		}
		frame.RasterCellsWork += verdict.Error.RasterResolution * verdict.Error.RasterResolution
		if verdict.ReferenceOccluder != "" {
			frame.References = append(frame.References, ChunkReference{
				Chunk: verdict.Chunk, Status: verdict.Status, Occluder: verdict.ReferenceOccluder,
			})
		}
	}
	sort.Ints(frame.Visible)
	sort.Ints(frame.Occluded)
	sort.Ints(frame.Uncertain)
	target := make([]int, 0, len(targetSet))
	for chunk := range targetSet {
		target = append(target, chunk)
	}
	sort.Ints(target)
	if len(target) > e.Params.MaxResident {
		return nil, model.InternalError(window,
			"驻留槽位不足，窗口未被静默截断", fmt.Errorf("需要 %d，上限 %d", len(target), e.Params.MaxResident))
	}
	totalCost := 0
	for _, chunk := range target {
		totalCost += e.Project.World.Chunks[chunk].LoadCost
	}
	if totalCost > e.Params.LoadCostBudget {
		return nil, model.InternalError(window,
			"加载成本预算耗尽，窗口进入失败态以允许重试", fmt.Errorf("成本 %d，预算 %d", totalCost, e.Params.LoadCostBudget))
	}
	residentSet := make(map[int]bool, len(e.resident))
	for _, chunk := range e.resident {
		residentSet[chunk] = true
	}
	targetMap := make(map[int]bool, len(target))
	for _, chunk := range target {
		targetMap[chunk] = true
		if !residentSet[chunk] {
			frame.Loaded = append(frame.Loaded, chunk)
		}
	}
	for _, chunk := range e.resident {
		if !targetMap[chunk] {
			frame.Unloaded = append(frame.Unloaded, chunk)
		}
	}
	frame.Resident = target
	frame.LoadCost = totalCost
	frame.computeDigests()
	return frame, nil
}

func (e *Engine) fail(window *Window, appErr *model.AppError) {
	if window.State == StateCommitted {
		return
	}
	if err := window.Transition(StateFailed); err == nil {
		window.LastError = appErr
	}
}

func (e *Engine) RetryWindow(index int) *model.AppError {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.Windows) {
		return model.StateError(index, "窗口编号越界")
	}
	for i := 0; i < index; i++ {
		if e.Windows[i].State != StateCommitted {
			return model.StateError(index, "前序窗口尚未提交，不能重试当前窗口")
		}
	}
	window := e.Windows[index]
	if window.State != StateFailed {
		return model.StateError(index, "只有失败窗口可以重试")
	}
	return window.Transition(StatePending)
}

func (e *Engine) UpdateUncommittedParams(params model.Params) *model.AppError {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := schema.ValidateParams(params); err != nil {
		return err
	}
	if params.Quantization != e.Project.Meta.Precision {
		return model.ParamError("quantization",
			"已加载场景的量化精度不可原地修改；请重新生成场景")
	}
	for _, window := range e.Windows {
		if window.State != StateCommitted {
			continue
		}
		if window.Frame != nil && window.Frame.ParamsHash == params.Hash() {
			continue
		}
		// Committed windows are intentionally frozen. A later parameter change
		// is only applied to work that has not been committed.
	}
	pool, err := NewPool(params)
	if err != nil {
		return err
	}
	e.Params = params
	e.pool = pool
	return nil
}

func (e *Engine) UpdateUncommittedCamera(window int, camera model.CameraSample) *model.AppError {
	e.mu.Lock()
	defer e.mu.Unlock()
	if window < 0 || window >= len(e.Windows) {
		return model.StateError(window, "窗口编号越界")
	}
	target := e.Windows[window]
	if target.State == StateCommitted {
		return model.StateError(window, "已提交窗口的相机轨迹消费偏移已冻结，不能修改")
	}
	if target.State == StateCulling || target.State == StateLoading {
		return model.StateError(window, "窗口正在剔除或加载，不能修改相机")
	}
	camera.Window = window
	if window > 0 && camera.Time <= e.Project.World.Cameras[window-1].Time {
		return model.SchemaError(0, fmt.Sprintf("cameras[%d].time", window),
			"调整后的采样时刻必须大于前一窗口")
	}
	if window+1 < len(e.Project.World.Cameras) && camera.Time >= e.Project.World.Cameras[window+1].Time {
		return model.SchemaError(0, fmt.Sprintf("cameras[%d].time", window),
			"调整后的采样时刻必须小于后一窗口")
	}
	if camera.Right == (model.Vec3{}) || camera.Up == (model.Vec3{}) ||
		camera.Forward == (model.Vec3{}) || camera.Near <= 0 || camera.Far <= camera.Near {
		return model.SchemaError(0, fmt.Sprintf("cameras[%d]", window), "相机内外参非法")
	}
	e.Project.World.Cameras[window] = camera
	if window < len(e.Project.Truth.Samples) {
		e.Project.Truth.Samples[window].Time = camera.Time
	}
	target.Frame = nil
	if target.State == StateFailed {
		if err := target.Transition(StatePending); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Restore(snapshots []Snapshot) *model.AppError {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(snapshots) > len(e.Windows) {
		return model.VersionError("检查点窗口数超过场景窗口数")
	}
	for i, snapshot := range snapshots {
		if snapshot.Window != i || snapshot.Frame == nil {
			return model.VersionError("检查点窗口顺序不连续")
		}
		if snapshot.Frame.Window != i || snapshot.Frame.SceneHash != e.SceneHash ||
			snapshot.SceneHash != e.SceneHash {
			return model.VersionError(fmt.Sprintf("窗口 %d 的场景哈希不匹配，快照整体作废", i))
		}
		frame := cloneFrame(snapshot.Frame)
		if frame == nil {
			return model.VersionError(fmt.Sprintf("窗口 %d 检查点无法解码", i))
		}
		e.Windows[i].Frame = frame
		e.Windows[i].State = StateCommitted
		e.Windows[i].History = []TransitionEvent{{
			From: "", To: StateCommitted,
		}}
		e.resident = append([]int(nil), frame.Resident...)
	}
	if len(snapshots) > 0 {
		last := snapshots[len(snapshots)-1]
		e.Params = last.Params
		pool, err := NewPool(last.Params)
		if err != nil {
			return err
		}
		e.pool = pool
	}
	return nil
}

func (e *Engine) Progress() (committed, failed, total int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, window := range e.Windows {
		switch window.State {
		case StateCommitted:
			committed++
		case StateFailed:
			failed++
		}
	}
	return committed, failed, len(e.Windows)
}

func (e *Engine) BudgetEvents() []BudgetEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pool.Events()
}

func (e *Engine) Snapshot(index int) (Snapshot, *model.AppError) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.Windows) || e.Windows[index].State != StateCommitted {
		return Snapshot{}, model.StateError(index, "窗口尚未提交")
	}
	return Snapshot{
		Window:        index,
		Frame:         cloneFrame(e.Windows[index].Frame),
		Params:        e.Params,
		SceneHash:     e.SceneHash,
		ParamsHash:    e.Windows[index].Frame.ParamsHash,
		ParamsVersion: e.Windows[index].Frame.ParamsVersion,
	}, nil
}

func (e *Engine) WindowView(index int) (*Window, *model.AppError) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.Windows) {
		return nil, model.StateError(index, "窗口编号越界")
	}
	copy := *e.Windows[index]
	return &copy, nil
}

func (e *Engine) CommittedFrames() []*WindowFrame {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*WindowFrame, 0, len(e.Windows))
	for _, window := range e.Windows {
		if window.State == StateCommitted && window.Frame != nil {
			out = append(out, cloneFrame(window.Frame))
		}
	}
	return out
}
