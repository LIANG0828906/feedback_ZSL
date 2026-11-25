package maskqa

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand"
)

type CandidateDefect struct {
	Kind        string `json:"kind"`
	Rect        Rect   `json:"rect"`
	Description string `json:"description"`
}

type Fixture struct {
	Name    string                   `json:"name"`
	Project *Project                 `json:"project"`
	Truth   *TruthManifest           `json:"truth"`
	Defects []CandidateDefect        `json:"defects"`
	Exports map[string]*ExportResult `json:"-"`
	Loaded  map[string]*Project      `json:"-"`
}

type FixtureSet struct {
	Seed     int64      `json:"seed"`
	Fixtures []*Fixture `json:"fixtures"`
}

type sampleSpec struct {
	name   string
	width  int
	height int
}

// GenerateSet builds the deterministic local sample batch.
func GenerateSet(seed int64) (*FixtureSet, error) {
	if seed == 0 {
		seed = DefaultSeed
	}
	specs := []sampleSpec{
		{name: "compact-80x48", width: 80, height: 48},
		{name: "standard-96x72", width: 96, height: 72},
		{name: "large-128x96", width: 128, height: 96},
	}
	set := &FixtureSet{Seed: seed}
	for i, spec := range specs {
		fx, err := generateFixture(seed+int64(i)*1000003, spec)
		if err != nil {
			return nil, err
		}
		set.Fixtures = append(set.Fixtures, fx)
	}
	return set, nil
}

func generateFixture(seed int64, spec sampleSpec) (*Fixture, error) {
	rng := rand.New(rand.NewSource(seed))
	strokes, xl, xr, yt, yb := designStrokes(spec.width, spec.height)
	brushRadius := clampInt(minInt(spec.width, spec.height)/16, 2, 4)
	brushWidth := brushRadius*2 + 1

	p := &Project{
		SourceVersion:  SchemaV3,
		ProjectID:      fmt.Sprintf("maskqa-%s-%d", spec.name, seed),
		Seed:           seed,
		Width:          spec.width,
		Height:         spec.height,
		BrushWidthPx:   brushWidth,
		CutAlphaBPS:    DefaultCutAlphaBPS,
		RoundingMode:   RoundingFloor,
		CoordEncoding:  CoordinateEncoding{Mode: "absolute_px", OffsetQuantum: 1},
		WeightScale:    "basis_points",
		DirectionTable: "canonical_snake_v3",
		Strokes:        strokes,
		ResidualDigest: DigestResiduals(nil),
	}

	truthMask := BuildAnalyticReferenceMask(p)
	measured := AnalyzeTopology(truthMask)
	truthDomains := make([]DomainMetric, 0, len(measured))
	for _, d := range measured {
		d.RingWidthPx = float64(brushWidth)
		truthDomains = append(truthDomains, d)
	}
	segments := buildTruthSegments(p, xl, xr, yt, yb)
	truth := &TruthManifest{
		ProjectID: p.ProjectID, Seed: seed, Width: p.Width, Height: p.Height,
		Mask: truthMask, Domains: truthDomains, Segments: segments,
	}

	candidate, defects := makeDefectiveCandidate(truthMask, truthDomains, xl, xr, yt)
	p.Candidate = candidate
	img := renderSampleImage(p, rng)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	p.ImagePNG = buf.Bytes()
	if err := ValidateProject(p); err != nil {
		return nil, err
	}

	fx := &Fixture{Name: spec.name, Project: p, Truth: truth, Defects: defects, Exports: map[string]*ExportResult{}, Loaded: map[string]*Project{}}
	for _, v := range []string{SchemaV1, SchemaV2, SchemaV3} {
		res, err := ExportProject(p, v)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", v, err)
		}
		fx.Exports[v] = res
		lp, err := LoadProject(res.Data)
		if err != nil {
			return nil, fmt.Errorf("load exported %s: %w", v, err)
		}
		fx.Loaded[v] = lp
	}
	return fx, nil
}

