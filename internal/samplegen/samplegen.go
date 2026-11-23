// Package samplegen produces deterministic glyph samples and independent
// truth data. It never reads from the network.
package samplegen

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"glyphbrush/internal/geom"
	"glyphbrush/internal/model"
)

const (
	GeneratorVersion = "glyphbrush/samplegen-v1"
	DefaultSeed      = int64(20261124)
)

type segmentTemplate struct {
	Kind    model.SegmentType
	Control []model.Point
	End     model.Point
}

type contourTemplate struct {
	Start    model.Point
	Segments []segmentTemplate
}

type glyphTemplate struct {
	ID       string
	Advance  int64
	Contours []contourTemplate
}

// Generate builds a deterministic document and matching truth.
func Generate(seed int64) (model.Document, model.TruthDocument) {
	doc := model.Document{
		SchemaVersion: model.SampleSchema,
		Generator:     GeneratorVersion,
		Seed:          seed,
		Quantization:  1,
		UnitsPerEm:    1024,
		CoordinateRange: model.CoordinateRange{
			MinX: -1024,
			MinY: -256,
			MaxX: 1024,
			MaxY: 1024,
		},
		DensityRange: model.DensityRange{MinPPM: 0, MaxPPM: 1000},
	}
	truth := model.TruthDocument{
		SchemaVersion: model.TruthSchema,
		Generator:     GeneratorVersion,
		Seed:          seed,
	}
	for _, tpl := range templates() {
		g := buildGlyph(tpl, seed, doc.CoordinateRange)
		doc.Glyphs = append(doc.Glyphs, g)
		truth.Glyphs = append(truth.Glyphs, buildTruth(g, seed))
	}
	return doc, truth
}

