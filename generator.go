package strokelab

import (
	"fmt"
	"image"
	"image/color"
)

type SampleSpec struct {
	Name      string
	Version   string
	Width     int
	Height    int
	Variation int
	BlockSize int
}

type Sample struct {
	Name          string
	Version       string
	Project       *Project
	VersionJSON   []byte
	Icon          *image.RGBA
	CandidateMask []uint8
	CompositeMask []uint8
	ExportReport  *ExportReport
}

type Suite struct {
	Seed    int64
	Samples []*Sample
}

func DefaultSampleSpecs() []SampleSpec {
	return []SampleSpec{
		{Name: "v1-pixel-64", Version: Version1, Width: 64, Height: 64, Variation: 1, BlockSize: 16},
		{Name: "v2-relative-96", Version: Version2, Width: 96, Height: 96, Variation: 2, BlockSize: 16},
		{Name: "v3-grouped-128", Version: Version3, Width: 128, Height: 128, Variation: 3, BlockSize: 16},
	}
}

// GenerateSuite produces all three protocol versions and multiple icon
// resolutions from one fixed seed. Each sample is exported strictly and then
// loaded again from its serialized form, so the returned Project is exactly
// what an external component would observe.
func GenerateSuite(seed int64) (*Suite, error) {
	suite := &Suite{Seed: seed}
	for _, spec := range DefaultSampleSpecs() {
		sample, err := GenerateSample(seed, spec)
		if err != nil {
			return nil, err
		}
		suite.Samples = append(suite.Samples, sample)
	}
	return suite, nil
}

func GenerateSample(seed int64, spec SampleSpec) (*Sample, error) {
	if spec.Width <= 0 || spec.Height <= 0 {
		return nil, coded(CodeVersion, "sample dimensions must be positive")
	}
	if spec.Version != Version1 && spec.Version != Version2 && spec.Version != Version3 {
		return nil, coded(CodeVersion, "unknown sample version %q", spec.Version)
	}
	rng := newDetRNG(uint64(seed) ^ uint64(spec.Width)<<32 ^ uint64(spec.Height))
	ideal, shapes := generateShapeMask(spec.Width, spec.Height, spec.Variation)
	candidate := perturbCandidate(ideal, spec.Width, spec.Height, spec.Variation, rng)
	strokes, groups, err := generateStrokes(spec, shapes)
	if err != nil {
		return nil, err
	}
	p := &Project{
		ID:            fmt.Sprintf("sample-%s-%d", VersionShort(spec.Version), seed),
		Seed:          seed,
		Name:          spec.Name,
		SourceVersion: spec.Version,
		Width:         spec.Width,
		Height:        spec.Height,
		BlockSize:     spec.BlockSize,
		Rounding:      RoundHalfAwayFromZero,
		CandidateMask: candidate,
		Groups:        groups,
		Strokes:       strokes,
		ConversionTrace: []ConversionTrace{{
			Version: spec.Version, Path: "project", OldValue: "<generated>",
			NewValue: "internal", Rule: "deterministic_generator",
		}},
		ConversionVersion: ConversionVersion,
	}
	ordered, err := OrderStrokes(p.Strokes)
	if err != nil {
		return nil, err
	}
	components := make([][]uint8, len(ordered))
	for i, os := range ordered {
		components[i] = RasterizeStroke(os.Stroke, p.Width, p.Height)
	}
	composite := compositeMasks(candidate, components)
	p.Truth = truthFromEvaluation(composite, p.Width, p.Height, ordered, components)
	if err := p.Validate(); err != nil {
		return nil, err
	}
	data, report, err := ExportProject(p, spec.Version, ExportOptions{VerifyRoundTrip: true})
	if err != nil {
		return nil, fmt.Errorf("export generated %s sample: %w", spec.Version, err)
	}
	loaded, err := LoadProjectJSON(data)
	if err != nil {
		return nil, fmt.Errorf("reload generated %s sample: %w", spec.Version, err)
	}
	icon := renderIcon(p, ideal, spec.Variation)
	loadedOrdered, err := OrderStrokes(loaded.Strokes)
	if err != nil {
		return nil, err
	}
	loadedComponents := make([][]uint8, len(loadedOrdered))
	for i, os := range loadedOrdered {
		loadedComponents[i] = RasterizeStroke(os.Stroke, loaded.Width, loaded.Height)
	}
	loadedComposite := compositeMasks(loaded.CandidateMask, loadedComponents)
	return &Sample{
		Name: spec.Name, Version: spec.Version, Project: loaded,
		VersionJSON: data, Icon: icon, CandidateMask: loaded.CandidateMask,
		CompositeMask: loadedComposite, ExportReport: report,
	}, nil
}

type detRNG struct {
	state uint64
}

func newDetRNG(seed uint64) *detRNG {
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return &detRNG{state: seed}
}

func (r *detRNG) next() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545F4914F6CDD1D
}

func (r *detRNG) intn(n int) int {
	if n <= 1 {
		return 0
	}
	return int(r.next() % uint64(n))
}