func designStrokes(w, h int) ([]Stroke, int, int, int, int) {
	xl := w * 3 / 16
	xr := w * 9 / 16
	yt := h / 4
	yb := h * 3 / 4
	brushRadius := clampInt(minInt(w, h)/16, 2, 4)
	brushWidth := brushRadius*2 + 1
	barX0 := xr + brushWidth + 4
	barLength := minInt(yb-yt, w-barX0-brushWidth-2)
	barY0 := yt + maxInt(0, (yb-yt-barLength)/2)
	weights := []int{8355, 7623, 9187, 6842, 8769}
	hardness := []int{820, 740, 900, 680, 780}
	raw := []struct {
		a, b Point
		cat  string
	}{
		{Point{xl, yt}, Point{xr, yt}, DirE},
		{Point{xr, yt}, Point{xr, yb}, DirS},
		{Point{xr, yb}, Point{xl, yb}, DirW},
		{Point{xl, yb}, Point{xl, yt}, DirN},
		{Point{barX0, barY0}, Point{barX0 + barLength, barY0 + barLength}, DirSE},
	}
	strokes := make([]Stroke, 0, len(raw))
	for i, r := range raw {
		idx := i + 1
		angle := AngleFromVector(float64(r.b.X-r.a.X), float64(r.b.Y-r.a.Y))
		strokes = append(strokes, Stroke{
			Index: idx, ID: fmt.Sprintf("stroke-%d", idx), Priority: idx * 10,
			Start: r.a, End: r.b, WeightBPS: weights[i], HardnessPermille: hardness[i],
			HardnessPresent: true, DirectionCategory: r.cat, DeclaredAngleDeg: angle,
		})
	}
	return strokes, xl, xr, yt, yb
}

func buildTruthSegments(p *Project, xl, xr, yt, yb int) []BoundarySegment {
	segments := []BoundarySegment{}
	for _, s := range p.Strokes {
		domainID := 1
		if s.Index == 5 {
			domainID = 2
		}
		_ = xl
		_ = xr
		_ = yt
		_ = yb
		offset := p.BrushWidthPx / 2
		for j, side := range []string{"normal_a", "normal_b"} {
			sign := 1
			if j == 1 {
				sign = -1
			}
			segments = append(segments, BoundarySegment{
				SegmentID:        fmt.Sprintf("seg-%d-%s", s.Index, side),
				DomainID:         domainID,
				StrokeIndex:      s.Index,
				Side:             side,
				Probe:            SideProbe(s, sign, offset),
				ExpectedCategory: s.DirectionCategory,
				ExpectedAngleDeg: s.DeclaredAngleDeg,
			})
		}
	}
	return segments
}

func makeDefectiveCandidate(truth *Mask, domains []DomainMetric, xl, xr, yt int) (*Mask, []CandidateDefect) {
	c := truth.Clone()
	defects := []CandidateDefect{}
	if len(domains) == 0 {
		return c, defects
	}
	d := domains[0]

	hole := Rect{X0: xl + (xr-xl)/2 - 1, Y0: yt, X1: xl + (xr-xl)/2 + 1, Y1: yt + 2}
	for y := hole.Y0; y < hole.Y1; y++ {
		for x := hole.X0; x < hole.X1; x++ {
			c.Set(x, y, false)
		}
	}
	defects = append(defects, CandidateDefect{Kind: "hole", Rect: hole, Description: "2x2 candidate void inside top ring stroke"})

	inset := Rect{X0: d.MinX, Y0: yt - 1, X1: d.MinX + 2, Y1: yt + 2}
	for y := inset.Y0; y < inset.Y1; y++ {
		for x := inset.X0; x < inset.X1; x++ {
			c.Set(x, y, false)
		}
	}
	defects = append(defects, CandidateDefect{Kind: "inset", Rect: inset, Description: "eroded outer top-left candidate edge"})

	sx := maxInt(1, d.MinX-5)
	spill := Rect{X0: sx, Y0: d.MinY + 3, X1: sx + 2, Y1: d.MinY + 5}
	for y := spill.Y0; y < spill.Y1; y++ {
		for x := spill.X0; x < spill.X1; x++ {
			c.Set(x, y, true)
		}
	}
	defects = append(defects, CandidateDefect{Kind: "spill", Rect: spill, Description: "isolated 2x2 candidate overflow blob"})
	return c, defects
}

