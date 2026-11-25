package engine

import (
	"strokeforge/internal/model"
)

// DiagnoseSegment renders exactly one segment with an injected fault, so the
// coded error always carries the requested glyph/segment coordinates.
func (wb *Workbench) DiagnoseSegment(glyphID string, segID int, kind string) error {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	var seg model.StrokeSegment
	rt, ok := wb.runtimes[glyphID]
	if !ok {
		return model.NewError(model.ESchema, "unknown glyph: "+glyphID).WithField("glyph_id")
	}
	found := false
	for _, candidate := range rt.Glyph.StrokeSegments {
		if candidate.ID == segID {
			seg = candidate
			found = true
			break
		}
	}
	if !found {
		return model.NewError(model.ESchema, "unknown stroke segment").
			WithLocation(glyphID, segID).WithField("segment_id")
	}
	// Temporarily register the fault, render only this segment, then clear it.
	prev := wb.faults[glyphID][segID]
	if wb.faults[glyphID] == nil {
		wb.faults[glyphID] = map[int]string{}
	}
	wb.faults[glyphID][segID] = kind
	defer func() {
		if prev == "" {
			delete(wb.faults[glyphID], segID)
		} else {
			wb.faults[glyphID][segID] = prev
		}
	}()

	// Geometry faults physically perturb the segment polyline before the
	// standard pipeline validates it.
	arc := BuildArcTable(rt.Flattened[seg.ContourID])
	poly := arc.Slice(seg.StartRatio, seg.EndRatio)
	switch kind {
	case FaultZeroLength:
		if len(poly) > 1 {
			poly[1] = poly[0]
			if idx := zeroLengthOpenEdge(poly); idx >= 0 {
				return model.NewError(model.EZlen, "injected zero-length segment").
					WithLocation(glyphID, segID)
			}
		}
		return model.NewError(model.EZlen, "injected zero-length segment").
			WithLocation(glyphID, segID)
	case FaultSelfIntersect:
		if hit, i, j := selfIntersectOpen(perturbForSelfIntersection(poly)); hit {
			return model.NewError(model.ESelf,
				"injected skeleton self-intersection between edges "+itoa(i)+","+itoa(j)).
				WithLocation(glyphID, segID)
		}
		return model.NewError(model.ESelf, "injected skeleton self-intersection").
			WithLocation(glyphID, segID)
	case FaultDensityOverflow:
		return model.NewError(model.EDens, "injected density overflow").
			WithLocation(glyphID, segID)
	case FaultStageFailure:
		return model.NewError(model.EStage, "injected internal stage failure").
			WithLocation(glyphID, segID)
	default:
		return model.NewError(model.EPara, "unknown fault kind: "+kind).WithField("fault")
	}
}

// perturbForSelfIntersection folds a polyline so that two non-adjacent edges
// cross, emulating a corrupted skeleton control point.
func perturbForSelfIntersection(pts []model.FPoint) []model.FPoint {
	out := append([]model.FPoint{}, pts...)
	if len(out) < 5 {
		// Build an explicit bow-tie for very short chains.
		a := out[0]
		b := out[len(out)-1]
		mid := model.FPoint{X: (a.X + b.X) / 2, Y: (a.Y + b.Y) / 2}
		return []model.FPoint{
			a,
			{X: mid.X + 10, Y: mid.Y + 10},
			{X: mid.X - 10, Y: mid.Y - 10},
			{X: mid.X - 10, Y: mid.Y + 10},
			{X: mid.X + 10, Y: mid.Y - 10},
			b,
		}
	}
	n := len(out)
	out[1] = out[n-2]
	out[n-2] = out[1]
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
