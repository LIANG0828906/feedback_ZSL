package engine

import (
	"fmt"

	"strokeforge/internal/model"
	"strokeforge/internal/raster"
)

// paramOwners maps a parameter name to the stroke-segment fields that depend
// on it. Every segment depends on all stroke params, while control edits only
// affect the glyph that owns the edited contour.
var paramOwners = map[string]bool{
	"width": true, "join": true, "cap": true, "spacing": true,
	"tolerance": true, "scale": true, "bbox_padding": true,
}

// UpdateParams applies a parameter change, bumps the param version and
// returns the precise invalidation list. Committed segments are reset to
// pending only when they directly depend on the changed parameter.
func (wb *Workbench) UpdateParams(next model.StrokeParams) ([]InvalidationEntry, string, error) {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	changes := diffParams(wb.doc.StrokeParams, next)
	if len(changes) == 0 {
		return nil, wb.paramVersion, nil
	}
	// Validate ranges before touching any state.
	if err := validateParamRanges(next); err != nil {
		return nil, "", err
	}
	wb.doc.StrokeParams = next
	wb.paramVersion = ParamsHash(next)
	entries := []InvalidationEntry{}
	for glyphID, rt := range wb.runtimes {
		for _, segID := range rt.SegOrder {
			rec := rt.Records[segID]
			if rec == nil || rec.State != StateCommitted {
				continue
			}
			for _, param := range changes {
				if paramOwners[param] {
					entries = append(entries, InvalidationEntry{
						GlyphID:   glyphID,
						SegmentID: segID,
						Reason:    fmt.Sprintf("parameter %s changed", param),
						Direct:    true,
						Parameter: param,
					})
					rec.State = StatePending
					rec.CacheKey = ""
				}
			}
		}
		// Downstream composite layer is always affected when any segment is.
		entries = append(entries, wb.propagateDownstream(rt, changes)...)
	}
	return entries, wb.paramVersion, nil
}

// propagateDownstream walks the upstream DAG and marks dependent segments.
func (wb *Workbench) propagateDownstream(rt *GlyphRuntime, changes []string) []InvalidationEntry {
	direct := map[int]bool{}
	for _, rec := range rt.Records {
		if rec.State == StatePending {
			direct[rec.SegmentID] = true
		}
	}
	out := []InvalidationEntry{}
	changed := true
	for changed {
		changed = false
		for _, seg := range rt.Glyph.StrokeSegments {
			if direct[seg.ID] {
				continue
			}
			for _, up := range seg.Upstream {
				if direct[up] {
					direct[seg.ID] = true
					changed = true
					out = append(out, InvalidationEntry{
						GlyphID:    rt.Glyph.ID,
						SegmentID:  seg.ID,
						Reason:     fmt.Sprintf("downstream of invalidated segment %d", up),
						Downstream: true,
						Parameter:  changes[0],
					})
					if rec := rt.Records[seg.ID]; rec != nil && rec.State == StateCommitted {
						rec.State = StatePending
						rec.CacheKey = ""
					}
					break
				}
			}
		}
	}
	return out
}

// EditControlPoint mutates one control point of one contour. Only the glyph
// containing that contour is invalidated; other glyphs keep their caches.
func (wb *Workbench) EditControlPoint(glyphID, contourID, segmentID string, field string, value model.Point) ([]InvalidationEntry, error) {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	rt, ok := wb.runtimes[glyphID]
	if !ok {
		return nil, model.NewError(model.ESchema, "unknown glyph").WithField("glyph_id")
	}
	q := wb.doc.Quantization
	if value.X < q.Min || value.X > q.Max || value.Y < q.Min || value.Y > q.Max {
		return nil, model.NewError(model.EPara, "edited coordinate outside quantization").
			WithField(field)
	}
	var edited bool
	for ci := range rt.Glyph.Contours {
		c := &rt.Glyph.Contours[ci]
		if c.ID != contourID {
			continue
		}
		for si := range c.Segments {
			s := &c.Segments[si]
			if s.ID != segmentID {
				continue
			}
			switch field {
			case "p0":
				s.P0 = value
			case "p1":
				s.P1 = value
			case "c1":
				s.C1 = value
			case "c2":
				s.C2 = value
			default:
				return nil, model.NewError(model.ESchema, "unknown point field: "+field).
					WithField(field)
			}
			edited = true
		}
	}
	if !edited {
		return nil, model.NewError(model.ESchema, "control point not found").
			WithField("segment_id")
	}
	// Rebuild flattened geometry and content hash for this glyph only.
	flat := map[string][]model.FPoint{}
	var bbox model.BBox
	first := true
	for ci := range rt.Glyph.Contours {
		pts, err := FlattenContour(rt.Glyph.Contours[ci], wb.doc.StrokeParams.Tolerance)
		if err != nil {
			return nil, err
		}
		flat[rt.Glyph.Contours[ci].ID] = pts
		b := BoundsOf(pts)
		if first {
			bbox, first = b, false
		} else {
			bbox = unionBBoxLocal(bbox, b)
		}
	}
	rt.Flattened = flat
	rt.Frames = raster.NewFrame(bbox, wb.doc.StrokeParams)
	rt.Mask = raster.NewMask(rt.Frames)
	rt.ContentHash = ContentHash(rt.Glyph)
	rt.ContourHash = contourHashes(rt.Glyph)

	entries := []InvalidationEntry{}
	affectedContour := map[int]bool{}
	for _, seg := range rt.Glyph.StrokeSegments {
		if seg.ContourID == contourID {
			affectedContour[seg.ID] = true
			entries = append(entries, InvalidationEntry{
				GlyphID:   glyphID,
				SegmentID: seg.ID,
				Reason:    "control point edited on contour " + contourID,
				Direct:    true,
			})
			if rec := rt.Records[seg.ID]; rec != nil {
				rec.State = StatePending
				rec.CacheKey = ""
			}
		}
	}
	entries = append(entries, wb.propagateDownstream(rt, []string{"control_point"})...)
	return entries, nil
}

// DirtySet converts invalidation entries to the render scheduler's set.
func DirtySet(entries []InvalidationEntry) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	for _, e := range entries {
		if out[e.GlyphID] == nil {
			out[e.GlyphID] = map[int]bool{}
		}
		out[e.GlyphID][e.SegmentID] = true
	}
	return out
}

func diffParams(a, b model.StrokeParams) []string {
	var out []string
	if a.Width != b.Width {
		out = append(out, "width")
	}
	if a.Join != b.Join {
		out = append(out, "join")
	}
	if a.Cap != b.Cap {
		out = append(out, "cap")
	}
	if a.Spacing != b.Spacing {
		out = append(out, "spacing")
	}
	if a.Tolerance != b.Tolerance {
		out = append(out, "tolerance")
	}
	if a.Scale != b.Scale {
		out = append(out, "scale")
	}
	if a.BBoxPadding != b.BBoxPadding {
		out = append(out, "bbox_padding")
	}
	return out
}

func validateParamRanges(p model.StrokeParams) error {
	if p.Width <= 0 || p.Width > 64 {
		return model.NewError(model.EPara, "width out of range").WithField("width")
	}
	if p.Tolerance <= 0 || p.Tolerance > 8 {
		return model.NewError(model.EPara, "tolerance out of range").WithField("tolerance")
	}
	if p.Spacing <= 0 || p.Spacing > p.Width*4 {
		return model.NewError(model.EPara, "spacing out of range").WithField("spacing")
	}
	if p.Scale <= 0 || p.Scale > 8 {
		return model.NewError(model.EPara, "scale out of range").WithField("scale")
	}
	return nil
}
