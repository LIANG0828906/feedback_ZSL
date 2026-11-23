package culling

import (
	"context"
	"sort"

	"occlbench/internal/core"
	"occlbench/internal/scene"
	"occlbench/internal/worker"
)

// Params 剔除参数。预览路径降低 GridResolution 并设置 SampleStride>1。
type Params struct {
	// GridResolution 屏幕覆盖栅格边长（像素格），必须为正。
	GridResolution int `json:"gridResolution"`
	// SampleStride 采样时刻抽样步长，1 表示全部采样时刻（精确路径）。
	SampleStride int            `json:"sampleStride"`
	Quant        QuantPrecision `json:"quant"`
	// Preview 标记是否为粗粒度预览，仅用于结果自描述。
	Preview bool `json:"preview"`
}

// Validate 参数越界返回 E_PARA 并指出字段名。
func (p Params) Validate() error {
	if p.GridResolution <= 0 {
		return core.NewError(core.EPara, "gridResolution must be positive",
			map[string]any{"field": "gridResolution", "value": p.GridResolution})
	}
	if p.SampleStride <= 0 {
		return core.NewError(core.EPara, "sampleStride must be positive",
			map[string]any{"field": "sampleStride", "value": p.SampleStride})
	}
	return p.Quant.Validate()
}

// SampleRatio 返回记录用的抽样比（抽样时刻数 / 总时刻数）。
func (p Params) SampleRatio(total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(len(p.SampleIndices(total))) / float64(total)
}

// SampleIndices 返回该参数下实际计算的采样时刻下标，始终包含最后一个时刻。
func (p Params) SampleIndices(total int) []int {
	idx := make([]int, 0)
	for i := 0; i < total; i += p.SampleStride {
		idx = append(idx, i)
	}
	if total > 0 && (len(idx) == 0 || idx[len(idx)-1] != total-1) {
		idx = append(idx, total-1)
	}
	return idx
}

// SampleResult 单个采样时刻的剔除结果。
type SampleResult struct {
	SampleIndex int           `json:"sampleIndex"`
	T           float64       `json:"t"`
	Visible     []int         `json:"visible"`
	Occluded    []int         `json:"occluded"`
	Basis       map[int][]int `json:"basis,omitempty"`
	GridSummary string        `json:"gridSummary"`
}

// Result 一次剔除运行的完整结果，纯函数输出，不含线程/历史状态。
type Result struct {
	Params         Params         `json:"params"`
	Samples        []SampleResult `json:"samples"`
	GridResolution int            `json:"gridResolution"`
	SampleRatio    float64        `json:"sampleRatio"`
	QuantVersion   string         `json:"quantVersion"`
	AlgoVersion    string         `json:"algoVersion"`
}

// Engine 无状态剔除引擎。所有方法对相同输入产出相同结果。
type Engine struct{}

// NewEngine 构造剔除引擎。
func NewEngine() *Engine { return &Engine{} }

type qchunk struct {
	ch       scene.Chunk
	quantBox core.AABB
	corners  [8]core.Vec3
}

type precomputed struct{ boxes []qchunk }

func (e *Engine) precompute(w *scene.World, params Params) precomputed {
	chunks := w.AllChunks()
	qc := make([]qchunk, len(chunks))
	for i, ch := range chunks {
		qb, corners := QuantizedCorners(ch.Box, params.Quant)
		qc[i] = qchunk{ch: ch, quantBox: qb, corners: corners}
	}
	return precomputed{boxes: qc}
}

// Run 串行执行剔除。预览/精确仅通过 Params 区分，共享视锥提取与量化。
func (e *Engine) Run(w *scene.World, params Params) (*Result, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	pre := e.precompute(w, params)
	indices := params.SampleIndices(len(w.Trajectory.Samples))
	out := e.newResult(w, params, len(indices))
	for _, si := range indices {
		out.Samples = append(out.Samples, e.computeSample(w, params, pre, si))
	}
	return out, nil
}

// RunParallel 使用有界线程池并行剔除，逐采样按固定索引回填。
// 单采样判定为纯函数，结果与 Run 完全一致，不受线程标识/完成顺序影响。
func (e *Engine) RunParallel(ctx context.Context, w *scene.World, params Params, pool *worker.Pool) (*Result, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	if err := pool.CheckRasterBuffer(params.GridResolution); err != nil {
		return nil, err
	}
	pre := e.precompute(w, params)
	indices := params.SampleIndices(len(w.Trajectory.Samples))
	out := e.newResult(w, params, len(indices))
	out.Samples = make([]SampleResult, len(indices))
	err := pool.Parallel(ctx, len(indices), func(k int) error {
		out.Samples[k] = e.computeSample(w, params, pre, indices[k])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (e *Engine) newResult(w *scene.World, params Params, n int) *Result {
	return &Result{
		Params:         params,
		Samples:        make([]SampleResult, 0, n),
		GridResolution: params.GridResolution,
		SampleRatio:    params.SampleRatio(len(w.Trajectory.Samples)),
		QuantVersion:   params.Quant.Version,
		AlgoVersion:    scene.AlgorithmVersion,
	}
}

func (e *Engine) computeSample(w *scene.World, params Params, pre precomputed, si int) SampleResult {
	cam := w.Trajectory.Samples[si]
	frustum := ExtractFrustum(cam)
	// 1) 视锥剔除得到候选分块。
	pass := make([]int, 0, len(pre.boxes))
	for i, item := range pre.boxes {
		if BoxInFrustum(frustum, item.corners) {
			pass = append(pass, i)
		}
	}
	// 2) 遮挡体栅格化，生成覆盖栅格与最近深度。
	grid := rasterizeOccluders(frustum, w.Occluders, params)
	visible := make([]int, 0, len(pass))
	occluded := make([]int, 0)
	basis := map[int][]int{}
	for _, i := range pass {
		item := pre.boxes[i]
		hits := occludedBy(frustum, item.quantBox, w.Occluders, grid)
		if len(hits) > 0 {
			occluded = append(occluded, item.ch.ID)
			basis[item.ch.ID] = hits
		} else {
			visible = append(visible, item.ch.ID)
		}
	}
	sort.Ints(visible)
	sort.Ints(occluded)
	return SampleResult{
		SampleIndex: si,
		T:           cam.T,
		Visible:     visible,
		Occluded:    occluded,
		Basis:       basis,
		GridSummary: grid.summary(),
	}
}

// DefaultPreciseParams 全分辨率栅格 + 全部采样时刻。
func DefaultPreciseParams() Params {
	return Params{GridResolution: 64, SampleStride: 1, Quant: DefaultQuantPrecision(), Preview: false}
}

// DefaultPreviewParams 低分辨率栅格 + 抽样采样时刻。
func DefaultPreviewParams() Params {
	return Params{GridResolution: 16, SampleStride: 4, Quant: DefaultQuantPrecision(), Preview: true}
}

// PreviewMeta 记录预览所用栅格分辨率与抽样比，供工作台展示与持久化。
type PreviewMeta struct {
	GridResolution int     `json:"gridResolution"`
	SampleStride   int     `json:"sampleStride"`
	SampleRatio    float64 `json:"sampleRatio"`
}

// Meta 从参数与轨迹长度生成预览元数据。
func (p Params) Meta(total int) PreviewMeta {
	return PreviewMeta{
		GridResolution: p.GridResolution,
		SampleStride:   p.SampleStride,
		SampleRatio:    p.SampleRatio(total),
	}
}

var _ = core.EPara
