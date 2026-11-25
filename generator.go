package maskqc

import (
	"fmt"
	"image"
	"image/color"
	"math/rand"
	"os"
	"path/filepath"
)

const FixedSeed int64 = 424242

type SampleSet struct {
	Seed     int64      `json:"seed"`
	Projects []*Project `json:"projects"`
}

type generatedSpec struct {
	id     string
	width  int
	height int
	paint  []Stroke
	spills []Stroke
}

func GenerateSampleSet(seed int64) (*SampleSet, error) {
	if seed == 0 {
		seed = FixedSeed
	}
	specs := defaultSpecs()
	set := &SampleSet{Seed: seed}
	for i, spec := range specs {
		rng := rand.New(rand.NewSource(seed + int64(i*1009+17)))
		paint := normalizeStrokes(spec.paint)
		reference, _, err := ComposeStrokesFull(spec.width, spec.height, nil, paint)
		if err != nil {
			return nil, fmt.Errorf("generate reference for %s: %w", spec.id, err)
		}

		inset := erodeMask4(reference)
		holeVariant := punchConstructionHoles(reference.Clone(), paint)
		spillVariant := reference.Clone()
		candidate := punchConstructionHoles(erodeMask4(reference), paint)

		repair := make([]Stroke, 0, len(spec.spills))
		for j, spill := range spec.spills {
			spill = fullHardStroke(spill)
			stampCoverage(spillVariant, spill)
			stampCoverage(candidate, spill)
			erase := spill
			erase.ID = fmt.Sprintf("erase-%d", j+1)
			erase.Seq = 100 + j + 1
			erase.Priority = 90
			erase.Op = StrokeOpErase
			erase.Role = RoleRepair
			repair = append(repair, erase)
		}

		defects := map[string]*Mask{
			"inset":     inset,
			"holes":     holeVariant,
			"spill":     spillVariant,
			"defective": candidate.Clone(),
		}

		allStrokes := append(append([]Stroke{}, paint...), repair...)
		domains, _, err := AnalyzeTopology(reference)
		if err != nil {
			return nil, err
		}
		truthDomains := make([]DomainTruth, 0, len(domains))
		for _, d := range domains {
			truthDomains = append(truthDomains, DomainTruth{
				DomainID: d.DomainID, CentroidX1000: d.CentroidX1000, CentroidY1000: d.CentroidY1000,
				Area: d.Area, BBox: d.BBox, Holes: d.Holes, BandWidthPixels: d.BandWidthPixels,
				HoleTolerance: 0, BandTolerancePM: 100,
			})
		}
		segments := ExtractDirectionSegments(reference, paint)
		img := renderGeneratedImage(spec, reference, rng)
		project := &Project{
			SchemaVersion: "internal/generated-v1",
			ID:            spec.id,
			Seed:          seed,
			Width:         spec.width,
			Height:        spec.height,
			Image:         img,
			Candidate:     candidate,
			DefectMasks:   defects,
			Strokes:       allStrokes,
			Truth: Truth{
				Reference: reference,
				Domains:   truthDomains,
				Segments:  segments,
			},
		}
		set.Projects = append(set.Projects, project)
	}
	return set, nil
}

