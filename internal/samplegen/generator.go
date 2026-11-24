package samplegen

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"fixedsampler/internal/fixedpoint"
	"fixedsampler/internal/loader"
	"fixedsampler/internal/model"
)

const DefaultSeed int64 = 20261125

type rawPlane struct {
	plane *model.Plane
	raw   []int64
}

type builder struct {
	rng     *rand.Rand
	q       int64
	project *model.Project
	raw     map[string]*rawPlane
}

func GenerateAll(outDir string, seed int64) ([]string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	dirs := []string{"blocks420", "rampcolor444", "mixed422"}
	ids := []string{"blocks420", "rampcolor444", "mixed422"}
	var manifests []string
	for i, name := range dirs {
		dir := filepath.Join(outDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		var p *model.Project
		var t *model.TruthManifest
		var err error
		switch i {
		case 0:
			p, t, err = buildBlocks420(seed + int64(i)*1009)
		case 1:
			p, t, err = buildRampColor444(seed + int64(i)*1009)
		default:
			p, t, err = buildMixed422(seed + int64(i)*1009)
		}
		if err != nil {
			return nil, err
		}
		p.TruthFile = "truth.json"
		t.ErrorBounds = analyticErrorBounds(p)
		t.Fingerprint = loader.TruthFingerprint(t)
		p.TruthFingerprint = t.Fingerprint
		if err := writeJSON(filepath.Join(dir, "truth.json"), t); err != nil {
			return nil, err
		}
		if err := loader.SaveProject(filepath.Join(dir, "project.json"), p); err != nil {
			return nil, err
		}
		manifests = append(manifests, filepath.Join(dir, "project.json"))
	}
	index := map[string]any{"seed": seed, "projects": ids, "manifests": manifests}
	if err := writeJSON(filepath.Join(outDir, "index.json"), index); err != nil {
		return nil, err
	}
	return manifests, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func newBuilder(id string, seed int64, w, h int, chroma string) *builder {
	q := int64(1_000_000)
	return &builder{
		rng: rand.New(rand.NewSource(seed)),
		q:   q,
		project: &model.Project{
			SchemaVersion:   "fixed-sample/v1",
			ProjectID:       id,
			Seed:            seed,
			FullWidth:       w,
			FullHeight:      h,
			ChromaFormat:    chroma,
			ValueScale:      q,
			SupportedDepths: []int{8, 10, 12, 16},
			ParamVersion:    model.ParamVersion,
			AlgoVersion:     model.AlgoVersion,
		},
		raw: map[string]*rawPlane{},
	}
}

func (b *builder) addPlane(name, role string, w, h, sx, sy, bits int, fill func(x, y int, rng *rand.Rand) int64) {
	r := make([]int64, w*h)
	p := &model.Plane{
		Name: name, Role: role, Width: w, Height: h, SubsampleX: sx, SubsampleY: sy, BitDepth: bits,
		ColorSpace: "Native", Values: make([]int64, w*h),
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := fill(x, y, b.rng)
			r[y*w+x] = v
			p.Values[y*w+x] = v * b.q
		}
	}
	b.project.Planes = append(b.project.Planes, p)
	b.raw[name] = &rawPlane{plane: p, raw: r}
}

func codeToDepth(v int64, fromBits, toBits int) int64 {
	fromMax := fixedpoint.MaxCode(fromBits)
	toMax := fixedpoint.MaxCode(toBits)
	return fixedpoint.RoundNearest(v*toMax, fromMax)
}

func rectValue(x, y int, rect model.Rect, inside, outside int64) int64 {
	if rect.Contains(x, y) {
		return inside
	}
	return outside
}

func (b *builder) boxStage(seq int, factor int, planes []string, scale int64) model.StageConfig {
	return model.StageConfig{
		Seq: seq, ID: fmt.Sprintf("box-x%d-%d", factor, seq), Type: "resample",
		Kernel: "box", Factor: factor, FactorX: factor, FactorY: factor,
		Taps: factor * factor, FixedScale: scale, Planes: planes,
		Description: fmt.Sprintf("deterministic fixed-point box downsample x%d", factor),
	}
}

func (b *builder) nearestStage(seq int, factor int, planes []string, scale int64) model.StageConfig {
	return model.StageConfig{
		Seq: seq, ID: fmt.Sprintf("nearest-x%d-%d", factor, seq), Type: "resample",
		Kernel: "nearest", Factor: factor, FactorX: factor, FactorY: factor,
		Taps: 1, FixedScale: scale, Planes: planes,
	}
}

func quantStage(seq, bits int, planes []string) model.StageConfig {
	return model.StageConfig{
		Seq: seq, ID: fmt.Sprintf("quant-%d-%d", bits, seq), Type: "quantize",
		BitDepth: bits, Planes: planes,
	}
}

func bt601InverseMatrix(q int64) *model.FixedMatrix {
	m := func(num, den int64) int64 { return fixedpoint.RoundNearest(num*q, den) }
	center := int64(512)
	coeff := [3][3]int64{
		{q, 0, m(1402, 1000)},
		{q, -m(344136, 1_000_000), -m(714136, 1_000_000)},
		{q, m(1772, 1000), 0},
	}
	offsets := [3]int64{}
	centers := [3]int64{0, center, center}
	for row := 0; row < 3; row++ {
		var sum int64
		for col := 0; col < 3; col++ {
			sum += coeff[row][col] * centers[col] * q
		}
		offsets[row] = -fixedpoint.RoundNearest(sum, q)
	}
	return &model.FixedMatrix{Scale: q, Rows: coeff, Offsets: offsets}
}

func buildBlocks420(seed int64) (*model.Project, *model.TruthManifest, error) {
	b := newBuilder("blocks420", seed, 48, 32, "420")
	yBase := codeToDepth(42, 8, 12)
	yBlock := codeToDepth(214, 8, 12)
	cbBase := codeToDepth(128, 8, 12)
	cbBlock := codeToDepth(196, 8, 12)
	crBase := codeToDepth(128, 8, 12)
	crBlock := codeToDepth(72, 8, 12)
	block := model.Rect{X0: 16, Y0: 8, X1: 32, Y1: 24}
	chromaBlock := model.Rect{X0: 8, Y0: 4, X1: 16, Y1: 12}
	b.addPlane("Y", "luma", 48, 32, 1, 1, 12, func(x, y int, _ *rand.Rand) int64 {
		return rectValue(x, y, block, yBlock, yBase)
	})
	b.addPlane("Cb", "chroma", 24, 16, 2, 2, 12, func(x, y int, _ *rand.Rand) int64 {
		return rectValue(x, y, chromaBlock, cbBlock, cbBase)
	})
	b.addPlane("Cr", "chroma", 24, 16, 2, 2, 12, func(x, y int, _ *rand.Rand) int64 {
		return rectValue(x, y, chromaBlock, crBlock, crBase)
	})
	b.project.Config = []model.StageConfig{
		b.boxStage(1, 2, []string{"Y", "Cb", "Cr"}, 999983),
		b.boxStage(2, 2, []string{"Y", "Cb", "Cr"}, 999983),
		quantStage(3, 8, []string{"Y", "Cb", "Cr"}),
	}
	if err := loader.ValidateProject(b.project); err != nil {
		return nil, nil, err
	}
	sim, err := newIdealSim(b.project)
	if err != nil {
		return nil, nil, err
	}
	if err := sim.runStage(1); err != nil {
		return nil, nil, err
	}
	snap1 := sim.snapshot()
	if err := sim.runStage(2); err != nil {
		return nil, nil, err
	}
	if err := sim.runStage(3); err != nil {
		return nil, nil, err
	}
	truth := &model.TruthManifest{
		SchemaVersion: "truth/v1", ProjectID: b.project.ProjectID, Seed: seed,
		AnalyticNotes: []string{
			"Values are integer fixed-point units: one source code equals value_scale units.",
			"Box truth is the exact arithmetic mean of the declared source block; no pipeline output is read.",
			"Block edges are chosen on cumulative factor boundaries, so analytic coordinates are exact.",
		},
	}
	points := []struct {
		id, plane string
		seq, x, y int
		snap      *idealSnapshot
	}{
		{"y-level1-edge", "Y", 1, 9, 5, &snap1},
		{"y-final-block", "Y", 3, 5, 3, nil},
		{"y-final-base", "Y", 3, 1, 1, nil},
		{"cb-final-block", "Cb", 3, 3, 2, nil},
		{"cr-final-block", "Cr", 3, 3, 2, nil},
	}
	for _, pt := range points {
		v, err := sim.valueAt(pt.snap, pt.plane, pt.x, pt.y)
		if err != nil {
			return nil, nil, err
		}
		truth.KnownPoints = append(truth.KnownPoints, model.KnownPoint{
			ID: pt.id, StageSeq: pt.seq, Plane: pt.plane, X: pt.x, Y: pt.y,
			ExpectedFX: v, ToleranceFX: b.q / 10,
		})
	}
	for _, edge := range []struct {
		seq   int
		axis  string
		coord int
	}{{1, "x", 8}, {1, "x", 16}, {1, "y", 4}, {1, "y", 12}, {2, "x", 4}, {2, "x", 8}, {2, "y", 2}, {2, "y", 6}} {
		truth.BlockBoundaries = append(truth.BlockBoundaries, model.BlockBoundary{
			ID:       fmt.Sprintf("Y-%s-%d-s%d", edge.axis, edge.coord, edge.seq),
			AfterSeq: edge.seq, Plane: "Y", Axis: edge.axis, Coordinate: edge.coord,
		})
	}
	return b.project, truth, nil
}

func buildRampColor444(seed int64) (*model.Project, *model.TruthManifest, error) {
	b := newBuilder("rampcolor444", seed, 36, 24, "444")
	b.addPlane("Y", "luma", 36, 24, 1, 1, 10, func(x, _ int, rng *rand.Rand) int64 {
		return int64(288 + 4*x + rng.Intn(5))
	})
	b.addPlane("Cb", "chroma", 36, 24, 1, 1, 10, func(_, _ int, rng *rand.Rand) int64 {
		return 520 + int64(rng.Intn(3))
	})
	b.addPlane("Cr", "chroma", 36, 24, 1, 1, 10, func(_, _ int, rng *rand.Rand) int64 {
		return 504 + int64(rng.Intn(3))
	})
	b.project.Config = []model.StageConfig{
		b.boxStage(1, 3, []string{"Y", "Cb", "Cr"}, 1_000_003),
		{
			Seq: 2, ID: "bt601-inverse", Type: "color", Intent: "ycbcr_bt601_to_rgb",
			SourceSpace: "YCbCr-BT601", TargetSpace: "RGB",
			InputNames: []string{"Y", "Cb", "Cr"}, OutputNames: []string{"R", "G", "B"},
			Matrix: bt601InverseMatrix(b.q), FixedScale: b.q,
		},
		quantStage(3, 8, []string{"R", "G", "B"}),
	}
	if err := loader.ValidateProject(b.project); err != nil {
		return nil, nil, err
	}
	sim, err := newIdealSim(b.project)
	if err != nil {
		return nil, nil, err
	}
	if err := sim.runAll(); err != nil {
		return nil, nil, err
	}
	truth := &model.TruthManifest{
		SchemaVersion: "truth/v1", ProjectID: b.project.ProjectID, Seed: seed,
		AnalyticNotes: []string{
			"BT.601 inverse matrix is stored as integer coefficients at value_scale.",
			"Known points are recomputed by the generator's independent integer reference.",
		},
	}
	for _, plane := range []string{"R", "G", "B"} {
		for _, xy := range [][2]int{{2, 2}, {7, 5}, {10, 6}} {
			x, y := xy[0], xy[1]
			v, err := sim.valueAt(nil, plane, x, y)
			if err != nil {
				return nil, nil, err
			}
			truth.KnownPoints = append(truth.KnownPoints, model.KnownPoint{
				ID: fmt.Sprintf("%s-%d-%d", plane, x, y), StageSeq: 3, Plane: plane, X: x, Y: y,
				ExpectedFX: v, ToleranceFX: b.q / 10,
			})
		}
	}
	return b.project, truth, nil
}

func buildMixed422(seed int64) (*model.Project, *model.TruthManifest, error) {
	b := newBuilder("mixed422", seed, 64, 32, "422")
	bar := model.Rect{X0: 16, Y0: 0, X1: 48, Y1: 32}
	y0 := codeToDepth(96, 8, 12)
	y1 := codeToDepth(188, 8, 12)
	b.addPlane("Y", "luma", 64, 32, 1, 1, 12, func(x, y int, _ *rand.Rand) int64 {
		return rectValue(x, y, bar, y1, y0)
	})
	b.addPlane("Cb", "chroma", 32, 32, 2, 1, 12, func(x, _ int, _ *rand.Rand) int64 {
		if x >= 8 && x < 24 {
			return codeToDepth(170, 8, 12)
		}
		return codeToDepth(128, 8, 12)
	})
	b.addPlane("Cr", "chroma", 32, 32, 2, 1, 12, func(x, _ int, _ *rand.Rand) int64 {
		if x >= 8 && x < 24 {
			return codeToDepth(90, 8, 12)
		}
		return codeToDepth(128, 8, 12)
	})
	b.project.Config = []model.StageConfig{
		b.nearestStage(1, 2, []string{"Y", "Cb", "Cr"}, 999991),
		b.boxStage(2, 2, []string{"Y", "Cb", "Cr"}, 999983),
		quantStage(3, 10, []string{"Y", "Cb", "Cr"}),
	}
	if err := loader.ValidateProject(b.project); err != nil {
		return nil, nil, err
	}
	sim, err := newIdealSim(b.project)
	if err != nil {
		return nil, nil, err
	}
	if err := sim.runAll(); err != nil {
		return nil, nil, err
	}
	truth := &model.TruthManifest{
		SchemaVersion: "truth/v1", ProjectID: b.project.ProjectID, Seed: seed,
		AnalyticNotes: []string{"Nearest followed by box exercises two kernels and 4:2:2 geometry."},
	}
	for _, pt := range []struct {
		plane string
		x, y  int
	}{{"Y", 2, 3}, {"Y", 11, 4}, {"Cb", 5, 7}, {"Cr", 6, 4}} {
		v, err := sim.valueAt(nil, pt.plane, pt.x, pt.y)
		if err != nil {
			return nil, nil, err
		}
		truth.KnownPoints = append(truth.KnownPoints, model.KnownPoint{
			ID: fmt.Sprintf("%s-%d-%d", pt.plane, pt.x, pt.y), StageSeq: 3,
			Plane: pt.plane, X: pt.x, Y: pt.y, ExpectedFX: v, ToleranceFX: b.q / 8,
		})
	}
	truth.BlockBoundaries = []model.BlockBoundary{
		{ID: "Y-bar-left", AfterSeq: 2, Plane: "Y", Axis: "x", Coordinate: 4},
		{ID: "Y-bar-right", AfterSeq: 2, Plane: "Y", Axis: "x", Coordinate: 12},
	}
	return b.project, truth, nil
}
