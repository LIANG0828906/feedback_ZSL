package pipeline

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"

	"mcapture/internal/session"
	"mcapture/internal/store"
)

// restore 从 SQLite 恢复已提交分段。
// 任一内容哈希、参数版本或算法版本不匹配，则整体快照作废并全量重算，
// 禁止部分混用旧分段。
func (e *Engine) restore() error {
	if e.st == nil {
		return nil
	}
	fp, err := e.st.GetMeta("session_fingerprint")
	if err != nil {
		return err
	}
	if fp != "" && fp != e.Sess.Manifest.Fingerprint() {
		e.log("snapshot_invalidated", "", -1, "session fingerprint mismatch -> full recompute")
		return e.st.Reset()
	}
	pv, err := e.st.GetMeta("param_version")
	if err != nil {
		return err
	}
	if pv != "" && pv != e.Cfg.ParamVersion {
		e.log("snapshot_invalidated", "", -1, "param version mismatch -> full recompute")
		return e.st.Reset()
	}
	av, err := e.st.GetMeta("algo_version")
	if err != nil {
		return err
	}
	if av != "" && av != e.Cfg.AlgoVersion {
		e.log("snapshot_invalidated", "", -1, "algo version mismatch -> full recompute")
		return e.st.Reset()
	}
	recs, order, err := e.st.LoadAll()
	if err != nil {
		return err
	}
	if len(order) == 0 {
		return nil
	}
	bounds := e.segmentBounds()
	// 先全量校验，任何一条不匹配都整体作废。
	type loaded struct {
		seg *Segment
	}
	loadedMap := map[string]map[int]*Segment{}
	for chID, byIdx := range recs {
		loadedMap[chID] = map[int]*Segment{}
		for idx, r := range byIdx {
			if r.SchemaVersion != store.SchemaVersion ||
				r.ParamVersion != e.Cfg.ParamVersion ||
				r.AlgoVersion != e.Cfg.AlgoVersion {
				e.log("snapshot_invalidated", chID, idx, "version mismatch -> full recompute")
				return e.st.Reset()
			}
			if idx >= len(bounds) || r.Start != bounds[idx][0] || r.End != bounds[idx][1] {
				e.log("snapshot_invalidated", chID, idx, "segment bounds mismatch -> full recompute")
				return e.st.Reset()
			}
			samples, ok := decodeF64(r.Output)
			if !ok || len(samples) != int(r.End-r.Start) {
				e.log("snapshot_invalidated", chID, idx, "output undecodable -> full recompute")
				return e.st.Reset()
			}
			if session.HashRaw(samples) != r.ContentHash {
				e.log("snapshot_invalidated", chID, idx, "content hash mismatch -> full recompute")
				return e.st.Reset()
			}
			var drift DriftParams
			if err := json.Unmarshal([]byte(r.DriftJSON), &drift); err != nil {
				e.log("snapshot_invalidated", chID, idx, "drift json invalid -> full recompute")
				return e.st.Reset()
			}
			loadedMap[chID][idx] = &Segment{
				ChannelID: chID, Index: idx, Start: r.Start, End: r.End,
				State: StateCommitted, Drift: drift, Output: samples, Hash: r.ContentHash,
			}
		}
	}
	// 全部匹配：把已提交分段注入状态机，未命中的分段保持 pending。
	for chID, byIdx := range loadedMap {
		for idx, seg := range byIdx {
			e.segments[chID][idx] = seg
			e.log("restored", chID, idx, "committed segment reused from checkpoint")
		}
	}
	return nil
}

func decodeF64(b []byte) ([]float64, bool) {
	if len(b)%8 != 0 {
		return nil, false
	}
	out := make([]float64, len(b)/8)
	for i := range out {
		v := math.Float64frombits(binary.LittleEndian.Uint64(b[i*8:]))
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// RetrySegment 重试失败分段：失败 -> 待估计，再重新计算并提交。
// 单段异常不污染其他段。
func (e *Engine) RetrySegment(chID string, segIdx int) error {
	seg := e.segments[chID][segIdx]
	if seg.State != StateFailed {
		return session.NewError(session.EState,
			"only failed segments can be retried").
			With(chID, "state").WithSegment(segIdx)
	}
	if err := seg.Transition(StatePending); err != nil {
		return err
	}
	redone, err := e.processUnit(chID, segIdx)
	if err != nil {
		return err
	}
	return e.commit(redone, ModeResume)
}

// EditDrift 编辑漂移曲线，只允许作用于未提交分段；触碰已提交分段即拒绝。
func (e *Engine) EditDrift(chID string, knotIdx int, newRate float64) error {
	if math.Abs(newRate) > e.Cfg.MaxDriftRate {
		return session.NewError(session.EPara, "edited drift rate out of range").
			With(chID, "max_drift_rate")
	}
	for _, seg := range e.segments[chID] {
		if seg.IsFrozen() && knotIdx >= seg.Index && knotIdx <= seg.Index+1 {
			return session.NewError(session.EState,
				"cannot edit drift of a committed (frozen) segment").
				With(chID, "state").WithSegment(seg.Index)
		}
	}
	p := e.drift[chID]
	if knotIdx < 0 || knotIdx >= len(p.Knots) {
		return session.NewError(session.EPara, "knot index out of range").
			With(chID, "drift_basis")
	}
	p.Knots[knotIdx].Rate = newRate
	e.drift[chID] = p
	return nil
}

// Result 是一次完整运行的产物。
type Result struct {
	Mode      Mode
	Outputs   [][]float64 // 与清单通道顺序一致，长度均为 CorePoints
	Drift     map[string]DriftParams
	Residuals map[string][]ResidualStats
	Segments  map[string][]*Segment
	CommitSeq []string
	Events    []Event
	Progress  [2]int
}

// Collect 在运行结束后收集确定性结果。
func (e *Engine) Collect(mode Mode) *Result {
	r := &Result{
		Mode:      mode,
		Outputs:   e.assembleOutput(),
		Drift:     map[string]DriftParams{},
		Residuals: map[string][]ResidualStats{},
		Segments:  e.segments,
		CommitSeq: e.CommitLog(),
		Events:    e.Events(),
	}
	committed, total := e.Progress()
	r.Progress = [2]int{committed, total}
	for id, p := range e.drift {
		r.Drift[id] = p
	}
	for chID, list := range e.segments {
		for _, seg := range list {
			r.Residuals[chID] = append(r.Residuals[chID], seg.Residuals...)
		}
	}
	return r
}

// ExportMultiChannel 把结果导出为多声道文件：每通道一个 .f64 与一个总清单。
func ExportMultiChannel(dir string, sess *session.Session, r *Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for ci, ch := range sess.Channels {
		path := filepath.Join(dir, ch.Spec.ID+".aligned.f64")
		if err := os.WriteFile(path, encodeF64(r.Outputs[ci]), 0o644); err != nil {
			return err
		}
	}
	manifest := map[string]any{
		"session_id":    sess.Manifest.SessionID,
		"sample_rate":   sess.Manifest.SampleRate,
		"channels":      len(sess.Channels),
		"core_points":   sess.Manifest.CorePoints,
		"encoding":      "float64le",
		"channel_order": channelOrder(sess),
		"commit_seq":    r.CommitSeq,
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "export.json"), data, 0o644)
}

func channelOrder(sess *session.Session) []string {
	out := make([]string, len(sess.Channels))
	for i, ch := range sess.Channels {
		out[i] = ch.Spec.ID
	}
	return out
}
