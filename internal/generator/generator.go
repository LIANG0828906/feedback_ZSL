// Package generator produces fully deterministic sample glyphs from a fixed
// seed. No external fonts or network resources are involved.
package generator

import (
	"math"
	"math/rand"

	"strokeforge/internal/engine"
	"strokeforge/internal/model"
	"strokeforge/internal/raster"
)

// DefaultSeed matches the project's repeatable demo.
const DefaultSeed int64 = 20261125

// DefaultParams is the fixed stroke parameter snapshot.
func DefaultParams() model.StrokeParams {
	return model.StrokeParams{
		Width:       9,
		Join:        "round",
		Cap:         "round",
		Spacing:     5,
		Tolerance:   0.75,
		Scale:       1.5,
		BBoxPadding: 16,
	}
}

func quant(v float64) int { return int(math.Round(v)) }

// ellipseContour emits a closed contour of alternating quadratic/cubic
// Bezier segments approximating an ellipse centered at (cx,cy).
func ellipseContour(id string, rng *rand.Rand, cx, cy, rx, ry float64, spans int, wobble float64) model.Contour {
	type knot struct{ x, y float64 }
	knots := make([]knot, spans)
	for i := 0; i < spans; i++ {
		ang := 2 * math.Pi * float64(i) / float64(spans)
		rf := 1 + (rng.Float64()*2-1)*wobble
		knots[i] = knot{cx + rx*rf*math.Cos(ang), cy + ry*rf*math.Sin(ang)}
	}
	segs := make([]model.Segment, spans)
	for i := 0; i < spans; i++ {
		a := knots[i]
		b := knots[(i+1)%spans]
		midAng := 2 * math.Pi * (float64(i) + 0.5) / float64(spans)
		if i%2 == 0 {
			// Quadratic with a control point pushed outward.
			cx1 := cx + (rx+18)*math.Cos(midAng)
			cy1 := cy + (ry+18)*math.Sin(midAng)
			segs[i] = model.Segment{
				ID:  id + segNum(i),
				Typ: model.SegQuadratic,
				P0:  model.Point{X: quant(a.x), Y: quant(a.y)},
				C1:  model.Point{X: quant(cx1), Y: quant(cy1)},
				P1:  model.Point{X: quant(b.x), Y: quant(b.y)},
			}
		} else {
			// Cubic with two tangential controls.
			dx := b.x - a.x
			dy := b.y - a.y
			c1 := model.Point{X: quant(a.x + dx*0.3), Y: quant(a.y + dy*0.3 + 8)}
			c2 := model.Point{X: quant(a.x + dx*0.7), Y: quant(a.y + dy*0.7 - 8)}
			segs[i] = model.Segment{
				ID:  id + segNum(i),
				Typ: model.SegCubic,
				P0:  model.Point{X: quant(a.x), Y: quant(a.y)},
				C1:  c1,
				C2:  c2,
				P1:  model.Point{X: quant(b.x), Y: quant(b.y)},
			}
		}
	}
	// Force exact closure by aligning the last end with the first start.
	segs[len(segs)-1].P1 = segs[0].P0
	return model.Contour{ID: id, Closed: true, Segments: segs}
}