func defaultSpecs() []generatedSpec {
	weight := 10000
	hardness := 1000
	return []generatedSpec{
		{
			id: "ring-32", width: 32, height: 32,
			paint: []Stroke{
				{ID: "ring-top", Seq: 1, Priority: 20, Start: Point{9, 9}, End: Point{22, 9}, Radius: 3, WeightBP: weight, HardnessPermille: hardness, Op: StrokeOpOver, Role: RolePaint},
				{ID: "ring-right", Seq: 2, Priority: 10, Start: Point{22, 9}, End: Point{22, 22}, Radius: 3, WeightBP: weight, HardnessPermille: hardness, Op: StrokeOpOver, Role: RolePaint},
				{ID: "ring-bottom", Seq: 3, Priority: 20, Start: Point{9, 22}, End: Point{22, 22}, Radius: 3, WeightBP: weight, HardnessPermille: hardness, Op: StrokeOpOver, Role: RolePaint},
				{ID: "ring-left", Seq: 4, Priority: 10, Start: Point{9, 9}, End: Point{9, 22}, Radius: 3, WeightBP: weight, HardnessPermille: hardness, Op: StrokeOpOver, Role: RolePaint},
			},
			spills: []Stroke{
				{ID: "spill-right", Start: Point{28, 9}, End: Point{28, 15}, Radius: 1},
				{ID: "spill-bottom", Start: Point{9, 28}, End: Point{15, 28}, Radius: 1},
			},
		},
		{
			id: "bars-48x36", width: 48, height: 36,
			paint: []Stroke{
				{ID: "bar-a", Seq: 1, Priority: 10, Start: Point{8, 10}, End: Point{31, 10}, Radius: 3, WeightBP: weight, HardnessPermille: hardness, Op: StrokeOpOver, Role: RolePaint},
				{ID: "bar-b", Seq: 2, Priority: 10, Start: Point{17, 25}, End: Point{40, 25}, Radius: 3, WeightBP: weight, HardnessPermille: hardness, Op: StrokeOpOver, Role: RolePaint},
			},
			spills: []Stroke{
				{ID: "spill-right", Start: Point{45, 9}, End: Point{45, 17}, Radius: 1},
				{ID: "spill-bottom", Start: Point{7, 32}, End: Point{14, 32}, Radius: 1},
			},
		},
		{
			id: "ring-dot-64", width: 64, height: 64,
			paint: []Stroke{
				{ID: "big-top", Seq: 1, Priority: 10, Start: Point{18, 18}, End: Point{45, 18}, Radius: 5, WeightBP: weight, HardnessPermille: hardness, Op: StrokeOpOver, Role: RolePaint},
				{ID: "big-right", Seq: 2, Priority: 10, Start: Point{45, 18}, End: Point{45, 45}, Radius: 5, WeightBP: weight, HardnessPermille: hardness, Op: StrokeOpOver, Role: RolePaint},
				{ID: "big-bottom", Seq: 3, Priority: 10, Start: Point{18, 45}, End: Point{45, 45}, Radius: 5, WeightBP: weight, HardnessPermille: hardness, Op: StrokeOpOver, Role: RolePaint},
				{ID: "big-left", Seq: 4, Priority: 10, Start: Point{18, 18}, End: Point{18, 45}, Radius: 5, WeightBP: weight, HardnessPermille: hardness, Op: StrokeOpOver, Role: RolePaint},
				{ID: "small-bar", Seq: 5, Priority: 20, Start: Point{10, 55}, End: Point{21, 59}, Radius: 3, WeightBP: weight, HardnessPermille: hardness, Op: StrokeOpOver, Role: RolePaint},
			},
			spills: []Stroke{
				{ID: "spill-right", Start: Point{57, 20}, End: Point{57, 31}, Radius: 1},
				{ID: "spill-top", Start: Point{8, 8}, End: Point{16, 8}, Radius: 1},
			},
		},
	}
}

func normalizeStrokes(in []Stroke) []Stroke {
	out := make([]Stroke, len(in))
	copy(out, in)
	for i := range out {
		if out[i].ID == "" {
			out[i].ID = fmt.Sprintf("stroke-%d", i+1)
		}
		if out[i].Seq == 0 {
			out[i].Seq = i + 1
		}
		if out[i].Priority == 0 {
			out[i].Priority = 10
		}
		out[i] = fullHardStroke(out[i])
		if out[i].Op == "" {
			out[i].Op = StrokeOpOver
		}
		if out[i].Role == "" {
			out[i].Role = RolePaint
		}
	}
	return out
}

func fullHardStroke(s Stroke) Stroke {
	if s.WeightBP == 0 {
		s.WeightBP = 10000
	}
	if s.HardnessPermille == 0 {
		s.HardnessPermille = 1000
	}
	if s.Op == "" {
		s.Op = StrokeOpOver
	}
	return s
}

func erodeMask4(m *Mask) *Mask {
	out := m.Clone()
	for y := 0; y < m.Height; y++ {
		for x := 0; x < m.Width; x++ {
			if m.Get(x, y) < ForegroundThreshold {
				continue
			}
			if m.Get(x-1, y) < ForegroundThreshold || m.Get(x+1, y) < ForegroundThreshold ||
				m.Get(x, y-1) < ForegroundThreshold || m.Get(x, y+1) < ForegroundThreshold {
				out.Set(x, y, 0)
			}
		}
	}
	return out
}

