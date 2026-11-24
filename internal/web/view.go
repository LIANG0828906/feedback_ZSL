package web

import "mcapture/internal/pipeline"

// segmentView 是分段状态机的可序列化视图。
type segmentView struct {
	Channel    string                   `json:"channel"`
	Segment    int                      `json:"segment"`
	Start      int64                    `json:"start"`
	End        int64                    `json:"end"`
	State      string                   `json:"state"`
	Hash       string                   `json:"hash"`
	FailCode   string                   `json:"fail_code,omitempty"`
	FailReason string                   `json:"fail_reason,omitempty"`
	Residuals  []pipeline.ResidualStats `json:"residuals,omitempty"`
}

func (s *Server) segmentViewLocked() []segmentView {
	var out []segmentView
	for _, ch := range s.sess.Channels {
		for _, seg := range s.last.Segments[ch.Spec.ID] {
			out = append(out, segmentView{
				Channel: seg.ChannelID, Segment: seg.Index, Start: seg.Start, End: seg.End,
				State: seg.State, Hash: seg.Hash, FailCode: seg.FailCode,
				FailReason: seg.FailReason, Residuals: seg.Residuals,
			})
		}
	}
	return out
}

// waveformView 提供对齐前后波形的抽稀视图（每 stride 个点取一个）。
type waveformView struct {
	Channel string    `json:"channel"`
	Stride  int       `json:"stride"`
	Before  []float64 `json:"before"`
	After   []float64 `json:"after"`
}

func (s *Server) waveformViewLocked() []waveformView {
	const target = 1200
	core := s.sess.Manifest.CorePoints
	stride := int(core) / target
	if stride < 1 {
		stride = 1
	}
	margin := s.sess.Manifest.Margin
	out := make([]waveformView, len(s.sess.Channels))
	for ci, ch := range s.sess.Channels {
		before := make([]float64, 0, target)
		for n := int64(0); n < core; n += int64(stride) {
			before = append(before, ch.Data[margin+n])
		}
		after := make([]float64, 0, target)
		for n := int64(0); n < core; n += int64(stride) {
			after = append(after, s.last.Outputs[ci][n])
		}
		out[ci] = waveformView{Channel: ch.Spec.ID, Stride: stride, Before: before, After: after}
	}
	return out
}