func renderSampleImage(p *Project, rng *rand.Rand) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, p.Width, p.Height))
	top := color.RGBA{R: 226, G: 230, B: 244, A: 255}
	bottom := color.RGBA{R: 207, G: 216, B: 238, A: 255}
	foreground := color.RGBA{R: 42, G: 72, B: 168, A: 255}
	accent := color.RGBA{R: 38, G: 176, B: 190, A: 255}

	for y := 0; y < p.Height; y++ {
		v := 0
		if p.Height > 1 {
			v = y * 256 / (p.Height - 1)
		}
		c := lerpColor(top, bottom, v)
		for x := 0; x < p.Width; x++ {
			img.SetRGBA(x, y, c)
		}
	}

	blockRects := []Rect{
		{X0: 2, Y0: p.Height - 9, X1: 8, Y1: p.Height - 3},
		{X0: p.Width - 12, Y0: 2, X1: p.Width - 4, Y1: 7},
	}
	for _, br := range blockRects {
		fillRect(img, br, foreground)
	}

	for _, s := range p.Strokes {
		b := StrokeBounds(p, s)
		for y := b.Y0; y < b.Y1; y++ {
			for x := b.X0; x < b.X1; x++ {
				if !image.Pt(x, y).In(img.Bounds()) {
					continue
				}
				geom := GeometryCoverageBPS(x, y, s, p.BrushWidthPx)
				if geom == 0 {
					continue
				}
				_, along := segmentGeometry(x, y, s)
				strokeColor := lerpColor(foreground, accent, int(along*255))
				alpha := 80 + int(geom)*160/WeightBasisScale
				blendPixel(img, x, y, strokeColor, clampU8(alpha))
			}
		}
	}

	for y := 0; y < p.Height; y++ {
		for x := 0; x < p.Width; x++ {
			c := img.RGBAAt(x, y)
			n := rng.Intn(17) - 8
			c.R = clampU8(int(c.R) + n)
			c.G = clampU8(int(c.G) + n)
			c.B = clampU8(int(c.B) + n)
			if rng.Intn(20) == 0 {
				c.A = 255
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func fillRect(img *image.RGBA, r Rect, c color.RGBA) {
	for y := r.Y0; y < r.Y1; y++ {
		for x := r.X0; x < r.X1; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

func lerpColor(a, b color.RGBA, t int) color.RGBA {
	ratio := float64(clampInt(t, 0, 255)) / 255.0
	return color.RGBA{
		R: uint8(float64(a.R) + (float64(b.R)-float64(a.R))*ratio),
		G: uint8(float64(a.G) + (float64(b.G)-float64(a.G))*ratio),
		B: uint8(float64(a.B) + (float64(b.B)-float64(a.B))*ratio),
		A: 255,
	}
}

func blendPixel(img *image.RGBA, x, y int, c color.RGBA, alpha uint8) {
	dst := img.RGBAAt(x, y)
	a := uint32(alpha)
	ia := 255 - a
	out := color.RGBA{
		R: uint8((uint32(c.R)*a + uint32(dst.R)*ia) / 255),
		G: uint8((uint32(c.G)*a + uint32(dst.G)*ia) / 255),
		B: uint8((uint32(c.B)*a + uint32(dst.B)*ia) / 255),
		A: 255,
	}
	img.SetRGBA(x, y, out)
}

func clampU8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}
