// Package loader validates the on-disk glyph and truth documents before the
// pipeline consumes them.
package loader

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"glyphbrush/internal/errs"
	"glyphbrush/internal/geom"
	"glyphbrush/internal/model"
	"glyphbrush/internal/skeleton"
)

type Bundle struct {
	Directory string
	Document  model.Document
	Truth     model.TruthDocument
	Glyphs    []Glyph
}

type Glyph struct {
	Glyph     model.Glyph
	Truth     model.GlyphTruth
	Skeletons []skeleton.Contour
}

func (b *Bundle) Find(id string) (Glyph, bool) {
	for _, g := range b.Glyphs {
		if g.Glyph.ID == id {
			return g, true
		}
	}
	return Glyph{}, false
}

type rawPoint struct {
	X *int64 `json:"x"`
	Y *int64 `json:"y"`
}

type rawBounds struct {
	MinX *int64 `json:"minX"`
	MinY *int64 `json:"minY"`
	MaxX *int64 `json:"maxX"`
	MaxY *int64 `json:"maxY"`
}

type rawRange struct {
	MinX *int64 `json:"minX"`
	MinY *int64 `json:"minY"`
	MaxX *int64 `json:"maxX"`
	MaxY *int64 `json:"maxY"`
}

type rawDensityRange struct {
	MinPPM *int `json:"minPpm"`
	MaxPPM *int `json:"maxPpm"`
}

type rawSegment struct {
	Type    *string         `json:"type"`
	Start   *rawPoint       `json:"start"`
	Control json.RawMessage `json:"control"`
	End     *rawPoint       `json:"end"`
}

type rawContour struct {
	Index    *int         `json:"index"`
	Closed   *bool        `json:"closed"`
	Segments []rawSegment `json:"segments"`
}

type rawGlyph struct {
	ID           *string      `json:"id"`
	Baseline     *int64       `json:"baseline"`
	AdvanceWidth *int64       `json:"advanceWidth"`
	Bounds       *rawBounds   `json:"bounds"`
	Contours     []rawContour `json:"contours"`
}

type rawDocument struct {
	SchemaVersion   *string          `json:"schemaVersion"`
	Generator       *string          `json:"generator"`
	Seed            *int64           `json:"seed"`
	Quantization    *int64           `json:"quantization"`
	UnitsPerEm      *int64           `json:"unitsPerEm"`
	CoordinateRange *rawRange        `json:"coordinateRange"`
	DensityRange    *rawDensityRange `json:"densityRange"`
	Glyphs          []rawGlyph       `json:"glyphs"`
}

type rawContourTruth struct {
	Index *int     `json:"index"`
	Area  *float64 `json:"area"`
}

type rawCurveSample struct {
	ContourIndex *int     `json:"contourIndex"`
	SegmentIndex *int     `json:"segmentIndex"`
	T            *float64 `json:"t"`
	X            *float64 `json:"x"`
	Y            *float64 `json:"y"`
}

type rawGlyphTruth struct {
	ID          *string           `json:"id"`
	Bounds      *rawBounds        `json:"bounds"`
	ClosedArea  *float64          `json:"closedArea"`
	Contours    []rawContourTruth `json:"contours"`
	Samples     []rawCurveSample  `json:"samples"`
	ContentHash *string           `json:"contentHash"`
}

type rawTruth struct {
	SchemaVersion *string         `json:"schemaVersion"`
	Generator     *string         `json:"generator"`
	Seed          *int64          `json:"seed"`
	Glyphs        []rawGlyphTruth `json:"glyphs"`
}

// Load reads and validates glyphs.json and truth.json from dir.
func Load(dir string) (*Bundle, error) {
	docRaw, err := os.ReadFile(filepath.Join(dir, "glyphs.json"))
	if err != nil {
		return nil, errs.Wrap(errs.CodeSchema, err, "cannot read glyphs.json")
	}
	truthRaw, err := os.ReadFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		return nil, errs.Wrap(errs.CodeSchema, err, "cannot read truth.json")
	}
	var rd rawDocument
	if err := json.Unmarshal(docRaw, &rd); err != nil {
		return nil, errs.Wrap(errs.CodeSchema, err, "glyphs.json is not valid JSON")
	}
	var rt rawTruth
	if err := json.Unmarshal(truthRaw, &rt); err != nil {
		return nil, errs.Wrap(errs.CodeSchema, err, "truth.json is not valid JSON")
	}
	return Validate(rd, rt, dir)
}

