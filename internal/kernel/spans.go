package kernel

import (
	"fmt"
	"math"
	"sort"

	"volumekernel/internal/camera"
	"volumekernel/internal/geom"
	"volumekernel/internal/model"
)

type rawSpan struct {
	MediumIndex int
	MediumID    string
	T0          int64
	T1          int64
	Residual0   float64
	Residual1   float64
}

type OrderedSpan struct {
	T0              int64
	T1              int64
	T0ResidualTicks float64
	T1ResidualTicks float64
	MediaIndexes    []int
	MediaIDs        []string
}

func BuildOrderedSpans(s model.Scene, ray camera.Ray) ([]OrderedSpan, error) {
	scale := s.FixedScale
	if scale <= 0 {
		return nil, model.ParamError("", "fixed_scale", "fixed scale must be positive")
	}
	var raw []rawSpan
	for mi := range s.Media {
		m := &s.Media[mi]
		iv, ok := geom.RayAABB(ray.Origin, ray.Direction, m.Bounds)
		if !ok {
			continue
		}
		t0 := math.Max(0, iv.T0)
		t1 := iv.T1
		if t1 <= t0 {
			continue
		}
		q0 := model.QuantizeFloor(t0, scale)
		q1 := model.QuantizeCeil(t1, scale)
		if q1.Ticks <= q0.Ticks {
			continue
		}
		raw = append(raw, rawSpan{
			MediumIndex: mi,
			MediumID:    m.ID,
			T0:          q0.Ticks,
			T1:          q1.Ticks,
			Residual0:   q0.Residual,
			Residual1:   q1.Residual,
		})
	}
	if len(raw) == 0 {
		return nil, nil
	}
	sort.Slice(raw, func(i, j int) bool {
		if raw[i].T0 != raw[j].T0 {
			return raw[i].T0 < raw[j].T0
		}
		if raw[i].T1 != raw[j].T1 {
			return raw[i].T1 < raw[j].T1
		}
		return raw[i].MediumID < raw[j].MediumID
	})
	ticks := make([]int64, 0, len(raw)*2)
	residual := make(map[int64]float64, len(raw)*2)
	for _, r := range raw {
		ticks = append(ticks, r.T0, r.T1)
		if math.Abs(r.Residual0) > math.Abs(residual[r.T0]) {
			residual[r.T0] = r.Residual0
		}
		if math.Abs(r.Residual1) > math.Abs(residual[r.T1]) {
			residual[r.T1] = r.Residual1
		}
	}
	sort.Slice(ticks, func(i, j int) bool { return ticks[i] < ticks[j] })
	uniq := ticks[:0]
	for _, t := range ticks {
		if len(uniq) == 0 || uniq[len(uniq)-1] != t {
			uniq = append(uniq, t)
		}
	}
	var ordered []OrderedSpan
	for i := 0; i+1 < len(uniq); i++ {
		t0, t1 := uniq[i], uniq[i+1]
		if t1 <= t0 {
			continue
		}
		mid := t0 + (t1-t0)/2
		var active []rawSpan
		for _, r := range raw {
			if r.T0 <= mid && mid < r.T1 {
				active = append(active, r)
			}
		}
		if len(active) == 0 {
			continue
		}
		sort.Slice(active, func(i, j int) bool { return active[i].MediumID < active[j].MediumID })
		if len(ordered) > 0 && ordered[len(ordered)-1].T1 == t0 && sameActive(ordered[len(ordered)-1].MediaIDs, active) {
			ordered[len(ordered)-1].T1 = t1
			ordered[len(ordered)-1].T1ResidualTicks = residual[t1]
			continue
		}
		ids := make([]string, len(active))
		idxs := make([]int, len(active))
		for j := range active {
			ids[j] = active[j].MediumID
			idxs[j] = active[j].MediumIndex
		}
		ordered = append(ordered, OrderedSpan{
			T0:              t0,
			T1:              t1,
			T0ResidualTicks: residual[t0],
			T1ResidualTicks: residual[t1],
			MediaIndexes:    idxs,
			MediaIDs:        ids,
		})
	}
	return ordered, nil
}

func sameActive(ids []string, raw []rawSpan) bool {
	if len(ids) != len(raw) {
		return false
	}
	for i := range ids {
		if ids[i] != raw[i].MediumID {
			return false
		}
	}
	return true
}

func AllocateSteps(spans []OrderedSpan, total int) ([]int, error) {
	if len(spans) == 0 {
		return nil, nil
	}
	if total <= 0 {
		return nil, fmt.Errorf("step budget must be positive")
	}
	if total < len(spans) {
		return nil, fmt.Errorf("step budget %d cannot cover %d ordered spans", total, len(spans))
	}
	alloc := make([]int, len(spans))
	for i := range alloc {
		alloc[i] = 1
	}
	remaining := total - len(spans)
	if remaining == 0 {
		return alloc, nil
	}
	lengths := make([]float64, len(spans))
	totalLen := 0.0
	for i, sp := range spans {
		lengths[i] = float64(sp.T1 - sp.T0)
		totalLen += lengths[i]
	}
	type rem struct {
		Index int
		Value float64
	}
	rems := make([]rem, len(spans))
	for i := range spans {
		exact := float64(remaining) * lengths[i] / totalLen
		whole := int(math.Floor(exact))
		alloc[i] += whole
		rems[i] = rem{Index: i, Value: exact - float64(whole)}
	}
	left := total
	for _, x := range alloc {
		left -= x
	}
	sort.SliceStable(rems, func(i, j int) bool {
		if rems[i].Value != rems[j].Value {
			return rems[i].Value > rems[j].Value
		}
		return rems[i].Index < rems[j].Index
	})
	for i := 0; i < left; i++ {
		alloc[rems[i%len(rems)].Index]++
	}
	return alloc, nil
}
