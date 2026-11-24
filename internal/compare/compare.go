package compare

import (
	"fmt"
	"math"

	"github.com/gameaudio/workbench/internal/errors"
	"github.com/gameaudio/workbench/internal/model"
)

// 声明容差：几何长度（定点量化误差）与声学量（float64 确定性合成）。
const (
	TolGeometry = 1e-3
	TolAcoustic = 1e-9
)

// Diff 定位首个不一致位置。
type Diff struct {
	PairKey   string
	SampleIdx int
	Field     string
	Left      float64
	Right     float64
}

func (d Diff) String() string {
	return fmt.Sprintf("pair=%s sample=%d field=%s left=%.12g right=%.12g",
		d.PairKey, d.SampleIdx, d.Field, d.Left, d.Right)
}

// Results 逐对比较两条计算路径的结果，返回首个不一致；nil 表示完全一致。
// 比较采用逐采样精确相等（float64 位级），满足“逐采样一致”的验收口径。
func Results(left, right []model.Result) *Diff {
	idx := map[string]model.Result{}
	for _, r := range right {
		idx[key(r.SourceID, r.ListenerID)] = r
	}
	for _, l := range left {
		k := key(l.SourceID, l.ListenerID)
		r, ok := idx[k]
		if !ok {
			return &Diff{PairKey: k, SampleIdx: -1, Field: "missing", Left: 0, Right: 0}
		}
		if d := pairDiff(k, l, r); d != nil {
			return d
		}
	}
	return nil
}

func pairDiff(k string, a, b model.Result) *Diff {
	if a.Occluded != b.Occluded {
		return boolDiff(k, "occluded", a.Occluded, b.Occluded)
	}
	if a.Failed != b.Failed || a.ErrorCode != b.ErrorCode {
		return &Diff{PairKey: k, SampleIdx: -1, Field: "failed",
			Left: b2f(a.Failed), Right: b2f(b.Failed)}
	}
	if math.Abs(a.DiffractionLen-b.DiffractionLen) > TolGeometry {
		return &Diff{PairKey: k, SampleIdx: -1, Field: "diffractionLen",
			Left: a.DiffractionLen, Right: b.DiffractionLen}
	}
	if math.Abs(a.EffectiveLen-b.EffectiveLen) > TolGeometry {
		return &Diff{PairKey: k, SampleIdx: -1, Field: "effectiveLen",
			Left: a.EffectiveLen, Right: b.EffectiveLen}
	}
	if math.Abs(a.SPL-b.SPL) > TolAcoustic {
		return &Diff{PairKey: k, SampleIdx: -1, Field: "spl",
			Left: a.SPL, Right: b.SPL}
	}
	n := len(a.Curve)
	if len(b.Curve) < n {
		n = len(b.Curve)
	}
	for i := 0; i < n; i++ {
		if a.Curve[i] != b.Curve[i] {
			return &Diff{PairKey: k, SampleIdx: i, Field: "curve",
				Left: a.Curve[i], Right: b.Curve[i]}
		}
	}
	if len(a.Curve) != len(b.Curve) {
		return &Diff{PairKey: k, SampleIdx: n, Field: "curveLength",
			Left: float64(len(a.Curve)), Right: float64(len(b.Curve))}
	}
	return nil
}

// Profiles 比较两条路径的听者剖面。
func Profiles(left, right []model.Profile) *Diff {
	idx := map[string]model.Profile{}
	for _, p := range right {
		idx[p.ListenerID] = p
	}
	for _, l := range left {
		r, ok := idx[l.ListenerID]
		if !ok {
			return &Diff{PairKey: l.ListenerID, SampleIdx: -1, Field: "profileMissing"}
		}
		if l.MeanSPL != r.MeanSPL {
			return &Diff{PairKey: l.ListenerID, SampleIdx: -1, Field: "profile.meanSpl",
				Left: l.MeanSPL, Right: r.MeanSPL}
		}
		if l.MaxSPL != r.MaxSPL {
			return &Diff{PairKey: l.ListenerID, SampleIdx: -1, Field: "profile.maxSpl",
				Left: l.MaxSPL, Right: r.MaxSPL}
		}
		if l.OccludedPairs != r.OccludedPairs || l.TotalPairs != r.TotalPairs ||
			l.FailedPairs != r.FailedPairs {
			return &Diff{PairKey: l.ListenerID, SampleIdx: -1, Field: "profile.counts",
				Left: float64(l.OccludedPairs), Right: float64(r.OccludedPairs)}
		}
		n := len(l.CurveMean)
		for i := 0; i < n; i++ {
			if l.CurveMean[i] != r.CurveMean[i] {
				return &Diff{PairKey: l.ListenerID, SampleIdx: i, Field: "profile.curveMean",
					Left: l.CurveMean[i], Right: r.CurveMean[i]}
			}
		}
	}
	return nil
}

// AsError 把不一致转为 E_DIFF 业务错误并写明两侧实测值。
func AsError(d *Diff) error {
	if d == nil {
		return nil
	}
	return errors.New(errors.EDiff, d.Field, d.String())
}

// Truth 比对单对计算结果与解析真值（容差口径）。
func Truth(r model.Result, t model.Truth) *Diff {
	k := key(r.SourceID, r.ListenerID)
	if r.Occluded != t.Occluded {
		return boolDiff(k, "truth.occluded", r.Occluded, t.Occluded)
	}
	if r.Occluded && math.Abs(r.DiffractionLen-t.DiffractionLen) > TolGeometry {
		return &Diff{PairKey: k, SampleIdx: -1, Field: "truth.diffractionLen",
			Left: r.DiffractionLen, Right: t.DiffractionLen}
	}
	if math.Abs(r.SPL-t.SPL) > TolAcoustic {
		return &Diff{PairKey: k, SampleIdx: -1, Field: "truth.spl",
			Left: r.SPL, Right: t.SPL}
	}
	for i := range t.Curve {
		if math.Abs(r.Curve[i]-t.Curve[i]) > TolAcoustic {
			return &Diff{PairKey: k, SampleIdx: i, Field: "truth.curve",
				Left: r.Curve[i], Right: t.Curve[i]}
		}
	}
	return nil
}

func key(s, l string) string { return s + "->" + l }

func boolDiff(k, field string, a, b bool) *Diff {
	return &Diff{PairKey: k, SampleIdx: -1, Field: field, Left: b2f(a), Right: b2f(b)}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