func Validate(rd rawDocument, rt rawTruth, dir string) (*Bundle, error) {
	if rd.SchemaVersion == nil || *rd.SchemaVersion != model.SampleSchema {
		return nil, schemaErr("", -1, "missing or unsupported schemaVersion")
	}
	if rd.Generator == nil || *rd.Generator == "" {
		return nil, schemaErr("", -1, "missing generator")
	}
	if rd.Seed == nil {
		return nil, schemaErr("", -1, "missing seed")
	}
	if rd.Quantization == nil || *rd.Quantization <= 0 {
		return nil, schemaErr("", -1, "missing or invalid quantization")
	}
	if rd.UnitsPerEm == nil || *rd.UnitsPerEm <= 0 {
		return nil, schemaErr("", -1, "missing or invalid unitsPerEm")
	}
	if rd.CoordinateRange == nil || rd.CoordinateRange.MinX == nil || rd.CoordinateRange.MinY == nil ||
		rd.CoordinateRange.MaxX == nil || rd.CoordinateRange.MaxY == nil {
		return nil, schemaErr("", -1, "missing coordinateRange field")
	}
	cr := model.CoordinateRange{
		MinX: *rd.CoordinateRange.MinX,
		MinY: *rd.CoordinateRange.MinY,
		MaxX: *rd.CoordinateRange.MaxX,
		MaxY: *rd.CoordinateRange.MaxY,
	}
	if cr.MinX > cr.MaxX || cr.MinY > cr.MaxY {
		return nil, schemaErr("", -1, "coordinateRange is inverted")
	}
	if rd.DensityRange == nil || rd.DensityRange.MinPPM == nil || rd.DensityRange.MaxPPM == nil {
		return nil, schemaErr("", -1, "missing densityRange field")
	}
	dr := model.DensityRange{MinPPM: *rd.DensityRange.MinPPM, MaxPPM: *rd.DensityRange.MaxPPM}
	if dr.MinPPM < 0 || dr.MaxPPM > 1000 || dr.MinPPM > dr.MaxPPM {
		return nil, schemaErr("", -1, "densityRange must be within 0..1000")
	}
	if len(rd.Glyphs) == 0 {
		return nil, schemaErr("", -1, "glyphs must not be empty")
	}

	if rt.SchemaVersion == nil || *rt.SchemaVersion != model.TruthSchema {
		return nil, schemaErr("", -1, "missing or unsupported truth schemaVersion")
	}
	if rt.Generator == nil || *rt.Generator == "" {
		return nil, schemaErr("", -1, "missing truth generator")
	}
	if *rt.Generator != *rd.Generator {
		return nil, schemaErr("", -1, "truth generator does not match glyph generator")
	}
	if rt.Seed == nil || *rt.Seed != *rd.Seed {
		return nil, schemaErr("", -1, "truth seed does not match glyph seed")
	}
	if len(rt.Glyphs) != len(rd.Glyphs) {
		return nil, schemaErr("", -1, "truth glyph count does not match glyph count")
	}

	doc := model.Document{
		SchemaVersion:   *rd.SchemaVersion,
		Generator:       *rd.Generator,
		Seed:            *rd.Seed,
		Quantization:    *rd.Quantization,
		UnitsPerEm:      *rd.UnitsPerEm,
		CoordinateRange: cr,
		DensityRange:    dr,
	}
	truthDoc := model.TruthDocument{
		SchemaVersion: *rt.SchemaVersion,
		Seed:          *rt.Seed,
	}
	if rt.Generator != nil {
		truthDoc.Generator = *rt.Generator
	}
	truthByID := make(map[string]rawGlyphTruth, len(rt.Glyphs))
	for _, raw := range rt.Glyphs {
		id, err := validateRawTruthGlyph(raw)
		if err != nil {
			return nil, err
		}
		if _, exists := truthByID[id]; exists {
			return nil, schemaErr(id, -1, "duplicate truth glyph id")
		}
		truthByID[id] = raw
	}

	bundle := &Bundle{Directory: dir, Document: doc}
	seen := make(map[string]bool, len(rd.Glyphs))
	for _, rg := range rd.Glyphs {
		g, err := convertGlyph(rg, cr)
		if err != nil {
			return nil, err
		}
		if seen[g.ID] {
			return nil, schemaErr(g.ID, -1, "duplicate glyph id")
		}
		seen[g.ID] = true
		rtruth, ok := truthByID[g.ID]
		if !ok {
			return nil, schemaErr(g.ID, -1, "missing truth entry")
		}
		truth, err := convertTruth(g.ID, rtruth)
		if err != nil {
			return nil, err
		}
		if err := verifyTruth(g, truth); err != nil {
			return nil, err
		}
		sks, err := skeleton.Build(g, skeleton.DefaultTolerance)
		if err != nil {
			return nil, schemaErr(g.ID, -1, "cannot flatten contour: %v", err)
		}
		for _, sc := range sks {
			if err := skeleton.ValidateClosed(sc); err != nil {
				return nil, schemaErr(g.ID, sc.ContourIndex, "%v", err)
			}
		}
		if err := verifyComputedBounds(g, sks); err != nil {
			return nil, err
		}
		if err := verifySamples(g, truth); err != nil {
			return nil, err
		}
		doc.Glyphs = append(doc.Glyphs, g)
		truthDoc.Glyphs = append(truthDoc.Glyphs, truth)
		bundle.Glyphs = append(bundle.Glyphs, Glyph{Glyph: g, Truth: truth, Skeletons: sks})
	}
	bundle.Document = doc
	bundle.Truth = truthDoc
	return bundle, nil
}