type shape struct {
	Kind   string
	X0     int
	Y0     int
	X1     int
	Y1     int
	CX     int
	CY     int
	Radius int
}

func generateShapeMask(width, height, variation int) ([]uint8, []shape) {
	mask := make([]uint8, width*height)
	even := func(v int) int {
		if v < 0 {
			v = 0
		}
		if v >= width {
			v = width - 1
		}
		return v &^ 1
	}
	evenY := func(v int) int {
		if v < 0 {
			v = 0
		}
		if v >= height {
			v = height - 1
		}
		return v &^ 1
	}
	rect := shape{
		Kind: "rect", X0: even(width / 6), Y0: evenY(height / 5),
		X1: even(width * 5 / 6), Y1: evenY(height * 3 / 5),
	}
	disc := shape{
		Kind: "disc", CX: even(width * 3 / 4), CY: even(height * 3 / 4),
		Radius: max(4, width/12),
	}
	bar := shape{
		Kind: "rect", X0: even(width / 5), Y0: evenY(height * 7 / 10),
		X1: even(width / 2), Y1: evenY(height * 4 / 5),
	}
	shapes := []shape{rect, disc, bar}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			on := false
			if x >= rect.X0 && x <= rect.X1 && y >= rect.Y0 && y <= rect.Y1 {
				on = true
			}
			dx, dy := x-disc.CX, y-disc.CY
			if dx*dx+dy*dy <= disc.Radius*disc.Radius {
				on = true
			}
			if x >= bar.X0 && x <= bar.X1 && y >= bar.Y0 && y <= bar.Y1 {
				on = true
			}
			if variation%2 == 0 {
				dx, dy := x-(rect.X0+rect.X1)/2, y-(rect.Y0+rect.Y1)/2
				if dx*dx/9+dy*dy/4 <= 1 && dx*dx/9+dy*dy/4 > 0 {
					// Kept as a softly filled interior; candidate perturbation
					// below punches the deterministic void.
					on = on || (x >= rect.X0+4 && x <= rect.X1-4)
				}
			}
			if on {
				mask[y*width+x] = 255
			}
		}
	}
	return mask, shapes
}

func perturbCandidate(ideal []uint8, width, height, variation int, rng *detRNG) []uint8 {
	candidate := make([]uint8, len(ideal))
	inBounds := func(x, y int) bool {
		return x >= 0 && x < width && y >= 0 && y < height
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			idx := y*width + x
			if ideal[idx] == 0 {
				continue
			}
			// Deterministic one-pixel inset: a pixel survives only when all
			// four neighbours are foreground.
			survives := true
			for _, p := range [][2]int{{x - 1, y}, {x + 1, y}, {x, y - 1}, {x, y + 1}} {
				if !inBounds(p[0], p[1]) || ideal[p[1]*width+p[0]] == 0 {
					survives = false
					break
				}
			}
			if survives {
				candidate[idx] = uint8(196 + rng.intn(25))
			}
		}
	}
	// Outset spill: irregular near-boundary pixels receive partial coverage.
	for y := 1; y < height-1; y++ {
		for x := 1; x < width-1; x++ {
			idx := y*width + x
			if ideal[idx] != 0 {
				continue
			}
			near := false
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if ideal[(y+dy)*width+x+dx] != 0 {
						near = true
					}
				}
			}
			if near && rng.intn(100) < 28 {
				candidate[idx] = uint8(38 + rng.intn(70))
			}
		}
	}
	// Small candidate-only holes.
	holes := [][3]int{
		{width / 3, height / 3, 2 + variation%2},
		{width * 2 / 3, height / 2, 1 + variation%2},
		{width / 2, height * 2 / 3, 2},
	}
	for _, h := range holes {
		for y := h[1] - h[2]; y <= h[1]+h[2]; y++ {
			for x := h[0] - h[2]; x <= h[0]+h[2]; x++ {
				if inBounds(x, y) {
					candidate[y*width+x] = 0
				}
			}
		}
	}
	return candidate
}

