package pipeline

import (
	"math"
	"mcapture/internal/session"
)

// DivergencePoint 定位首个逐样本不一致位置。
type DivergencePoint struct {
	Channel string
	Segment int
	Sample  int64
	A       float64
	B       float64
}

// CompareResults 逐样本、逐通道比对两份结果，同时核对漂移参数与提交序列。
// 首个不一致返回 E_DIVERGE 并给出通道、分段、采样位置；完全一致返回 nil。
func CompareResults(sess *session.Session, cfg AlgoConfig, a, b *Result) error {
	if len(a.Outputs) != len(b.Outputs) {
		return session.NewError(session.EDiverge, "channel count differs")
	}
	for ci, ch := range sess.Channels {
		x, y := a.Outputs[ci], b.Outputs[ci]
		if len(x) != len(y) {
			return session.NewError(session.EDiverge, "output length differs").
				With(ch.Spec.ID, "core_points")
		}
		for i := range x {
			// 要求逐位一致（确定性浮点流水线），不使用容差。
			if math.Float64bits(x[i]) != math.Float64bits(y[i]) {
				seg := int(int64(i) / cfg.SegmentSamples)
				return session.NewError(session.EDiverge,
					"first sample divergence").
					With(ch.Spec.ID, "sample").
					WithSegment(seg).WithSample(int64(i))
			}
		}
	}
	// 漂移参数逐位核对。
	for id, pa := range a.Drift {
		pb, ok := b.Drift[id]
		if !ok {
			return session.NewError(session.EDiverge, "drift missing in second result").With(id, "drift")
		}
		if math.Float64bits(pa.Phi0) != math.Float64bits(pb.Phi0) ||
			len(pa.Knots) != len(pb.Knots) {
			return session.NewError(session.EDiverge, "drift parameter differs").With(id, "phi0")
		}
		for k := range pa.Knots {
			if math.Float64bits(pa.Knots[k].Rate) != math.Float64bits(pb.Knots[k].Rate) {
				return session.NewError(session.EDiverge, "drift rate differs").
					With(id, "drift_basis").WithSegment(k)
			}
		}
	}
	return nil
}

// CompareResiduals 单独核对两条完整运行的标记残余偏移统计。
func CompareResiduals(a, b *Result) error {
	for id, ra := range a.Residuals {
		rb := b.Residuals[id]
		if len(ra) != len(rb) {
			return session.NewError(session.EDiverge, "residual stats length differs").
				With(id, "residuals")
		}
		for k := range ra {
			if ra[k].MarkerIndex != rb[k].MarkerIndex ||
				math.Float64bits(ra[k].Residual) != math.Float64bits(rb[k].Residual) {
				return session.NewError(session.EDiverge, "residual stat differs").
					With(id, "residuals").WithMarker(ra[k].MarkerIndex)
			}
		}
	}
	return nil
}

// CompareCommitSeq 单独核对两条完整运行的提交序列完全一致。
func CompareCommitSeq(a, b *Result) error {
	if len(a.CommitSeq) != len(b.CommitSeq) {
		return session.NewError(session.EDiverge, "commit sequence length differs")
	}
	for i := range a.CommitSeq {
		if a.CommitSeq[i] != b.CommitSeq[i] {
			return session.NewError(session.EDiverge, "commit sequence order differs").
				With(a.CommitSeq[i], "commit_seq").WithSegment(i)
		}
	}
	return nil
}
