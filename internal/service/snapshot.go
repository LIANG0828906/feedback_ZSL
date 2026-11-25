package service

import (
	"encoding/base64"
	"fmt"

	"strokeforge/internal/engine"
	"strokeforge/internal/model"
)

// SegmentView is the UI-facing status of one stroke segment.
type SegmentView struct {
	ID        int            `json:"id"`
	ContourID string         `json:"contour_id"`
	State     string         `json:"state"`
	Upstream  []int          `json:"upstream"`
	Densities []int          `json:"densities"`
	Boundary  []model.FPoint `json:"boundary"`
	CacheKey  string         `json:"cache_key"`
	Polyline  []model.FPoint `json:"skeleton_polyline"`
	Outline   model.Polygon  `json:"band_outline"`
}

// GlyphView aggregates all visual layers of one glyph.
type GlyphView struct {
	ID          string                    `json:"id"`
	Contours    []model.Contour           `json:"contours"`
	Skeleton    map[string][]model.FPoint `json:"skeleton"`
	Segments    []SegmentView             `json:"segments"`
	MaskWidth   int                       `json:"mask_width"`
	MaskHeight  int                       `json:"mask_height"`
	MaskDataURL string                    `json:"mask_data_url"`
	Truth       model.GlyphTruth          `json:"truth"`
	BBox        model.BBox                `json:"bbox"`
}

// SnapshotView is the full initial payload for the diagnostics UI.
type SnapshotView struct {
	Algorithm    string             `json:"algorithm"`
	Seed         int64              `json:"seed"`
	Params       model.StrokeParams `json:"params"`
	Quantization model.Quantization `json:"quantization"`
	Glyphs       []GlyphView        `json:"glyphs"`
	Pool         engine.PoolConfig  `json:"pool"`
}

// BuildSnapshot renders the current engine state into a serializable view.
func BuildSnapshot(doc *model.Document, wb *engine.Workbench) (*SnapshotView, error) {
	view := &SnapshotView{
		Algorithm:    model.AlgorithmVersion,
		Seed:         doc.Seed,
		Params:       doc.StrokeParams,
		Quantization: doc.Quantization,
		Pool:         engine.DefaultPoolConfig(),
	}
	truthByID := map[string]model.GlyphTruth{}
	for _, t := range doc.Truth.Glyphs {
		truthByID[t.GlyphID] = t
	}
	for gi := range doc.Glyphs {
		g := &doc.Glyphs[gi]
		rt := wb.Runtime(g.ID)
		gv := GlyphView{
			ID:          g.ID,
			Contours:    g.Contours,
			Skeleton:    rt.Flattened,
			MaskWidth:   rt.Frames.Width,
			MaskHeight:  rt.Frames.Height,
			Truth:       truthByID[g.ID],
			BBox:        rt.Frames.BBox,
			MaskDataURL: "",
		}
		pngBytes, err := wb.MaskPNG(g.ID)
		if err != nil {
			return nil, err
		}
		gv.MaskDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
		for _, seg := range g.StrokeSegments {
			sv := SegmentView{
				ID:        seg.ID,
				ContourID: seg.ContourID,
				State:     engine.StatePending,
				Upstream:  seg.Upstream,
			}
			arc := engine.BuildArcTable(rt.Flattened[seg.ContourID])
			sv.Polyline = arc.Slice(seg.StartRatio, seg.EndRatio)
			if rec := rt.Records[seg.ID]; rec != nil {
				sv.State = rec.State
				sv.Densities = rec.Densities
				sv.Boundary = rec.Boundary
				sv.CacheKey = rec.CacheKey
				sv.Outline = rec.Band.Outline
			}
			gv.Segments = append(gv.Segments, sv)
		}
		view.Glyphs = append(view.Glyphs, gv)
	}
	return view, nil
}

// String renders a compact human-readable summary for the CLI.
func (v *SnapshotView) String() string {
	total := 0
	for _, g := range v.Glyphs {
		total += len(g.Segments)
	}
	return fmt.Sprintf("snapshot seed=%d glyphs=%d segments=%d", v.Seed, len(v.Glyphs), total)
}