func templates() []glyphTemplate {
	return []glyphTemplate{
		{
			ID:      "ui-a",
			Advance: 700,
			Contours: []contourTemplate{
				{
					Start: model.Point{X: 0, Y: 720},
					Segments: []segmentTemplate{
						{Kind: model.Cubic, Control: []model.Point{{X: 110, Y: 520}, {X: 285, Y: 105}}, End: model.Point{X: 315, Y: 0}},
						{Kind: model.Cubic, Control: []model.Point{{X: 230, Y: 80}, {X: -230, Y: 80}}, End: model.Point{X: -315, Y: 0}},
						{Kind: model.Cubic, Control: []model.Point{{X: -285, Y: 105}, {X: -110, Y: 520}}, End: model.Point{X: 0, Y: 720}},
					},
				},
				{
					Start: model.Point{X: 0, Y: 520},
					Segments: []segmentTemplate{
						{Kind: model.Cubic, Control: []model.Point{{X: 62, Y: 402}, {X: 112, Y: 190}}, End: model.Point{X: 126, Y: 140}},
						{Kind: model.Cubic, Control: []model.Point{{X: 72, Y: 156}, {X: -72, Y: 156}}, End: model.Point{X: -126, Y: 140}},
						{Kind: model.Cubic, Control: []model.Point{{X: -112, Y: 190}, {X: -62, Y: 402}}, End: model.Point{X: 0, Y: 520}},
					},
				},
			},
		},
		{
			ID:      "orbit-o",
			Advance: 760,
			Contours: []contourTemplate{
				{
					Start: model.Point{X: -330, Y: 310},
					Segments: []segmentTemplate{
						{Kind: model.Quadratic, Control: []model.Point{{X: -330, Y: 542}}, End: model.Point{X: 0, Y: 542}},
						{Kind: model.Quadratic, Control: []model.Point{{X: 330, Y: 542}}, End: model.Point{X: 330, Y: 310}},
						{Kind: model.Quadratic, Control: []model.Point{{X: 330, Y: 78}}, End: model.Point{X: 0, Y: 78}},
						{Kind: model.Quadratic, Control: []model.Point{{X: -330, Y: 78}}, End: model.Point{X: -330, Y: 310}},
					},
				},
				{
					Start: model.Point{X: -136, Y: 310},
					Segments: []segmentTemplate{
						{Kind: model.Cubic, Control: []model.Point{{X: -136, Y: 345}, {X: -100, Y: 370}}, End: model.Point{X: 0, Y: 370}},
						{Kind: model.Cubic, Control: []model.Point{{X: 100, Y: 370}, {X: 136, Y: 345}}, End: model.Point{X: 136, Y: 310}},
						{Kind: model.Cubic, Control: []model.Point{{X: 136, Y: 275}, {X: 100, Y: 250}}, End: model.Point{X: 0, Y: 250}},
						{Kind: model.Cubic, Control: []model.Point{{X: -100, Y: 250}, {X: -136, Y: 275}}, End: model.Point{X: -136, Y: 310}},
					},
				},
			},
		},
		{
			ID:      "wave-s",
			Advance: 720,
			Contours: []contourTemplate{
				{
					Start: model.Point{X: 265, Y: 560},
					Segments: []segmentTemplate{
						{Kind: model.Cubic, Control: []model.Point{{X: 40, Y: 690}, {X: -245, Y: 660}}, End: model.Point{X: -260, Y: 455}},
						{Kind: model.Cubic, Control: []model.Point{{X: -270, Y: 300}, {X: -40, Y: 360}}, End: model.Point{X: 60, Y: 300}},
						{Kind: model.Cubic, Control: []model.Point{{X: 210, Y: 245}, {X: 245, Y: 120}}, End: model.Point{X: 130, Y: 70}},
						{Kind: model.Quadratic, Control: []model.Point{{X: -80, Y: -15}}, End: model.Point{X: -245, Y: 105}},
						{Kind: model.Quadratic, Control: []model.Point{{X: -335, Y: 160}}, End: model.Point{X: -250, Y: 250}},
						{Kind: model.Cubic, Control: []model.Point{{X: -100, Y: 170}, {X: -65, Y: 230}}, End: model.Point{X: -115, Y: 305}},
						{Kind: model.Cubic, Control: []model.Point{{X: -160, Y: 420}, {X: -130, Y: 475}}, End: model.Point{X: 25, Y: 455}},
						{Kind: model.Cubic, Control: []model.Point{{X: 155, Y: 435}, {X: 330, Y: 515}}, End: model.Point{X: 265, Y: 560}},
					},
				},
				{
					Start: model.Point{X: 225, Y: 520},
					Segments: []segmentTemplate{
						{Kind: model.Quadratic, Control: []model.Point{{X: 80, Y: 585}}, End: model.Point{X: -200, Y: 555}},
						{Kind: model.Quadratic, Control: []model.Point{{X: -160, Y: 465}}, End: model.Point{X: 55, Y: 460}},
						{Kind: model.Cubic, Control: []model.Point{{X: 195, Y: 420}, {X: 145, Y: 335}}, End: model.Point{X: -160, Y: 395}},
						{Kind: model.Cubic, Control: []model.Point{{X: -285, Y: 420}, {X: -225, Y: 500}}, End: model.Point{X: 225, Y: 520}},
					},
				},
			},
		},
	}
}

