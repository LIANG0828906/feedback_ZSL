package pipeline

import (
	"context"
	"encoding/binary"
	"math"
	"mcapture/internal/dsp"
	"mcapture/internal/session"
	"mcapture/internal/store"
)

// Mode 标识三条处理路径。
type Mode string

const (
	ModeFull     Mode = "full"     // 一次性全量重算
	ModeResume   Mode = "resume"   // 断点恢复续跑
	ModeRealtime Mode = "realtime" // 实时采集（分段流式到达）
)

// Event 是调度过程中的可观测事件（预算阻塞、状态迁移、提交等）。
type Event struct {
	Kind    string
	Channel string
	Segment int
	Detail  string
}

// Engine 承载一次会话处理的全部状态。
type Engine struct {
	Sess *session.Session
	Cfg  AlgoConfig
	st   *store.Store

	tpl      []float64
	segments map[string][]*Segment // channelID -> 分段
	drift    map[string]DriftParams
	refMarks []markerObs
	refOut   []float64

	events []Event
	jobLog []string // 确定性提交序列（与线程数无关）
}

// NewEngine 构造引擎；store 可为 nil（纯计算场景，测试用）。
func NewEngine(sess *session.Session, cfg AlgoConfig, st *store.Store) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := Preflight(sess); err != nil {
		return nil, err
	}
	e := &Engine{
		Sess:     sess,
		Cfg:      cfg,
		st:       st,
		tpl:      dsp.MarkerTemplate(sess.Manifest.MarkerShape, sess.Manifest.MarkerWidth),
		segments: map[string][]*Segment{},
		drift:    map[string]DriftParams{},
	}
	return e, nil
}

// Events 返回累积的调度事件快照。
func (e *Engine) Events() []Event {
	out := make([]Event, len(e.events))
	copy(out, e.events)
	return out
}

// CommitLog 返回确定性提交序列。
func (e *Engine) CommitLog() []string {
	out := make([]string, len(e.jobLog))
	copy(out, e.jobLog)
	return out
}

func (e *Engine) log(kind, ch string, seg int, detail string) {
	e.events = append(e.events, Event{Kind: kind, Channel: ch, Segment: seg, Detail: detail})
}

// segmentBounds 计算通道无关的分段边界（最后一段吸收余数）。
func (e *Engine) segmentBounds() [][2]int64 {
	core := e.Sess.Manifest.CorePoints
	segLen := e.Cfg.SegmentSamples
	var bounds [][2]int64
	for start := int64(0); start < core; start += segLen {
		end := start + segLen
		if end > core {
			end = core
		}
		bounds = append(bounds, [2]int64{start, end})
	}
	return bounds
}

// estimate 执行全局标记检测与漂移估计（不依赖分段，三条路径共用）。
func (e *Engine) estimate() error {
	m := e.Sess.Manifest
	ref := e.Sess.Channels[e.Sess.RefIndex]
	refMarks, err := detectAllMarkers(ref.Data, e.tpl, m.Margin, m.CorePoints)
	if err != nil {
		return err
	}
	if len(refMarks) < 3 {
		return session.NewError(session.EMark, "too few markers on reference channel").
			With(ref.Spec.ID, "marker").WithMarker(len(refMarks))
	}
	e.refMarks = refMarks
	e.drift[ref.Spec.ID] = DriftParams{
		Knots: zeroKnots(m.CorePoints, e.Cfg.SegmentSamples), MarkersUsed: len(refMarks),
	}
	for ci, ch := range e.Sess.Channels {
		if ci == e.Sess.RefIndex {
			continue
		}
		obs, err := detectChannelMarkers(ch.Data, e.tpl, m.Margin, refMarks, ch.Spec.ID)
		if err != nil {
			return err
		}
		params, err := EstimateDrift(obs, m.CorePoints, m.Margin, e.Cfg.SegmentSamples)
		if err != nil {
			return err.(*session.Error).With(ch.Spec.ID, "drift")
		}
		if err := e.validateDriftRate(ch.Spec.ID, params); err != nil {
			return err
		}
		if err := e.checkAgainstTruth(ch.Spec, obs, params); err != nil {
			return err
		}
		e.drift[ch.Spec.ID] = params
	}
	return nil
}

func zeroKnots(core, segLen int64) []session.Knot {
	numSeg := int((core + segLen - 1) / segLen)
	knots := make([]session.Knot, 0, numSeg+1)
	for s := 0; s <= numSeg; s++ {
		at := int64(s) * segLen
		if at > core {
			at = core
		}
		knots = append(knots, session.Knot{At: at})
	}
	return knots
}

func (e *Engine) validateDriftRate(chID string, p DriftParams) error {
	for _, k := range p.Knots {
		if math.Abs(k.Rate) > e.Cfg.MaxDriftRate {
			return session.NewError(session.EPara, "estimated drift rate out of allowed range").
				With(chID, "max_drift_rate")
		}
	}
	return nil
}