func validateRawTruthGlyph(raw rawGlyphTruth) (string, error) {
	if raw.ID == nil || *raw.ID == "" {
		return "", schemaErr("", -1, "truth glyph is missing id")
	}
	id := *raw.ID
	if raw.Bounds == nil {
		return "", schemaErr(id, -1, "truth glyph is missing bounds")
	}
	if raw.ClosedArea == nil {
		return "", schemaErr(id, -1, "truth glyph is missing closedArea")
	}
	if raw.ContentHash == nil || *raw.ContentHash == "" {
		return "", schemaErr(id, -1, "truth glyph is missing contentHash")
	}
	if len(raw.Contours) == 0 {
		return "", schemaErr(id, -1, "truth glyph is missing contour entries")
	}
	if len(raw.Samples) == 0 {
		return "", schemaErr(id, -1, "truth glyph is missing samples")
	}
	return id, nil
}

func convertGlyph(raw rawGlyph, cr model.CoordinateRange) (model.Glyph, error) {
	if raw.ID == nil || *raw.ID == "" {
		return model.Glyph{}, schemaErr("", -1, "glyph is missing id")
	}
	g := model.Glyph{ID: *raw.ID}
	if raw.Baseline == nil {
		return g, schemaErr(g.ID, -1, "glyph is missing baseline")
	}
	g.Baseline = *raw.Baseline
	if raw.AdvanceWidth == nil {
		return g, schemaErr(g.ID, -1, "glyph is missing advanceWidth")
	}
	g.AdvanceWidth = *raw.AdvanceWidth
	if g.AdvanceWidth <= 0 {
		return g, schemaErr(g.ID, -1, "advanceWidth must be positive")
	}
	if raw.Bounds == nil {
		return g, schemaErr(g.ID, -1, "glyph is missing bounds")
	}
	b, err := convertBounds(*raw.Bounds)
	if err != nil {
		return g, schemaErr(g.ID, -1, "invalid bounds: %v", err)
	}
	g.Bounds = b
	if b.MinX < cr.MinX || b.MinY < cr.MinY || b.MaxX > cr.MaxX || b.MaxY > cr.MaxY {
		return g, schemaErr(g.ID, -1, "glyph bounds exceed declared coordinate range")
	}
	if len(raw.Contours) == 0 {
		return g, schemaErr(g.ID, -1, "glyph has no contours")
	}
	for ci, rc := range raw.Contours {
		if rc.Index == nil {
			return g, schemaErr(g.ID, ci, "contour is missing index")
		}
		if *rc.Index != ci {
			return g, schemaErr(g.ID, *rc.Index, "contour index must be stable and sequential")
		}
		if rc.Closed == nil {
			return g, schemaErr(g.ID, ci, "contour is missing closed field")
		}
		if !*rc.Closed {
			return g, schemaErr(g.ID, ci, "contour is not closed")
		}
		if len(rc.Segments) == 0 {
			return g, schemaErr(g.ID, ci, "contour has no segments")
		}
		c := model.Contour{Index: ci, Closed: true}
		for si, rs := range rc.Segments {
			seg, err := convertSegment(rs, cr)
			if err != nil {
				return g, schemaErr(g.ID, ci, "segment %d: %v", si, err)
			}
			c.Segments = append(c.Segments, seg)
		}
		if !samePoint(c.Segments[0].Start, c.Segments[len(c.Segments)-1].End) {
			return g, schemaErr(g.ID, ci, "contour start and end do not match")
		}
		for si := 1; si < len(c.Segments); si++ {
			if !samePoint(c.Segments[si-1].End, c.Segments[si].Start) {
				return g, schemaErr(g.ID, ci, "segment %d start is not continuous with previous end", si)
			}
		}
		g.Contours = append(g.Contours, c)
	}
	return g, nil
}

