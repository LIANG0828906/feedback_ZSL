package streaming

import (
	"occlbench/internal/core"
	"occlbench/internal/culling"
	"occlbench/internal/scene"
)

// Config 流式处理配置。
type Config struct {
	WindowSize int            `json:"windowSize"` // 每个窗口覆盖的采样时刻数
	Budget     Budget         `json:"budget"`
	CullParams culling.Params `json:"cullParams"`
}

// Validate 校验配置，参数问题分别返回 E_PARA / E_BUDGET。
func (c Config) Validate() error {
	if c.WindowSize <= 0 {
		return core.NewError(core.EPara, "windowSize must be positive",
			map[string]any{"field": "windowSize", "value": c.WindowSize})
	}
	if err := c.Budget.Validate(); err != nil {
		return err
	}
	return c.CullParams.Validate()
}

// Engine 串行推进窗口的流式引擎。其输出仅取决于世界、轨迹与配置，
// 不依赖线程标识、完成顺序或历史判定，是三路径一致性的共同实现。
type Engine struct {
	world    *scene.World
	cfg      Config
	culler   *culling.Engine
	sched    *Scheduler
	windows  []*Window
	resident []int
}

// NewEngine 构造流式引擎并预计算全部采样时刻的确定性剔除结果。
func NewEngine(w *scene.World, cfg Config) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	sched, err := NewScheduler(w, cfg.Budget)
	if err != nil {
		return nil, err
	}
	return &Engine{
		world:  w,
		cfg:    cfg,
		culler: culling.NewEngine(),
		sched:  sched,
	}, nil
}

// BuildWindows 依据窗口大小切分采样时刻并聚合剔除结果（导出供编排层复用）。
func BuildWindows(w *scene.World, windowSize int, samples []culling.SampleResult) []*Window {
	total := len(w.Trajectory.Samples)
	windows := make([]*Window, 0)
	idx := 0
	for start := 0; start < total; start += windowSize {
		end := start + windowSize
		if end > total {
			end = total
		}
		windows = append(windows, aggregateWindow(idx, start, end, samples))
		idx++
	}
	return windows
}

// RunAll 一次性按窗口推进全量重放，返回全部窗口结果。
func (e *Engine) RunAll() ([]*Window, error) {
	cullResult, err := e.culler.Run(e.world, e.cfg.CullParams)
	if err != nil {
		return nil, err
	}
	e.windows = BuildWindows(e.world, e.cfg.WindowSize, cullResult.Samples)
	e.resident = []int{}
	for _, win := range e.windows {
		if err := e.processWindow(win, cullResult.Samples); err != nil {
			return e.windows, err
		}
	}
	return e.windows, nil
}

// ProcessWindow 推进单个窗口并更新传入的驻留集合（导出供编排层复用同一逻辑）。
func (e *Engine) ProcessWindow(win *Window, samples []culling.SampleResult, resident *[]int) error {
	e.resident = append((*resident)[:0], (*resident)...)
	if err := e.processWindow(win, samples); err != nil {
		return err
	}
	*resident = append((*resident)[:0], e.resident...)
	return nil
}

// processWindow 推进单个窗口经历 pending->culling->loading->committed。
// 任意阶段异常都置窗口 failed 并返回，不污染其他窗口。
func (e *Engine) processWindow(win *Window, samples []culling.SampleResult) error {
	var err error
	if win.State, err = Transition(win.State, StateCulling); err != nil {
		return err
	}
	// 剔除结果已由纯函数引擎预计算；这里只做窗口聚合的一致性确认。
	if len(win.Visible) == 0 && len(win.Occluded) == 0 {
		win.FailureCause = "empty cull result"
		win.State, _ = Transition(win.State, StateFailed)
		return core.NewError(core.EIO, "window culling produced no chunks",
			map[string]any{"window": win.Index})
	}
	if win.State, err = Transition(win.State, StateLoading); err != nil {
		return err
	}

	camPos := e.world.Trajectory.Samples[win.SampleStart].Pos
	sched := e.sched.Plan(win.Index, e.resident, win.Visible, camPos)
	win.LoadList = append(win.LoadList[:0], idsOf(sched.Loads)...)
	win.UnloadList = append(win.UnloadList[:0], idsOf(sched.Unloads)...)
	win.Delayed = sched.Delayed
	win.Resident = sched.Resident
	e.resident = append(e.resident[:0], sched.Resident...)
	win.ConsumedOffset = win.SampleEnd

	if win.State, err = Transition(win.State, StateCommitted); err != nil {
		return err
	}
	win.Freeze()
	return nil
}

// RetryWindow 重试失败窗口：failed -> pending 后重新走完整流程。
func (e *Engine) RetryWindow(index int, samples []culling.SampleResult) error {
	if index < 0 || index >= len(e.windows) {
		return core.NewError(core.ERef, "window index out of range", map[string]any{"window": index})
	}
	win := e.windows[index]
	if win.State != StateFailed {
		return core.NewError(core.EState, "only failed window can retry",
			map[string]any{"window": index, "state": string(win.State)})
	}
	var err error
	if win.State, err = Transition(win.State, StatePending); err != nil {
		return err
	}
	win.FailureCause = ""
	return e.processWindow(win, samples)
}

// Windows 返回当前窗口切片。
func (e *Engine) Windows() []*Window { return e.windows }

// SetWindows 注入窗口切片（用于断点恢复/重试场景复用引擎状态机）。
func (e *Engine) SetWindows(windows []*Window) { e.windows = windows }

// Progress 返回已提交窗口数与总窗口数。
func (e *Engine) Progress() (committed, total int) {
	for _, win := range e.windows {
		if win.State == StateCommitted {
			committed++
		}
	}
	return committed, len(e.windows)
}

func idsOf(actions []Action) []int {
	out := make([]int, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.ChunkID)
	}
	return out
}