// checkAgainstTruth 用清单真值做验收级核对；仅比对估计值，绝不从真值生成采样。
func (e *Engine) checkAgainstTruth(spec session.ChannelSpec, obs []markerObs, p DriftParams) error {
	if len(spec.Markers) == 0 {
		return nil
	}
	truth := map[int]session.MarkerTruth{}
	for _, mt := range spec.Markers {
		truth[mt.Index] = mt
	}
	for _, o := range obs {
		mt, ok := truth[o.index]
		if !ok {
			continue
		}
		coreN := int64(math.Round(o.coreRef))
		modelArrival := float64(e.Sess.Manifest.Margin) + float64(coreN) + p.PhiAt(coreN)
		if math.Abs(modelArrival-mt.ArrivalSample) > e.Cfg.Tolerance {
			return session.NewError(session.EDrift,
				"estimated marker offset outside tolerance vs ground truth").
				With(spec.ID, "marker").WithMarker(o.index)
		}
	}
	return nil
}

// buildSegments 初始化分段状态机（已提交分段由恢复流程覆盖）。
func (e *Engine) buildSegments() {
	bounds := e.segmentBounds()
	for _, ch := range e.Sess.Channels {
		list := make([]*Segment, len(bounds))
		for i, b := range bounds {
			list[i] = &Segment{
				ChannelID: ch.Spec.ID, Index: i, Start: b[0], End: b[1], State: StatePending,
			}
		}
		e.segments[ch.Spec.ID] = list
	}
}

// processUnit 是单通道单分段的纯函数式计算，任意线程执行都逐样本一致。
func (e *Engine) processUnit(chID string, segIdx int) (*Segment, error) {
	ch := e.Sess.Manifest.ChannelByID(chID)
	chData := e.channelData(chID)
	seg := e.segments[chID][segIdx]
	if seg.IsFrozen() {
		return seg, nil
	}
	if err := seg.Transition(StateAligned); err != nil {
		return nil, err
	}
	m := e.Sess.Manifest
	var aligned []float64

	if ch.IsRef {
		// 参考通道：直接取核心窗口，无重采样，但仍走同一长度与边界约束。
		aligned = chData[m.Margin : m.Margin+m.CorePoints]
	} else {
		full, aerr := AlignChannel(chData, e.drift[chID], m.Margin, m.CorePoints, e.Cfg)
		if aerr != nil {
			_ = seg.Transition(StateFailed)
			seg.FailCode = session.CodeOf(aerr)
			seg.FailReason = aerr.Error()
			return nil, aerr
		}
		aligned = full
	}
	slice := aligned[seg.Start:seg.End]
	seg.Output = append([]float64(nil), slice...)
	seg.Drift = e.drift[chID]
	seg.Hash = session.HashRaw(seg.Output)
	if !ch.IsRef {
		stats, rerr := CheckResidual(aligned, e.refOut, e.tpl, m.MarkerWidth,
			e.markersInSegment(segIdx), e.Cfg.ResidualBound, chID)
		if rerr != nil {
			_ = seg.Transition(StateFailed)
			seg.FailCode = session.CodeOf(rerr)
			seg.FailReason = rerr.Error()
			return nil, rerr
		}
		seg.Residuals = stats
	}
	return seg, nil
}

func (e *Engine) markersInSegment(segIdx int) []markerObs {
	bounds := e.segmentBounds()
	b := bounds[segIdx]
	var out []markerObs
	for _, rm := range e.refMarks {
		n := int64(math.Round(rm.coreRef))
		if n >= b[0] && n < b[1] {
			out = append(out, rm)
		}
	}
	return out
}

func (e *Engine) channelData(chID string) []float64 {
	for _, ch := range e.Sess.Channels {
		if ch.Spec.ID == chID {
			return ch.Data
		}
	}
	return nil
}

// assembleOutput 拼装全通道核心长度输出（按清单通道顺序）。
func (e *Engine) assembleOutput() [][]float64 {
	core := e.Sess.Manifest.CorePoints
	out := make([][]float64, len(e.Sess.Channels))
	for ci, ch := range e.Sess.Channels {
		buf := make([]float64, core)
		for _, seg := range e.segments[ch.Spec.ID] {
			copy(buf[seg.Start:seg.End], seg.Output)
		}
		out[ci] = buf
	}
	return out
}

// encodeF64 把一段输出编码为小端 float64 字节。
func encodeF64(x []float64) []byte {
	b := make([]byte, 8*len(x))
	for i, v := range x {
		binary.LittleEndian.PutUint64(b[i*8:], math.Float64bits(v))
	}
	return b
}

// Progress 返回已提交分段数与总分段数。
func (e *Engine) Progress() (committed, total int) {
	for _, list := range e.segments {
		for _, seg := range list {
			total++
			if seg.IsFrozen() {
				committed++
			}
		}
	}
	return
}

// Snapshot 返回各通道分段当前状态。
func (e *Engine) Snapshot() map[string][]*Segment { return e.segments }

// DriftParams 返回某通道的估计漂移模型。
func (e *Engine) DriftParams(chID string) (DriftParams, bool) {
	p, ok := e.drift[chID]
	return p, ok
}

// Run 执行完整处理。workers=1 为串行路径；workers>1 走预算受控线程池。
func (e *Engine) Run(ctx context.Context, mode Mode, workers int) error {
	if err := e.estimate(); err != nil {
		return err
	}
	e.buildSegments()
	m := e.Sess.Manifest
	// 参考全通道对齐结果用于残差核对。
	refCh := e.Sess.Channels[e.Sess.RefIndex]
	e.refOut = refCh.Data[m.Margin : m.Margin+m.CorePoints]

	if mode == ModeResume {
		if err := e.restore(); err != nil {
			return err
		}
	}
	if err := e.schedule(ctx, workers, mode); err != nil {
		return err
	}
	return nil
}
