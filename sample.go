package maskqc

import (
	"fmt"
)

const DefaultSeed int64 = 420261127

type DeterministicRNG struct {
	state uint64
}

func NewDeterministicRNG(seed int64) *DeterministicRNG {
	if seed == 0 {
		seed = DefaultSeed
	}
	return &DeterministicRNG{state: uint64(seed)}
}

func (r *DeterministicRNG) next() uint64 {
	// splitmix64: stable across platforms and Go versions.
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *DeterministicRNG) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

type Sample struct {
	Project *Project
	Docs    map[string][]byte
	Loaded  map[string]*Project
}

type SampleSet struct {
	Seed    int64
	Samples []*Sample
}

func GenerateSamples(seed int64) (*SampleSet, error) {
	if seed == 0 {
		seed = DefaultSeed
	}
	rng := NewDeterministicRNG(seed)
	specs := []struct {
		id string
		w  int
		h  int
	}{
		{"icon-32", 32, 32},
		{"icon-48", 48, 32},
		{"icon-64", 64, 48},
	}
	set := &SampleSet{Seed: seed}
	for _, spec := range specs {
		p := buildSampleProject(spec.id, spec.w, spec.h, seed+int64(spec.w*17+spec.h), rng)
		if err := buildAssets(p, rng); err != nil {
			return nil, err
		}
		sample := &Sample{Project: p, Docs: map[string][]byte{}, Loaded: map[string]*Project{}}
		for _, v := range []string{SchemaV1, SchemaV2, SchemaV3} {
			res, err := ExportProject(p, v)
			if err != nil {
				return nil, fmt.Errorf("export %s for %s: %w", v, p.ID, err)
			}
			sample.Docs[v] = res.Data
			loaded, err := LoadProject(res.Data)
			if err != nil {
				return nil, fmt.Errorf("roundtrip load %s for %s: %w", v, p.ID, err)
			}
			sample.Loaded[v] = loaded
		}
		set.Samples = append(set.Samples, sample)
	}
	return set, nil
}

func buildSampleProject(id string, w, h int, seed int64, rng *DeterministicRNG) *Project {
	midY := h / 2
	stroke1Y := h*3/10 - 1
	stroke2Y := h*3/4 + 1
	strokes := []Stroke{
		{
			ID: "s1", Seq: 1, Priority: 10, Group: "ring",
			Start: Point{X: maxInt(4, w/8), Y: stroke1Y},
			End:   Point{X: w - maxInt(5, w/8), Y: stroke1Y + 1},
			Width: maxInt(5, h/6), HardnessPct: 85, WeightMyriad: 10000,
			DirectionClass: DirTangent,
		},
		{
			ID: "s2", Seq: 2, Priority: 20, Group: "ring",
			Start: Point{X: maxInt(5, w/7), Y: stroke2Y},
			End:   Point{X: w - maxInt(6, w/7), Y: maxInt(stroke1Y+6, midY+2)},
			Width: maxInt(4, h/7), HardnessPct: 65, WeightMyriad: 8000,
			DirectionClass: DirTangent,
		},
		{
			ID: "s3", Seq: 3, Priority: 30, Group: "accent",
			Start: Point{X: w/2 - 2, Y: maxInt(3, stroke1Y-5)},
			End:   Point{X: w/2 + 2, Y: maxInt(5, stroke1Y-2)},
			Width: 4, HardnessPct: 100, WeightMyriad: 7500,
			DirectionClass: DirMixed,
		},
	}
	// Keep s3 from touching s1 on tiny canvases; on 32px it is an accent above.
	groups := []StrokeGroup{
		{ID: "ring", Kind: "outline_ring", StrokeIDs: []string{"s1", "s2"}},
		{ID: "accent", Kind: "embedded_accent", StrokeIDs: []string{"s3"}},
	}
	return &Project{
		ID: id, SourceVersion: "generator", SchemaVersion: SchemaV3,
		Width: w, Height: h, TileWidth: 16, TileHeight: 16,
		Seed: seed, Strokes: strokes, Groups: groups,
	}
}

func buildAssets(p *Project, rng *DeterministicRNG) error {
	full := Chunk{ID: 0, X: 0, Y: 0, Width: p.Width, Height: p.Height}
	empty := make([]byte, p.Width*p.Height)
	var comps []StrokeComponent
	order := []string{}
	for _, s := range p.SortedStrokes() {
		cov, err := RasterStrokeChunk(s, full)
		if err != nil {
			return err
		}
		comps = append(comps, StrokeComponent{StrokeID: s.ID, ChunkID: 0, Coverage: cov})
		order = append(order, s.ID)
	}
	refBytes, err := CompositeChunk(empty, comps, order, p.Width, p.Height)
	if err != nil {
		return err
	}
	ref := &GrayMask{Width: p.Width, Height: p.Height, Pix: refBytes}
	p.Truth = BuildTruth(ref, p.Strokes)
	p.Candidate = makeDefectiveCandidate(ref, rng)
	p.Icon = makeIcon(p, ref, rng)
	return nil
}

