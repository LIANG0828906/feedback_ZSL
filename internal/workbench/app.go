package workbench

import (
	"context"
	"sync"

	"occlbench/internal/core"
	"occlbench/internal/culling"
	"occlbench/internal/persist"
	"occlbench/internal/scene"
	"occlbench/internal/streaming"
	"occlbench/internal/worker"
)

// App 工作台内存状态，聚合世界、配置、线程池与检查点存储。
type App struct {
	mu      sync.RWMutex
	world   *scene.World
	cfg     streaming.Config
	store   *persist.Store
	pool    *worker.Pool
	dataDir string

	// 最近一次三路径比对与缓存命中统计。
	lastDivergence *Divergence
	cacheHits      int
	cacheMisses    int
	// 最近一次运行各路径窗口结果。
	results map[PathName][]*streaming.Window
	// 最近一次预览结果与元数据。
	preview     *culling.Result
	previewMeta culling.PreviewMeta
}

// NewApp 构造工作台应用。
func NewApp(world *scene.World, cfg streaming.Config, store *persist.Store, dataDir string) (*App, error) {
	pool, err := worker.New(4, 256*256)
	if err != nil {
		return nil, err
	}
	return &App{
		world:   world,
		cfg:     cfg,
		store:   store,
		pool:    pool,
		dataDir: dataDir,
		results: map[PathName][]*streaming.Window{},
	}, nil
}

// World 返回当前世界（只读使用）。
func (a *App) World() *scene.World {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.world
}

// Config 返回当前流式配置的副本。
func (a *App) Config() streaming.Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// UpdateBudget 调整槽位与内存预算，触发后续增量重算（参数校验失败返回 E_BUDGET）。
func (a *App) UpdateBudget(slots int, memory float64) error {
	budget := streaming.Budget{Slots: slots, Memory: memory}
	if err := budget.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg.Budget = budget
	a.results = map[PathName][]*streaming.Window{}
	a.mu.Unlock()
	return nil
}

// UpdateQuantResolution 调整栅格分辨率（量化精度参数族），触发增量重算。
func (a *App) UpdateQuantResolution(gridResolution int) error {
	// 先借用线程池做越界校验，参数非法时不得改变既有配置（返回 E_PARA）。
	if err := a.pool.CheckRasterBuffer(gridResolution); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg.CullParams.GridResolution = gridResolution
	a.results = map[PathName][]*streaming.Window{}
	a.mu.Unlock()
	return nil
}

// RunPreview 执行粗粒度预览路径并记录分辨率与抽样比。
func (a *App) RunPreview() (*culling.Result, culling.PreviewMeta, error) {
	a.mu.Lock()
	w := a.world
	params := culling.DefaultPreviewParams()
	a.mu.Unlock()
	res, err := culling.NewEngine().Run(w, params)
	if err != nil {
		return nil, culling.PreviewMeta{}, err
	}
	meta := params.Meta(len(w.Trajectory.Samples))
	a.mu.Lock()
	a.preview = res
	a.previewMeta = meta
	a.mu.Unlock()
	return res, meta, nil
}

// RunPrecise 执行精确剔除（全分辨率、全部采样时刻）。
func (a *App) RunPrecise(ctx context.Context) (*culling.Result, error) {
	a.mu.RLock()
	w := a.world
	params := a.cfg.CullParams
	a.mu.RUnlock()
	return culling.NewEngine().RunParallel(ctx, w, params, a.pool)
}

// Verify 执行三路径一致性比对并缓存统计。
func (a *App) Verify(ctx context.Context) (map[PathName][]*streaming.Window, *Divergence, []string, error) {
	a.mu.RLock()
	w := a.world
	cfg := a.cfg
	a.mu.RUnlock()

	paths, diverge, err := VerifyThreePaths(ctx, w, cfg, a.store)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		if diverge != nil {
			a.lastDivergence = diverge
		}
		a.cacheMisses++
		return paths, diverge, nil, err
	}
	a.results = paths
	a.lastDivergence = nil
	a.cacheHits++
	return paths, nil, nil, nil
}

// RetryWindow 对指定窗口执行重试（仅失败窗口可重试）。
func (a *App) RetryWindow(index int) error {
	a.mu.RLock()
	cfg := a.cfg
	w := a.world
	a.mu.RUnlock()
	eng, err := streaming.NewEngine(w, cfg)
	if err != nil {
		return err
	}
	cullResult, err := culling.NewEngine().Run(w, cfg.CullParams)
	if err != nil {
		return err
	}
	windows := streaming.BuildWindows(w, cfg.WindowSize, cullResult.Samples)
	if index < 0 || index >= len(windows) {
		return core.NewError(core.ERef, "window index out of range", map[string]any{"window": index})
	}

	eng.SetWindows(windows)
	// 顺序重放前置窗口以重建驻留集合（不触碰检查点，仅内存态）。
	resident := []int{}
	for i := 0; i < index; i++ {
		if err := eng.ProcessWindow(windows[i], cullResult.Samples, &resident); err != nil {
			return err
		}
	}
	// 将目标窗口置于失败态后走合法重试迁移 failed->pending->...。
	windows[index].State = streaming.StateFailed
	windows[index].FailureCause = "simulated failure"
	return eng.RetryWindow(index, cullResult.Samples)
}

// Snapshot 返回供页面渲染的只读快照。
func (a *App) Snapshot() Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	snap := Snapshot{
		WorldName:      a.world.Name,
		Seed:           a.world.Seed,
		ChunkCount:     a.world.ChunkCount(),
		PartitionCount: len(a.world.Partitions),
		SampleCount:    len(a.world.Trajectory.Samples),
		Config:         a.cfg,
		CacheHits:      a.cacheHits,
		CacheMisses:    a.cacheMisses,
		PreviewMeta:    a.previewMeta,
		Divergence:     a.lastDivergence,
		PoolWorkers:    a.pool.MaxWorker(),
	}
	if rt, ok := a.results[PathRealtime]; ok {
		snap.Windows = rt
	}
	return snap
}

// Store 暴露检查点存储（HTTP 导出用）。
func (a *App) Store() *persist.Store { return a.store }

// Pool 暴露线程池。
func (a *App) Pool() *worker.Pool { return a.pool }

// Snapshot 页面渲染数据。
type Snapshot struct {
	WorldName      string              `json:"worldName"`
	Seed           int64               `json:"seed"`
	ChunkCount     int                 `json:"chunkCount"`
	PartitionCount int                 `json:"partitionCount"`
	SampleCount    int                 `json:"sampleCount"`
	Config         streaming.Config    `json:"config"`
	Windows        []*streaming.Window `json:"windows,omitempty"`
	CacheHits      int                 `json:"cacheHits"`
	CacheMisses    int                 `json:"cacheMisses"`
	PreviewMeta    culling.PreviewMeta `json:"previewMeta"`
	Divergence     *Divergence         `json:"divergence,omitempty"`
	PoolWorkers    int                 `json:"poolWorkers"`
}