func segNum(i int) string {
	if i < 10 {
		return "-0" + string(rune('0'+i))
	}
	return "-" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// shieldContour builds a non-ellipse closed outline (shield-ish glyph) so the
// sample set contains mixed geometry.
func shieldContour(id string, rng *rand.Rand) model.Contour {
	raw := []struct{ x, y float64 }{
		{-72, -118}, {72, -118}, {97, -68}, {97, 20}, {75, 88},
		{0, 151}, {-75, 88}, {-97, 20}, {-97, -68},
	}
	for i := range raw {
		raw[i].x += (rng.Float64()*2 - 1) * 3
		raw[i].y += (rng.Float64()*2 - 1) * 3
	}
	n := len(raw)
	segs := make([]model.Segment, n)
	for i := 0; i < n; i++ {
		a := raw[i]
		b := raw[(i+1)%n]
		if i%2 == 0 {
			mx, my := (a.x+b.x)/2, (a.y+b.y)/2
			segs[i] = model.Segment{
				ID:  id + segNum(i),
				Typ: model.SegQuadratic,
				P0:  model.Point{X: quant(a.x), Y: quant(a.y)},
				C1:  model.Point{X: quant(mx), Y: quant(my + 14)},
				P1:  model.Point{X: quant(b.x), Y: quant(b.y)},
			}
		} else {
			dx, dy := b.x-a.x, b.y-a.y
			segs[i] = model.Segment{
				ID:  id + segNum(i),
				Typ: model.SegCubic,
				P0:  model.Point{X: quant(a.x), Y: quant(a.y)},
				C1:  model.Point{X: quant(a.x + dx*0.33), Y: quant(a.y + dy*0.33)},
				C2:  model.Point{X: quant(a.x + dx*0.66), Y: quant(a.y + dy*0.66)},
				P1:  model.Point{X: quant(b.x), Y: quant(b.y)},
			}
		}
	}
	segs[n-1].P1 = segs[0].P0
	return model.Contour{ID: id, Closed: true, Segments: segs}
}

// partition splits a contour into five equal arc-ratio stroke segments and
// chains them linearly through Upstream.
func partition(contourID string, baseID int) []model.StrokeSegment {
	out := make([]model.StrokeSegment, 5)
	for i := 0; i < 5; i++ {
		up := []int{}
		if i > 0 {
			up = []int{baseID + i - 1}
		}
		out[i] = model.StrokeSegment{
			ID:         baseID + i,
			ContourID:  contourID,
			StartRatio: float64(i) / 5,
			EndRatio:   float64(i+1) / 5,
			Upstream:   up,
		}
	}
	return out
}

// Generate builds the full deterministic document including analytical truth.
func Generate(seed int64) (*model.Document, error) {
	rng := rand.New(rand.NewSource(seed))
	params := DefaultParams()

	aegisOuter := shieldContour("aegis-outer", rng)
	aegisCore := ellipseContour("aegis-core", rng, 0, 12, 46, 116, 8, 0.05)
	orbisOuter := ellipseContour("orbis-outer", rng, 0, 0, 92, 104, 9, 0.04)
	orbisCore := ellipseContour("orbis-core", rng, 0, 0, 44, 52, 7, 0.06)
	serpOuter := ellipseContour("serpent-outer", rng, 0, 0, 94, 138, 10, 0.07)
	serpCore := ellipseContour("serpent-core", rng, 0, 0, 38, 60, 8, 0.08)

	glyphs := []model.Glyph{
		{
			ID:             "Aegis",
			Contours:       []model.Contour{aegisOuter, aegisCore},
			StrokeSegments: append(partition("aegis-outer", 0), partition("aegis-core", 5)...),
		},
		{
			ID:             "Orbis",
			Contours:       []model.Contour{orbisOuter, orbisCore},
			StrokeSegments: append(partition("orbis-outer", 0), partition("orbis-core", 5)...),
		},
		{
			ID:             "Serpent",
			Contours:       []model.Contour{serpOuter, serpCore},
			StrokeSegments: append(partition("serpent-outer", 0), partition("serpent-core", 5)...),
		},
	}

	doc := &model.Document{
		SchemaVersion: model.SchemaVersion,
		Seed:          seed,
		Quantization:  model.Quantization{Min: -2048, Max: 2048, Step: 1, Unit: "1/64 em"},
		StrokeParams:  params,
		Glyphs:        glyphs,
	}
	truth, err := computeTruth(doc)
	if err != nil {
		return nil, err
	}
	doc.Truth = model.Truth{Glyphs: truth}
	return doc, nil
}

// computeTruth renders every glyph with the reference pipeline and records
// sample points, area, bbox, mask dimensions and density values.
func computeTruth(doc *model.Document) ([]model.GlyphTruth, error) {
	out := make([]model.GlyphTruth, 0, len(doc.Glyphs))
	for gi := range doc.Glyphs {
		g := &doc.Glyphs[gi]
		flat := map[string][]model.FPoint{}
		var area float64
		var bbox model.BBox
		first := true
		for ci := range g.Contours {
			pts, err := engine.FlattenContour(g.Contours[ci], doc.StrokeParams.Tolerance)
			if err != nil {
				return nil, err
			}
			flat[g.Contours[ci].ID] = pts
			a := engine.SignedArea(pts)
			if a < 0 {
				a = -a
			}
			area += a
			b := engine.BoundsOf(pts)
			if first {
				bbox, first = b, false
			} else {
				bbox = unionBBox(bbox, b)
			}
		}
		frame := raster.NewFrame(bbox, doc.StrokeParams)
		mask := raster.NewMask(frame)
		samples := []model.SamplePointTruth{}
		for _, ss := range g.StrokeSegments {
			arc := engine.BuildArcTable(flat[ss.ContourID])
			poly := arc.Slice(ss.StartRatio, ss.EndRatio)
			band, err := engine.BuildStrokeBand(ss.ID, poly, doc.StrokeParams)
			if err != nil {
				return nil, err
			}
			mask.CompositeBand(band, frame)
			// Five evenly spaced sample points per stroke segment.
			for k := 0; k < 5; k++ {
				r := ss.StartRatio + (float64(k)+0.5)/5*(ss.EndRatio-ss.StartRatio)
				p := arc.AtRatio(r)
				samples = append(samples, model.SamplePointTruth{
					StrokeSegmentID: ss.ID,
					Point:           model.FPoint{X: float64(quant(p.X)), Y: float64(quant(p.Y))},
				})
			}
		}
		for i := range samples {
			d, err := mask.DensityAt(samples[i].Point, frame)
			if err != nil {
				return nil, err
			}
			samples[i].Density = d / 255
		}
		overall := 0
		for _, v := range mask.Pix {
			if int(v)/255 > overall {
				overall = int(v) / 255
			}
		}
		out = append(out, model.GlyphTruth{
			GlyphID:       g.ID,
			SamplePoints:  samples,
			ClosedArea:    area,
			BBox:          bbox,
			MaskWidth:     frame.Width,
			MaskHeight:    frame.Height,
			StrokeDensity: overall,
		})
	}
	return out, nil
}

func unionBBox(a, b model.BBox) model.BBox {
	return model.BBox{
		MinX: math.Min(a.MinX, b.MinX),
		MinY: math.Min(a.MinY, b.MinY),
		MaxX: math.Max(a.MaxX, b.MaxX),
		MaxY: math.Max(a.MaxY, b.MaxY),
	}
}