func buildGlyph(tpl glyphTemplate, seed int64, rngRange model.CoordinateRange) model.Glyph {
	rng := rand.New(rand.NewSource(seed ^ hashID(tpl.ID)))
	jitter := make(map[[2]int64][2]int64)
	jit := func(p model.Point) model.Point {
		key := [2]int64{p.X, p.Y}
		d, ok := jitter[key]
		if !ok {
			d = [2]int64{int64(rng.Intn(5) - 2), int64(rng.Intn(5) - 2)}
			jitter[key] = d
		}
		p.X += d[0]
		p.Y += d[1]
		if p.X < rngRange.MinX {
			p.X = rngRange.MinX
		}
		if p.X > rngRange.MaxX {
			p.X = rngRange.MaxX
		}
		if p.Y < rngRange.MinY {
			p.Y = rngRange.MinY
		}
		if p.Y > rngRange.MaxY {
			p.Y = rngRange.MaxY
		}
		return p
	}
	g := model.Glyph{ID: tpl.ID, AdvanceWidth: tpl.Advance, Baseline: 0}
	for ci, ct := range tpl.Contours {
		contour := model.Contour{Index: ci, Closed: true}
		start := jit(ct.Start)
		cur := start
		for _, st := range ct.Segments {
			seg := model.Segment{Type: st.Kind, Start: cur}
			for _, c := range st.Control {
				seg.Control = append(seg.Control, jit(c))
			}
			seg.End = jit(st.End)
			contour.Segments = append(contour.Segments, seg)
			cur = seg.End
		}
		g.Contours = append(g.Contours, contour)
	}
	g.Bounds = computeBounds(g)
	return g
}

func computeBounds(g model.Glyph) model.Bounds {
	b := geom.EmptyBounds()
	for _, c := range g.Contours {
		for _, s := range c.Segments {
			pts, err := model.FlattenSegment(s, 0.2)
			if err != nil {
				panic(err)
			}
			b.AddPoint(s.Start.F())
			for _, p := range pts {
				b.AddPoint(p)
			}
		}
	}
	return model.Bounds{
		MinX: int64(math.Floor(b.MinX)),
		MinY: int64(math.Floor(b.MinY)),
		MaxX: int64(math.Ceil(b.MaxX)),
		MaxY: int64(math.Ceil(b.MaxY)),
	}
}

func buildTruth(g model.Glyph, seed int64) model.GlyphTruth {
	t := model.GlyphTruth{ID: g.ID, Bounds: g.Bounds, ContentHash: g.Hash()}
	totalArea := 0.0
	for _, c := range g.Contours {
		poly := contourPolygon(c)
		area := geom.Area(poly)
		totalArea += area
		t.Contours = append(t.Contours, model.ContourTruth{Index: c.Index, Area: area})
		for si, s := range c.Segments {
			for _, u := range []float64{0.25, 0.5, 0.75} {
				p, _ := model.SegmentCurve(s, u)
				t.Samples = append(t.Samples, model.CurveSample{
					ContourIndex: c.Index,
					SegmentIndex: si,
					T:            u,
					X:            p.X,
					Y:            p.Y,
				})
			}
		}
	}
	t.ClosedArea = totalArea
	_ = seed
	return t
}

func contourPolygon(c model.Contour) []geom.Point {
	var poly []geom.Point
	for _, s := range c.Segments {
		if len(poly) == 0 {
			poly = append(poly, s.Start.F())
		}
		pts, err := model.FlattenSegment(s, 0.2)
		if err != nil {
			panic(err)
		}
		poly = append(poly, pts...)
	}
	return poly
}

func hashID(id string) int64 {
	var h int64 = 1469598103934665603
	for _, b := range []byte(id) {
		h ^= int64(b)
		h *= 1099511628211
	}
	return h
}

// Write writes glyphs.json and truth.json with deterministic indentation.
func Write(dir string, seed int64) (model.Document, model.TruthDocument, error) {
	doc, truth := Generate(seed)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return doc, truth, err
	}
	if err := writeJSON(filepath.Join(dir, "glyphs.json"), doc); err != nil {
		return doc, truth, err
	}
	if err := writeJSON(filepath.Join(dir, "truth.json"), truth); err != nil {
		return doc, truth, err
	}
	return doc, truth, nil
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Read reads a previously generated sample pair.
func Read(dir string) (model.Document, model.TruthDocument, error) {
	var doc model.Document
	var truth model.TruthDocument
	if err := readJSON(filepath.Join(dir, "glyphs.json"), &doc); err != nil {
		return doc, truth, err
	}
	if err := readJSON(filepath.Join(dir, "truth.json"), &truth); err != nil {
		return doc, truth, err
	}
	return doc, truth, nil
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