func convertSegment(raw rawSegment, cr model.CoordinateRange) (model.Segment, error) {
	var seg model.Segment
	if raw.Type == nil {
		return seg, fmt.Errorf("missing type")
	}
	seg.Type = model.SegmentType(*raw.Type)
	if seg.Type != model.Quadratic && seg.Type != model.Cubic {
		return seg, fmt.Errorf("type must be quadratic or cubic")
	}
	if raw.Start == nil || raw.End == nil {
		return seg, fmt.Errorf("missing start or end point")
	}
	start, err := convertPoint(*raw.Start, cr)
	if err != nil {
		return seg, fmt.Errorf("start: %w", err)
	}
	end, err := convertPoint(*raw.End, cr)
	if err != nil {
		return seg, fmt.Errorf("end: %w", err)
	}
	if len(raw.Control) == 0 {
		return seg, fmt.Errorf("missing control points field")
	}
	var controls []rawPoint
	if err := json.Unmarshal(raw.Control, &controls); err != nil {
		return seg, fmt.Errorf("invalid control points: %w", err)
	}
	want := 1
	if seg.Type == model.Cubic {
		want = 2
	}
	if len(controls) != want {
		return seg, fmt.Errorf("%s requires %d control points", seg.Type, want)
	}
	seg.Start, seg.End = start, end
	for i, cp := range controls {
		p, err := convertPoint(cp, cr)
		if err != nil {
			return seg, fmt.Errorf("control %d: %w", i, err)
		}
		seg.Control = append(seg.Control, p)
	}
	return seg, nil
}

func convertPoint(raw rawPoint, cr model.CoordinateRange) (model.Point, error) {
	if raw.X == nil || raw.Y == nil {
		return model.Point{}, fmt.Errorf("point is missing x or y")
	}
	p := model.Point{X: *raw.X, Y: *raw.Y}
	if !cr.Contains(p) {
		return model.Point{}, fmt.Errorf("point (%d,%d) is outside declared coordinate range", p.X, p.Y)
	}
	return p, nil
}

func convertBounds(raw rawBounds) (model.Bounds, error) {
	if raw.MinX == nil || raw.MinY == nil || raw.MaxX == nil || raw.MaxY == nil {
		return model.Bounds{}, fmt.Errorf("missing min/max coordinate")
	}
	b := model.Bounds{MinX: *raw.MinX, MinY: *raw.MinY, MaxX: *raw.MaxX, MaxY: *raw.MaxY}
	if b.MinX > b.MaxX || b.MinY > b.MaxY {
		return model.Bounds{}, fmt.Errorf("inverted bounds")
	}
	return b, nil
}

func convertTruth(id string, raw rawGlyphTruth) (model.GlyphTruth, error) {
	b, err := convertBounds(*raw.Bounds)
	if err != nil {
		return model.GlyphTruth{}, schemaErr(id, -1, "invalid truth bounds: %v", err)
	}
	t := model.GlyphTruth{ID: id, Bounds: b, ClosedArea: *raw.ClosedArea, ContentHash: *raw.ContentHash}
	for _, rc := range raw.Contours {
		if rc.Index == nil || rc.Area == nil {
			return t, schemaErr(id, -1, "truth contour entry is missing index or area")
		}
		t.Contours = append(t.Contours, model.ContourTruth{Index: *rc.Index, Area: *rc.Area})
	}
	for _, rs := range raw.Samples {
		if rs.ContourIndex == nil || rs.SegmentIndex == nil || rs.T == nil || rs.X == nil || rs.Y == nil {
			return t, schemaErr(id, -1, "truth sample is missing a required field")
		}
		t.Samples = append(t.Samples, model.CurveSample{
			ContourIndex: *rs.ContourIndex,
			SegmentIndex: *rs.SegmentIndex,
			T:            *rs.T,
			X:            *rs.X,
			Y:            *rs.Y,
		})
	}
	return t, nil
}