func makeDefectiveCandidate(ref *GrayMask, rng *DeterministicRNG) *GrayMask {
	m := NewGrayMask(ref.Width, ref.Height)
	for i, v := range ref.Pix {
		if v >= MaskThreshold {
			m.Pix[i] = 255
		}
	}
	// Inset: remove one 4-edge boundary layer.
	edge := []int{}
	w, h := m.Width, m.Height
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			if m.Pix[i] == 0 {
				continue
			}
			if x == 0 || y == 0 || x == w-1 || y == h-1 ||
				m.At(x-1, y) == 0 || m.At(x+1, y) == 0 || m.At(x, y-1) == 0 || m.At(x, y+1) == 0 {
				edge = append(edge, i)
			}
		}
	}
	for _, i := range edge {
		m.Pix[i] = 0
	}
	// Small interior holes. Find pixels whose four neighbors remain foreground.
	holeCandidates := []int{}
	for y := 2; y < h-2; y++ {
		for x := 2; x < w-2; x++ {
			i := y*w + x
			if m.Pix[i] == 255 && m.At(x-1, y) == 255 && m.At(x+1, y) == 255 && m.At(x, y-1) == 255 && m.At(x, y+1) == 255 {
				holeCandidates = append(holeCandidates, i)
			}
		}
	}
	for k := 0; k < 2 && len(holeCandidates) > 0; k++ {
		idx := holeCandidates[rng.Intn(len(holeCandidates))]
		m.Pix[idx] = 0
	}
	// Spill: deterministic 2-pixel nibs immediately outside foreground bboxes.
	spillSeeds := []Point{}
	for _, d := range AnalyzeMask(ref, AnalyzeOptions{}).Domains {
		spillSeeds = append(spillSeeds, Point{X: d.BBox.MaxX + 1, Y: d.BBox.MinY + 1})
		spillSeeds = append(spillSeeds, Point{X: d.BBox.MinX - 1, Y: d.BBox.MaxY})
	}
	for _, pt := range spillSeeds {
		for dy := 0; dy < 2; dy++ {
			for dx := 0; dx < 2; dx++ {
				x, y := pt.X+dx, pt.Y+dy
				if x >= 0 && y >= 0 && x < w && y < h {
					m.Set(x, y, 255)
				}
			}
		}
	}
	return m
}

func makeIcon(p *Project, ref *GrayMask, rng *DeterministicRNG) *RGBAMask {
	img := NewRGBAMask(p.Width, p.Height)
	for y := 0; y < p.Height; y++ {
		for x := 0; x < p.Width; x++ {
			t := y * 1000 / maxInt(1, p.Height-1)
			// Near-foreground blue-grey endpoint makes low contrast regions.
			r := byte(226 - 45*t/1000)
			g := byte(232 - 35*t/1000)
			b := byte(244 - 18*t/1000)
			noise := int(rng.next()%25) - 12
			img.Pix[(y*p.Width+x)*4+0] = clampByte(int(r) + noise)
			img.Pix[(y*p.Width+x)*4+1] = clampByte(int(g) + noise)
			img.Pix[(y*p.Width+x)*4+2] = clampByte(int(b) + noise)
			img.Pix[(y*p.Width+x)*4+3] = 255
		}
	}
	fg := [3]byte{24, 116, 196}
	for i, a := range ref.Pix {
		if a > 0 {
			x, y := i%p.Width, i/p.Width
			img.SetRGBA(x, y, fg[0], fg[1], fg[2], a)
		}
	}
	// Translucent construction stroke overlay.
	full := Chunk{X: 0, Y: 0, Width: p.Width, Height: p.Height}
	overlayColor := [3]byte{255, 176, 64}
	for _, s := range p.SortedStrokes() {
		cov, err := RasterStrokeChunk(s, full)
		if err != nil {
			continue
		}
		for i, a := range cov {
			if a > 0 {
				x, y := i%p.Width, i/p.Width
				img.SetRGBA(x, y, overlayColor[0], overlayColor[1], overlayColor[2], byte(int(a)*45/255))
			}
		}
	}
	// Embedded sparse noise.
	for k := 0; k < p.Width*p.Height/12; k++ {
		x := rng.Intn(p.Width)
		y := rng.Intn(p.Height)
		i := (y*p.Width + x) * 4
		v := byte(rng.Intn(80) + 20)
		img.Pix[i] = clampByte(int(img.Pix[i]) ^ int(v))
		img.Pix[i+1] = clampByte(int(img.Pix[i+1]) ^ int(v))
		img.Pix[i+2] = clampByte(int(img.Pix[i+2]) ^ int(v))
	}
	return img
}

func clampByte(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}
