package engine

import (
	"fmt"

	"github.com/gameaudio/workbench/internal/acoustics"
	"github.com/gameaudio/workbench/internal/geometry"
	"github.com/gameaudio/workbench/internal/model"
)

// AlgorithmVersion 标识计算算法版本，改变几何或声学公式必须提升该值，
// 持久化快照在版本不匹配时整体作废。
const AlgorithmVersion = "algo-1.0.0"

// Engine 绑定一个工程与定点几何引擎，提供单对计算能力。
type Engine struct {
	project *model.Project
	geo     *geometry.Engine
	regions map[string]model.Region
	sources map[string]model.Source
	listen  map[string]model.Listener
}

// New 构造计算引擎。
func New(p *model.Project, cfg model.GeoConfig) (*Engine, error) {
	geo, err := geometry.New(cfg, p.Walls)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		project: p,
		geo:     geo,
		regions: map[string]model.Region{},
		sources: map[string]model.Source{},
		listen:  map[string]model.Listener{},
	}
	for _, r := range p.Regions {
		e.regions[r.ID] = r
	}
	for _, s := range p.Sources {
		e.sources[s.ID] = s
	}
	for _, l := range p.Listeners {
		e.listen[l.ID] = l
	}
	return e, nil
}

// PairKey 返回声源-听者对的稳定键。
func PairKey(sourceID, listenerID string) string {
	return sourceID + "->" + listenerID
}

// ComputePair 计算单个声源-听者对。任何异常都被收敛进 Result.Failed，
// 不向上 panic，从而保证单对失败不污染其他对。
func (e *Engine) ComputePair(sourceID, listenerID string) model.Result {
	s, okS := e.sources[sourceID]
	l, okL := e.listen[listenerID]
	res := model.Result{SourceID: sourceID, ListenerID: listenerID, RegionID: s.RegionID}
	if !okS || !okL {
		res.Failed = true
		res.ErrorCode = "E_REF"
		res.ErrorDetail = fmt.Sprintf("声源或听者不存在: %s->%s", sourceID, listenerID)
		return res
	}

	// 几何判定：定点遮挡与绕射。
	fS := e.geo.Quant(s.Pos)
	fL := e.geo.Quant(l.Pos)
	blocking := e.geo.BlockingWalls(fS, fL)
	res.Occluded = len(blocking) > 0
	res.WallCount = len(blocking)
	res.DirectLen = e.geo.Distance(fS, fL)

	effective := res.DirectLen
	if res.Occluded {
		diff, err := e.geo.Diffraction(fS, fL, blocking)
		if err != nil {
			res.Failed = true
			res.ErrorCode = "E_PATH"
			res.ErrorDetail = err.Error()
			res.DiffractionLen = 0
			return res
		}
		res.DiffractionLen = diff
		effective = diff
	}
	res.EffectiveLen = effective

	// 声学合成：仅依赖区域参数、有效长度与遮挡布尔值。
	region := e.regions[s.RegionID]
	out := acoustics.Synthesize(acoustics.Input{
		Region:         region,
		EffectiveLen:   effective,
		Occluded:       res.Occluded,
		DiffractionLen: res.DiffractionLen,
	})
	res.Curve = out.Curve
	res.SPL = out.SPL
	return res
}

// Project 返回绑定工程。
func (e *Engine) Project() *model.Project { return e.project }

// Geo 返回定点几何引擎。
func (e *Engine) Geo() *geometry.Engine { return e.geo }