func verifyTruth(g model.Glyph, truth model.GlyphTruth) error {
	if truth.ContentHash != g.Hash() {
		return schemaErr(g.ID, -1, "glyph content hash does not match truth")
	}
	if !sameBounds(g.Bounds, truth.Bounds) {
		return schemaErr(g.ID, -1, "glyph bounds do not match truth bounds")
	}
	if len(truth.Contours) != len(g.Contours) {
		return schemaErr(g.ID, -1, "truth contour count does not match")
	}
	seenContours := make(map[int]bool, len(truth.Contours))
	for _, ct := range truth.Contours {
		if ct.Index < 0 || ct.Index >= len(g.Contours) {
			return schemaErr(g.ID, ct.Index, "truth contour index is out of range")
		}
		if seenContours[ct.Index] {
			return schemaErr(g.ID, ct.Index, "duplicate truth contour index")
		}
		seenContours[ct.Index] = true
	}
	var area float64
	for _, c := range g.Contours {
		var ct *model.ContourTruth
		for i := range truth.Contours {
			if truth.Contours[i].Index == c.Index {
				ct = &truth.Contours[i]
				break
			}
		}
		if ct == nil {
			return schemaErr(g.ID, c.Index, "missing truth area for contour")
		}
		poly := flattenContour(c)
		got := geom.Area(poly)
		if !closeEnough(got, ct.Area, 0.01) {
			return schemaErr(g.ID, c.Index, "closed area %.6f does not match truth %.6f", got, ct.Area)
		}
		area += got
	}
	if !closeEnough(area, truth.ClosedArea, 0.01) {
		return schemaErr(g.ID, -1, "total closed area %.6f does not match truth %.6f", area, truth.ClosedArea)
	}
	return nil
}

func flattenContour(c model.Contour) []geom.Point {
	var poly []geom.Point
	for _, s := range c.Segments {
		pts, err := model.FlattenSegment(s, skeleton.DefaultTolerance)
		if err != nil {
			return nil
		}
		if len(poly) == 0 {
			poly = append(poly, s.Start.F())
		}
		poly = append(poly, pts...)
	}
	if len(poly) > 1 && poly[0].Distance(poly[len(poly)-1]) < 1e-7 {
		poly = poly[:len(poly)-1]
	}
	return poly
}

func verifyComputedBounds(g model.Glyph, contours []skeleton.Contour) error {
	b := geom.EmptyBounds()
	for _, c := range contours {
		for _, p := range c.Points {
			b.AddPoint(p.Position)
		}
	}
	declared := g.Bounds.F()
	const boundTol = 1.0
	if math.Abs(b.MinX-declared.MinX) > boundTol || math.Abs(b.MinY-declared.MinY) > boundTol ||
		math.Abs(b.MaxX-declared.MaxX) > boundTol || math.Abs(b.MaxY-declared.MaxY) > boundTol {
		return schemaErr(g.ID, -1, "computed bounds (%.3f,%.3f)-(%.3f,%.3f) do not match declared bounds",
			b.MinX, b.MinY, b.MaxX, b.MaxY)
	}
	return nil
}

func verifySamples(g model.Glyph, truth model.GlyphTruth) error {
	for _, sample := range truth.Samples {
		if sample.ContourIndex < 0 || sample.ContourIndex >= len(g.Contours) {
			return schemaErr(g.ID, sample.ContourIndex, "truth sample contour index is out of range")
		}
		c := g.Contours[sample.ContourIndex]
		if sample.SegmentIndex < 0 || sample.SegmentIndex >= len(c.Segments) {
			return schemaErr(g.ID, c.Index, "truth sample segment index is out of range")
		}
		if sample.T < 0 || sample.T > 1 {
			return schemaErr(g.ID, c.Index, "truth sample t is outside [0,1]")
		}
		p, err := model.SegmentCurve(c.Segments[sample.SegmentIndex], sample.T)
		if err != nil {
			return schemaErr(g.ID, c.Index, "truth sample cannot be evaluated: %v", err)
		}
		if math.Abs(p.X-sample.X) > 1e-6 || math.Abs(p.Y-sample.Y) > 1e-6 {
			return schemaErr(g.ID, c.Index, "known sample (%d,%d,t=%g) evaluates to (%.6f,%.6f)",
				sample.SegmentIndex, sample.ContourIndex, sample.T, p.X, p.Y)
		}
	}
	return nil
}

func samePoint(a, b model.Point) bool { return a.X == b.X && a.Y == b.Y }
func sameBounds(a, b model.Bounds) bool {
	return a.MinX == b.MinX && a.MinY == b.MinY && a.MaxX == b.MaxX && a.MaxY == b.MaxY
}
func closeEnough(a, b, tol float64) bool { return math.Abs(a-b) <= tol+math.Abs(b)*1e-9 }

func schemaErr(glyphID string, contour int, format string, args ...any) *errs.Error {
	e := errs.New(errs.CodeSchema, format, args...).WithGlyph(glyphID)
	if contour >= 0 {
		e = e.WithContour(contour)
	}
	return e
}
