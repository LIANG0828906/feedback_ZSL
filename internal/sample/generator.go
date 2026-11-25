package sample

import (
	"fmt"

	"semanticshading/internal/apierr"
	"semanticshading/internal/colormetric"
	"semanticshading/internal/model"
)

type Config struct {
	Name         string
	Width        int
	Height       int
	Channels     int
	TileSize     int
	QuantGear    int
	Seed         int64
	Metric       string
	WindowRadius int
	BandWidth    int
}

type Sample struct {
	Manifest     model.Manifest
	Ref          []byte
	Recon        []byte
	Labels       []byte
	Truth        model.Truth
	ManifestHash string
	InputHash    string
}

type rect struct {
	label     int
	name      string
	x0, y0    int
	x1, y1    int
	center    model.Point
	samplePts []model.Point
}

func DefaultConfig() Config {
	return Config{
		Name:         "ui-semantic-checkerboard",
		Width:        64,
		Height:       48,
		Channels:     4,
		TileSize:     16,
		QuantGear:    3,
		Seed:         0x5EED279,
		Metric:       "rgb_l2_q8",
		WindowRadius: 1,
	}
}

func shapes() []rect {
	return []rect{
		{label: 1, name: "solid_coral", x0: 4, y0: 4, x1: 20, y1: 20, samplePts: []model.Point{{X: 8, Y: 8}, {X: 15, Y: 15}, {X: 18, Y: 18}}},
		{label: 2, name: "solid_teal", x0: 28, y0: 4, x1: 56, y1: 18, samplePts: []model.Point{{X: 32, Y: 8}, {X: 47, Y: 10}, {X: 53, Y: 15}}},
		{label: 3, name: "gradient_band", x0: 4, y0: 24, x1: 60, y1: 34, samplePts: []model.Point{{X: 8, Y: 28}, {X: 31, Y: 29}, {X: 55, Y: 31}}},
		{label: 4, name: "texture_grid", x0: 24, y0: 36, x1: 40, y1: 46, samplePts: []model.Point{{X: 27, Y: 39}, {X: 31, Y: 41}, {X: 37, Y: 43}}},
	}
}

func QuantBitsForGear(gear int) int {
	return 9 - gear
}

func QuantStep(bits int) int {
	if bits >= 8 {
		return 1
	}
	levels := 1 << bits
	step := 256 / levels
	if step < 1 {
		return 1
	}
	return step
}

func QuantizeChannel(v byte, step int) byte {
	if step <= 1 {
		return v
	}
	iv := int(v)
	q := (iv / step) * step
	r := iv - q
	if r*2 >= step {
		q += step
	}
	if q > 255 {
		q = 255
	}
	return byte(q)
}

func permutationFor(bx, by int, seed int64, gear int) [4]int {
	patterns := [][4]int{
		{0, 1, 2, 3},
		{1, 0, 2, 3},
		{2, 1, 0, 3},
		{0, 2, 1, 3},
		{1, 2, 0, 3},
		{2, 0, 1, 3},
	}
	if gear <= 1 {
		return patterns[0]
	}
	if gear == 2 {
		if (bx+by)%2 == 0 {
			return patterns[0]
		}
		return patterns[1]
	}
	h := uint64(seed) ^ uint64(bx*73856093) ^ uint64(by*19349663) ^ uint64(gear*83492791)
	return patterns[int(h%uint64(len(patterns)))]
}