func punchConstructionHoles(m *Mask, strokes []Stroke) *Mask {
	if m == nil {
		return nil
	}
	out := m.Clone()
	for _, s := range strokes {
		mid := Point{X: (s.Start.X + s.End.X) / 2, Y: (s.Start.Y + s.End.Y) / 2}
		out.Set(mid.X, mid.Y, 0)
		if s.Radius >= 4 {
			out.Set(mid.X+1, mid.Y, 0)
			out.Set(mid.X, mid.Y+1, 0)
		}
	}
	return out
}

func stampCoverage(m *Mask, s Stroke) {
	s = fullHardStroke(s)
	bounds := s.Bounds()
	minX := maxInt(0, bounds.MinX)
	minY := maxInt(0, bounds.MinY)
	maxX := minInt(m.Width-1, bounds.MaxX)
	maxY := minInt(m.Height-1, bounds.MaxY)
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			a := coverageAt(x, y, s)
			i := m.index(x, y)
			m.Alpha[i] = AlphaOver(m.Alpha[i], a)
		}
	}
}

func renderGeneratedImage(spec generatedSpec, ref *Mask, rng *rand.Rand) *Image {
	w, h := spec.width, spec.height
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	top := color.RGBA{R: 225, G: 231, B: 250, A: 255}
	bottom := color.RGBA{R: 184, G: 199, B: 238, A: 255}
	fg := color.RGBA{R: 43, G: 91, B: 205, A: 255}
	strokeTint := color.RGBA{R: 255, G: 255, B: 255, A: 95}

	for y := 0; y < h; y++ {
		t := int64(y) * 1000 / int64(maxInt(1, h-1))
		bg := blendColor(top, bottom, int(t))
		for x := 0; x < w; x++ {
			c := bg
			if ref.Get(x, y) >= ForegroundThreshold {
				block := ((x / 8) + (y / 8)) % 3
				c = shiftColor(fg, (block-1)*14)
				if isBoundaryPixel(ref, x, y) {
					c = blendColor(c, strokeTint, 370)
				}
			}
			noise := rng.Intn(17) - 8
			c = shiftColor(c, noise)
			img.SetRGBA(x, y, c)
		}
	}
	return &Image{Width: w, Height: h, RGBA: append([]byte(nil), img.Pix...)}
}

func isBoundaryPixel(m *Mask, x, y int) bool {
	return m.Get(x, y) >= ForegroundThreshold &&
		(m.Get(x-1, y) < ForegroundThreshold || m.Get(x+1, y) < ForegroundThreshold ||
			m.Get(x, y-1) < ForegroundThreshold || m.Get(x, y+1) < ForegroundThreshold)
}

func blendColor(a, b color.RGBA, permille int) color.RGBA {
	ratio := permille
	out := color.RGBA{
		R: byte((int(a.R)*(1000-ratio) + int(b.R)*ratio) / 1000),
		G: byte((int(a.G)*(1000-ratio) + int(b.G)*ratio) / 1000),
		B: byte((int(a.B)*(1000-ratio) + int(b.B)*ratio) / 1000),
		A: 255,
	}
	return out
}

func shiftColor(c color.RGBA, delta int) color.RGBA {
	clamp := func(v int) byte {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return byte(v)
	}
	c.R = clamp(int(c.R) + delta)
	c.G = clamp(int(c.G) + delta)
	c.B = clamp(int(c.B) + delta)
	return c
}

func (s *SampleSet) WriteTo(root string) error {
	for _, p := range s.Projects {
		dir := filepath.Join(root, p.ID)
		if err := WriteImagePNG(filepath.Join(dir, "image.png"), p.Image); err != nil {
			return err
		}
		if err := WriteMaskPNG(filepath.Join(dir, "candidate.png"), p.Candidate); err != nil {
			return err
		}
		if err := WriteMaskPNG(filepath.Join(dir, "reference.png"), p.Truth.Reference); err != nil {
			return err
		}
		for name, mask := range p.DefectMasks {
			if err := WriteMaskPNG(filepath.Join(dir, "candidate-"+name+".png"), mask); err != nil {
				return err
			}
		}
		for _, version := range []string{"v1", "v2", "v3"} {
			data, err := MarshalProjectVersion(p, version)
			if err != nil {
				return err
			}
			path := filepath.Join(dir, "project-"+version+".json")
			if err := writeFileAtomic(path, data); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