func generateStrokes(spec SampleSpec, shapes []shape) ([]Stroke, []Group, error) {
	rect, disc, bar := shapes[0], shapes[1], shapes[2]
	mk := func(stage, seq int, id, group string, a, b Point, radius, hardness, weight int) Stroke {
		return Stroke{
			ID: id, Group: group, Start: a, End: b, Radius: radius,
			Hardness: hardness, Weight: weight, Stage: stage, Seq: seq,
		}
	}
	hardness := 1000
	if spec.Version == Version3 {
		hardness = 560
	}
	weightA, weightB, weightC := 8400, 7200, 9100
	if spec.Version != Version3 {
		// Legacy targets use per-mille weights; these values are exact in both
		// scales and exercise lossless export.
		weightA, weightB, weightC = 8000, 7000, 9000
	}
	strokes := []Stroke{
		mk(10, 1, "body-top", "body", Point{X: rect.X0, Y: rect.Y0}, Point{X: rect.X1, Y: rect.Y0}, 3, hardness, weightA),
		mk(10, 2, "body-bottom", "body", Point{X: rect.X0, Y: rect.Y1}, Point{X: rect.X1, Y: rect.Y1}, 3, hardness, weightA),
		mk(20, 1, "body-left", "detail", Point{X: rect.X0, Y: rect.Y0}, Point{X: rect.X0, Y: rect.Y1}, 2, hardness, weightB),
		mk(20, 2, "body-right", "detail", Point{X: rect.X1, Y: rect.Y0}, Point{X: rect.X1, Y: rect.Y1}, 2, hardness, weightB),
		mk(20, 3, "bar-outline", "detail", Point{X: bar.X0, Y: bar.Y0}, Point{X: bar.X1, Y: bar.Y0}, 2, hardness, weightB),
		mk(20, 4, "bar-lower", "detail", Point{X: bar.X0, Y: bar.Y1}, Point{X: bar.X1, Y: bar.Y1}, 2, hardness, weightB),
	}
	// Add radial repair strokes around the secondary disc.
	radial := max(2, disc.Radius/2)
	strokes = append(strokes,
		mk(30, 1, "disc-nw", "repair", Point{X: disc.CX - radial, Y: disc.CY - radial},
			Point{X: disc.CX + radial, Y: disc.CY + radial}, 2, hardness, weightC),
		mk(30, 2, "disc-ne", "repair", Point{X: disc.CX + radial, Y: disc.CY - radial},
			Point{X: disc.CX - radial, Y: disc.CY + radial}, 2, hardness, weightC),
	)
	// A repair stroke crossing a candidate hole.
	holeX, holeY := (rect.X0+rect.X1)/2, (rect.Y0+rect.Y1)/2
	strokes = append(strokes,
		mk(30, 3, "hole-repair", "repair",
			Point{X: holeX - (rect.X1-rect.X0)/6, Y: holeY},
			Point{X: holeX + (rect.X1-rect.X0)/6, Y: holeY}, 3, hardness, weightC),
	)
	for i := range strokes {
		// Relative-coordinate v2 and v1 both work on integer pixel grids. The
		// generator keeps all endpoints even so the declared v2 quantum is
		// lossless while still exercising the conversion path.
		strokes[i].Start.X = intMax(0, strokes[i].Start.X&^1)
		strokes[i].Start.Y = intMax(0, strokes[i].Start.Y&^1)
		strokes[i].End.X = intMin(spec.Width-1, strokes[i].End.X&^1)
		strokes[i].End.Y = intMin(spec.Height-1, strokes[i].End.Y&^1)
		strokes[i].DeclOrder = i
		if spec.Version == Version1 {
			strokes[i].Group = "default"
		}
	}
	if spec.Version == Version1 {
		return strokes, []Group{{Name: "default", Stage: 0}}, nil
	}
	return strokes, []Group{
		{Name: "body", Stage: 10},
		{Name: "detail", Stage: 20},
		{Name: "repair", Stage: 30},
	}, nil
}

func renderIcon(p *Project, ideal []uint8, variation int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, p.Width, p.Height))
	foreground := color.RGBA{R: 42, G: 134, B: 212, A: 255}
	for y := 0; y < p.Height; y++ {
		for x := 0; x < p.Width; x++ {
			idx := y*p.Width + x
			// Background is deliberately close to the foreground hue.
			n := int(hashPixel(p.Seed, x, y, variation) % 9)
			bg := color.RGBA{
				R: uint8(44 + x*28/max(1, p.Width) + n),
				G: uint8(92 + y*22/max(1, p.Height) + n),
				B: uint8(128 + (x+y)*18/max(1, p.Width+p.Height) + n),
				A: 255,
			}
			if ideal[idx] != 0 {
				noise := int(hashPixel(p.Seed+7, x, y, variation) % 11)
				bg = color.RGBA{
					R: clampU8(int64(int(foreground.R) + noise - 5)),
					G: clampU8(int64(int(foreground.G) + noise - 5)),
					B: clampU8(int64(int(foreground.B) + noise - 5)),
					A: 255,
				}
				boundary := false
				for _, d := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
					nx, ny := x+d[0], y+d[1]
					if nx < 0 || nx >= p.Width || ny < 0 || ny >= p.Height || ideal[ny*p.Width+nx] == 0 {
						boundary = true
					}
				}
				if boundary {
					bg.A = uint8(128 + hashPixel(p.Seed+19, x, y, 0)%64)
				}
			}
			img.SetRGBA(x, y, bg)
		}
	}
	return img
}

func hashPixel(seed int64, x, y, salt int) uint64 {
	v := uint64(seed) ^ uint64(x+1)*0x9e3779b97f4a7c15 ^ uint64(y+7)*0xbf58476d1ce4e5b9 ^ uint64(salt+13)
	v ^= v >> 30
	v *= 0xbf58476d1ce4e5b9
	v ^= v >> 27
	v *= 0x94d049bb133111eb
	v ^= v >> 31
	return v
}

func intMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func intMax(a, b int) int {
	if a > b {
		return a
	}
	return b
}