func referencePixel(x, y int) [4]byte {
	pixel := [4]byte{28, 30, 38, 255}
	for _, shape := range shapes() {
		if x < shape.x0 || x >= shape.x1 || y < shape.y0 || y >= shape.y1 {
			continue
		}
		switch shape.label {
		case 1:
			pixel = [4]byte{214, 83, 72, 255}
		case 2:
			pixel = [4]byte{64, 178, 166, 255}
		case 3:
			lx := x - shape.x0
			ly := y - shape.y0
			r := 72 + lx*3
			g := 110 + ly*8
			b := 214 - lx*2
			pixel = [4]byte{clampByte(r), clampByte(g), clampByte(b), 255}
		case 4:
			lx := x - shape.x0
			ly := y - shape.y0
			base := [3]int{226, 184, 76}
			if ((lx/2)+(ly/2))%2 == 0 {
				for c := range base {
					base[c] -= 42
				}
			}
			if lx%4 == 0 || ly%4 == 0 {
				base[0] += 24
				base[1] += 18
				base[2] -= 10
			}
			pixel = [4]byte{clampByte(base[0]), clampByte(base[1]), clampByte(base[2]), 255}
		}
	}
	return pixel
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

func Generate(cfg Config) (*Sample, error) {
	if cfg.Name == "" {
		cfg.Name = DefaultConfig().Name
	}
	if cfg.Width == 0 {
		cfg.Width = DefaultConfig().Width
	}
	if cfg.Height == 0 {
		cfg.Height = DefaultConfig().Height
	}
	if cfg.Channels == 0 {
		cfg.Channels = 4
	}
	if cfg.TileSize == 0 {
		cfg.TileSize = 16
	}
	if cfg.Seed == 0 {
		cfg.Seed = DefaultConfig().Seed
	}
	if cfg.Metric == "" {
		cfg.Metric = "rgb_l2_q8"
	}
	if cfg.WindowRadius == 0 {
		cfg.WindowRadius = 1
	}
	if cfg.QuantGear == 0 {
		cfg.QuantGear = 3
	}

	manifest := model.Manifest{
		SchemaVersion: model.SchemaVersion,
		Name:          cfg.Name,
		Width:         cfg.Width,
		Height:        cfg.Height,
		Channels:      cfg.Channels,
		TileSize:      cfg.TileSize,
		QuantGear:     cfg.QuantGear,
		QuantBits:     QuantBitsForGear(cfg.QuantGear),
		Seed:          cfg.Seed,
		Categories:    []string{"background", "solid_coral", "solid_teal", "gradient_band", "texture_grid"},
		Metric:        cfg.Metric,
		WindowRadius:  cfg.WindowRadius,
		BandWidth:     cfg.BandWidth,
		ParamVersion:  model.ParamVersion,
		AlgoVersion:   model.AlgorithmVersion,
	}
	if manifest.BandWidth == 0 {
		manifest.BandWidth = colormetric.RecommendedBand(manifest.TileSize, manifest.WindowRadius)
	}
	if err := ValidateManifest(manifest); err != nil {
		return nil, err
	}

	pixelCount := manifest.Width * manifest.Height
	ref := make([]byte, pixelCount*manifest.Channels)
	recon := make([]byte, pixelCount*manifest.Channels)
	labels := make([]byte, pixelCount)
	step := QuantStep(manifest.QuantBits)

	for y := 0; y < manifest.Height; y++ {
		for x := 0; x < manifest.Width; x++ {
			p := referencePixel(x, y)
			offset := (y*manifest.Width + x) * manifest.Channels
			for c := 0; c < manifest.Channels; c++ {
				ref[offset+c] = p[c]
			}
			label := 0
			for _, shape := range shapes() {
				if x >= shape.x0 && x < shape.x1 && y >= shape.y0 && y < shape.y1 {
					label = shape.label
					break
				}
			}
			labels[y*manifest.Width+x] = byte(label)

			bx := x / manifest.TileSize
			by := y / manifest.TileSize
			perm := permutationFor(bx, by, manifest.Seed, manifest.QuantGear)
			quantized := [4]byte{}
			for c := 0; c < 4; c++ {
				quantized[c] = QuantizeChannel(p[c], step)
			}
			for c := 0; c < manifest.Channels; c++ {
				recon[offset+c] = quantized[perm[c]]
			}
		}
	}

	truth, err := buildTruth(manifest, ref, recon, labels)
	if err != nil {
		return nil, err
	}
	sample := &Sample{
		Manifest:     manifest,
		Ref:          ref,
		Recon:        recon,
		Labels:       labels,
		Truth:        truth,
		ManifestHash: model.CanonicalHash(manifest),
	}
	sample.InputHash = model.BytesHash([]byte(sample.ManifestHash), ref, recon, labels)
	return sample, nil
}

func buildTruth(m model.Manifest, ref, recon, labels []byte) (model.Truth, error) {
	regions := make([]model.RegionTruth, 0, len(m.Categories))
	for label := 0; label < len(m.Categories); label++ {
		runs := maskRuns(labels, m.Width, m.Height, byte(label))
		area := 0
		sx, sy := 0, 0
		for _, run := range runs {
			runWidth := run.X1 - run.X0
			area += runWidth
			for x := run.X0; x < run.X1; x++ {
				sx += x
				sy += run.Y
			}
		}
		if area == 0 {
			continue
		}
		acc := model.Accumulator{Label: label}
		for y := 0; y < m.Height; y++ {
			for x := 0; x < m.Width; x++ {
				idx := y*m.Width + x
				if int(labels[idx]) != label {
					continue
				}
				d, _ := colormetric.WindowDistanceQ8(ref, recon, m.Width, m.Height, m.Channels, x, y, m.WindowRadius, m.Metric)
				acc.Area++
				acc.SumDistanceQ8 += d
				acc.SumSquaredQ16 += d * d
			}
		}
		for y := 0; y < m.Height; y++ {
			for x := 0; x < m.Width; x++ {
				if int(labels[y*m.Width+x]) != label {
					continue
				}
				if x+1 < m.Width && labels[y*m.Width+x+1] != byte(label) {
					jump, _ := colormetric.DifferenceJumpQ8(ref, recon, m.Width, m.Height, m.Channels, model.Point{X: x, Y: y}, model.Point{X: x + 1, Y: y}, m.WindowRadius, m.Metric)
					acc.BoundaryEdges++
					acc.BoundarySumQ8 += jump
				}
				if y+1 < m.Height && labels[(y+1)*m.Width+x] != byte(label) {
					jump, _ := colormetric.DifferenceJumpQ8(ref, recon, m.Width, m.Height, m.Channels, model.Point{X: x, Y: y}, model.Point{X: x, Y: y + 1}, m.WindowRadius, m.Metric)
					acc.BoundaryEdges++
					acc.BoundarySumQ8 += jump
				}
			}
		}
		mean := int64(0)
		boundary := int64(0)
		if acc.Area > 0 {
			mean = acc.SumDistanceQ8 / acc.Area
		}
		if acc.BoundaryEdges > 0 {
			boundary = acc.BoundarySumQ8 / acc.BoundaryEdges
		}
		std, _ := colormetric.StdFromSums(acc.SumDistanceQ8, acc.SumSquaredQ16, acc.Area)
		center := model.Point{X: sx / area, Y: sy / area}
		regions = append(regions, model.RegionTruth{
			Label:              label,
			Name:               m.Categories[label],
			Area:               area,
			Center:             center,
			MaskRuns:           runs,
			ExpectedMeanQ8:     mean,
			ExpectedStdQ8:      std,
			ExpectedBoundaryQ8: boundary,
		})
	}

	samples := make([]model.SampleExpectation, 0)
	for _, shape := range shapes() {
		for _, p := range shape.samplePts {
			off := (p.Y*m.Width + p.X) * m.Channels
			d := colormetric.PixelDistanceQ8(ref, recon, m.Channels, off, m.Metric)
			samples = append(samples, model.SampleExpectation{X: p.X, Y: p.Y, Label: shape.label, Metric: m.Metric, ExpectedDistanceQ8: d})
		}
	}

	truth := model.Truth{
		TruthVersion: model.TruthVersion,
		Width:        m.Width,
		Height:       m.Height,
		Channels:     m.Channels,
		TileSize:     m.TileSize,
		QuantGear:    m.QuantGear,
		QuantBits:    m.QuantBits,
		Seed:         m.Seed,
		Metric:       m.Metric,
		WindowRadius: m.WindowRadius,
		Tolerance: model.Tolerance{
			AreaAbs:             0,
			MeanAbsQ8:           1,
			StdAbsQ8:            2,
			BoundaryAbsQ8:       2,
			SampleDistanceAbsQ8: 1,
			BoundAbsQ8:          0,
		},
		Regions: regions,
		Samples: samples,
	}
	truth.TruthFingerprint = model.CanonicalHash(truth)
	return truth, nil
}

func maskRuns(labels []byte, width, height int, label byte) []model.MaskRun {
	runs := make([]model.MaskRun, 0)
	for y := 0; y < height; y++ {
		x := 0
		for x < width {
			for x < width && labels[y*width+x] != label {
				x++
			}
			x0 := x
			for x < width && labels[y*width+x] == label {
				x++
			}
			if x > x0 {
				runs = append(runs, model.MaskRun{Y: y, X0: x0, X1: x})
			}
		}
	}
	return runs
}

func DescribeError(err error) string {
	if ae, ok := apierr.As(err); ok {
		return fmt.Sprintf("%s field=%s loc=%+v", ae.Code, ae.Field, ae.Location)
	}
	return err.Error()
}
