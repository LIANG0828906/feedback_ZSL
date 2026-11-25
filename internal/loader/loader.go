// Package loader validates and parses deterministic sample documents.
package loader

import (
	"bytes"
	"encoding/json"
	"fmt"

	"strokeforge/internal/model"
)

// Parse decodes raw JSON and runs every structural/semantic validation.
func Parse(raw []byte) (*model.Document, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc model.Document
	if err := dec.Decode(&doc); err != nil {
		return nil, model.NewError(model.EDecode, "malformed JSON: "+err.Error())
	}
	if err := Validate(&doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// Validate checks the document against every rule in the specification.
func Validate(doc *model.Document) error {
	if doc.SchemaVersion != model.SchemaVersion {
		return model.NewError(model.ESchema, "unsupported schema_version").WithField("schema_version")
	}
	if doc.Seed == 0 {
		return model.NewError(model.ESchema, "missing seed").WithField("seed")
	}
	q := doc.Quantization
	if q.Step <= 0 || q.Min >= q.Max {
		return model.NewError(model.ESchema, "invalid quantization envelope").WithField("quantization")
	}
	if err := validateParams(doc.StrokeParams); err != nil {
		return err
	}
	if len(doc.Glyphs) == 0 {
		return model.NewError(model.ESchema, "no glyphs present").WithField("glyphs")
	}
	if len(doc.Truth.Glyphs) != len(doc.Glyphs) {
		return model.NewError(model.ESchema, "truth entries do not match glyphs").WithField("truth")
	}
	for gi := range doc.Glyphs {
		if err := validateGlyph(&doc.Glyphs[gi], q, doc.StrokeParams, &doc.Truth.Glyphs[gi]); err != nil {
			return err
		}
	}
	return nil
}

func validateParams(p model.StrokeParams) error {
	if p.Width <= 0 || p.Width > 64 {
		return model.NewError(model.EPara, fmt.Sprintf("width %.3f out of range (0,64]", p.Width)).
			WithField("width")
	}
	if p.Tolerance <= 0 || p.Tolerance > 8 {
		return model.NewError(model.EPara, fmt.Sprintf("tolerance %.3f out of range (0,8]", p.Tolerance)).
			WithField("tolerance")
	}
	if p.Spacing <= 0 || p.Spacing > p.Width*4 {
		return model.NewError(model.EPara, "spacing must be positive and <= 4*width").
			WithField("spacing")
	}
	if p.Scale <= 0 || p.Scale > 8 {
		return model.NewError(model.EPara, "scale out of range (0,8]").WithField("scale")
	}
	if p.BBoxPadding < 0 {
		return model.NewError(model.EPara, "bbox_padding must be non-negative").WithField("bbox_padding")
	}
	switch p.Join {
	case "round", "miter", "bevel":
	default:
		return model.NewError(model.ESchema, "unknown join: "+p.Join).WithField("join")
	}
	switch p.Cap {
	case "round", "square":
	default:
		return model.NewError(model.ESchema, "unknown cap: "+p.Cap).WithField("cap")
	}
	return nil
}

func inQuant(p model.Point, q model.Quantization) bool {
	return p.X >= q.Min && p.X <= q.Max && p.Y >= q.Min && p.Y <= q.Max &&
		(p.X%q.Step == 0) && (p.Y%q.Step == 0)
}

func validateGlyph(g *model.Glyph, q model.Quantization, params model.StrokeParams, truth *model.GlyphTruth) error {
	if g.ID == "" {
		return model.NewError(model.ESchema, "glyph missing id").WithField("id")
	}
	if truth.GlyphID != g.ID {
		return model.NewError(model.ESchema, "truth glyph_id mismatch: "+truth.GlyphID).
			WithField("truth.glyph_id")
	}
	if len(g.Contours) == 0 {
		return model.NewError(model.ESchema, "glyph has no contours").WithField("contours")
	}
	contourIDs := map[string]bool{}
	for ci := range g.Contours {
		c := &g.Contours[ci]
		if c.ID == "" {
			return model.NewError(model.ESchema, "contour missing id").WithField("contours.id")
		}
		if contourIDs[c.ID] {
			return model.NewError(model.ESchema, "duplicate contour id: "+c.ID).WithField("contours.id")
		}
		contourIDs[c.ID] = true
		if !c.Closed {
			return model.NewError(model.EDecode, "contour is not closed: "+c.ID).
				WithLocation(g.ID, -1).WithField("closed")
		}
		if err := validateContour(c, q, g.ID); err != nil {
			return err
		}
	}
	if err := validateStrokeSegments(g, contourIDs); err != nil {
		return err
	}
	if err := validateTruth(g, truth, params); err != nil {
		return err
	}
	return nil
}

func validateContour(c *model.Contour, q model.Quantization, glyphID string) error {
	if len(c.Segments) < 2 {
		return model.NewError(model.EDecode, "contour needs >= 2 segments: "+c.ID).
			WithLocation(glyphID, -1)
	}
	first := c.Segments[0].P0
	last := c.Segments[len(c.Segments)-1].P1
	if first != last {
		return model.NewError(model.EDecode, "contour endpoints do not close: "+c.ID).
			WithLocation(glyphID, -1).WithField("closed")
	}
	for i := range c.Segments {
		s := &c.Segments[i]
		if s.ID == "" {
			return model.NewError(model.ESchema, "segment missing id").WithField("segments.id")
		}
		switch s.Typ {
		case model.SegQuadratic:
			if s.C2 != (model.Point{}) {
				return model.NewError(model.EDecode, "quadratic segment must not carry c2: "+s.ID).
					WithField("c2")
			}
		case model.SegCubic:
		default:
			return model.NewError(model.EDecode, "segment type must be quadratic or cubic: "+s.Typ).
				WithField("type")
		}
		for _, p := range []model.Point{s.P0, s.C1, s.C2, s.P1} {
			if !inQuant(p, q) {
				return model.NewError(model.EDecode,
					fmt.Sprintf("coordinate (%d,%d) outside quantization [%d,%d]", p.X, p.Y, q.Min, q.Max)).
					WithLocation(glyphID, -1).WithField("coordinates")
			}
		}
		if i > 0 {
			prev := c.Segments[i-1].P1
			if prev != s.P0 {
				return model.NewError(model.EDecode,
					"segment chain broken at "+s.ID).WithLocation(glyphID, -1).
					WithField("p0")
			}
		}
	}
	return nil
}

func validateStrokeSegments(g *model.Glyph, contourIDs map[string]bool) error {
	if len(g.StrokeSegments) == 0 {
		return model.NewError(model.ESchema, "missing stroke segment partition").
			WithField("stroke_segments")
	}
	ids := map[int]bool{}
	byContour := map[string][]model.StrokeSegment{}
	for i := range g.StrokeSegments {
		s := &g.StrokeSegments[i]
		if ids[s.ID] {
			return model.NewError(model.ESchema, "duplicate stroke segment id").
				WithField("stroke_segments.id")
		}
		ids[s.ID] = true
		if !contourIDs[s.ContourID] {
			return model.NewError(model.ESchema, "unknown contour id: "+s.ContourID).
				WithField("stroke_segments.contour_id")
		}
		if s.StartRatio < 0 || s.EndRatio > 1 || s.StartRatio >= s.EndRatio {
			return model.NewError(model.ESchema, "arc ratio interval invalid").
				WithField("start_ratio/end_ratio")
		}
		byContour[s.ContourID] = append(byContour[s.ContourID], *s)
		for _, up := range s.Upstream {
			if !ids[up] && up >= s.ID {
				return model.NewError(model.ESchema, "upstream segment unknown or not earlier").
					WithField("upstream")
			}
		}
	}
	// Every contour's full arc length [0,1] must be covered without gaps.
	for cid, segs := range byContour {
		sortSegs(segs)
		if segs[0].StartRatio != 0 || segs[len(segs)-1].EndRatio != 1 {
			return model.NewError(model.EDecode, "arc coverage not spanning [0,1] for "+cid).
				WithLocation(g.ID, -1).WithField("start_ratio/end_ratio")
		}
		for i := 1; i < len(segs); i++ {
			if segs[i].StartRatio != segs[i-1].EndRatio {
				return model.NewError(model.EDecode, "arc coverage gap/overlap in "+cid).
					WithLocation(g.ID, -1).WithField("start_ratio/end_ratio")
			}
		}
	}
	return nil
}

func sortSegs(segs []model.StrokeSegment) {
	for i := 1; i < len(segs); i++ {
		for j := i; j > 0 && segs[j-1].StartRatio > segs[j].StartRatio; j-- {
			segs[j-1], segs[j] = segs[j], segs[j-1]
		}
	}
}

func validateTruth(g *model.Glyph, t *model.GlyphTruth, params model.StrokeParams) error {
	if t.MaskWidth <= 0 || t.MaskHeight <= 0 {
		return model.NewError(model.ESchema, "mask dimensions missing").WithField("mask_width/mask_height")
	}
	if t.StrokeDensity < 0 || t.StrokeDensity > 255 {
		return model.NewError(model.EDens, "stroke density out of byte range").
			WithLocation(g.ID, -1).WithField("stroke_density")
	}
	// Re-derive mask dimensions from the recorded bbox and flag the exact
	// deviating field required by the specification.
	gotW := int((t.BBox.MaxX-t.BBox.MinX+2*params.BBoxPadding)*params.Scale + 0.999999)
	gotH := int((t.BBox.MaxY-t.BBox.MinY+2*params.BBoxPadding)*params.Scale + 0.999999)
	if gotW != t.MaskWidth {
		return model.NewError(model.EDens,
			fmt.Sprintf("mask_width mismatch: truth=%d derived=%d", t.MaskWidth, gotW)).
			WithField("mask_width")
	}
	if gotH != t.MaskHeight {
		return model.NewError(model.EDens,
			fmt.Sprintf("mask_height mismatch: truth=%d derived=%d", t.MaskHeight, gotH)).
			WithField("mask_height")
	}
	segIDs := map[int]bool{}
	for _, s := range g.StrokeSegments {
		segIDs[s.ID] = true
	}
	if len(t.SamplePoints) == 0 {
		return model.NewError(model.ESchema, "no sample points recorded").WithField("sample_points")
	}
	for _, sp := range t.SamplePoints {
		if !segIDs[sp.StrokeSegmentID] {
			return model.NewError(model.ESchema, "sample points unknown segment").
				WithField("sample_points.stroke_segment_id")
		}
		if sp.Density < 0 || sp.Density > 255 {
			return model.NewError(model.EDens, "negative or overflow density").
				WithLocation(g.ID, sp.StrokeSegmentID).WithField("density")
		}
	}
	return nil
}
