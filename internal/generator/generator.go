package generator

import (
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"sort"

	"occlusionlab/internal/geometry"
	"occlusionlab/internal/model"
	"occlusionlab/internal/schema"
)

type Config struct {
	Seed         int64
	Quantization model.Quantization
	TolerancePxQ int64
}

func DefaultConfig(seed int64) Config {
	return Config{
		Seed: seed,
		Quantization: model.Quantization{
			WorldFracBits:  8,
			BasisFracBits:  16,
			ScreenFracBits: 8,
			ParamFracBits:  16,
		},
		TolerancePxQ: 2 << 8,
	}
}

func quant(v float64, bits uint8) int64 {
	return int64(math.Round(v * float64(int64(1)<<bits)))
}

func normalize(v [3]float64) [3]float64 {
	n := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	if n == 0 {
		return [3]float64{0, 0, 1}
	}
	return [3]float64{v[0] / n, v[1] / n, v[2] / n}
}

func cross(a, b [3]float64) [3]float64 {
	return [3]float64{
		a[1]*b[2] - a[2]*b[1],
		a[2]*b[0] - a[0]*b[2],
		a[0]*b[1] - a[1]*b[0],
	}
}

func vec(q model.Quantization, v [3]float64) model.Vec3 {
	return model.Vec3{
		X: quant(v[0], q.WorldFracBits),
		Y: quant(v[1], q.WorldFracBits),
		Z: quant(v[2], q.WorldFracBits),
	}
}

func basisVec(q model.Quantization, v [3]float64) model.Vec3 {
	return model.Vec3{
		X: quant(v[0], q.BasisFracBits),
		Y: quant(v[1], q.BasisFracBits),
		Z: quant(v[2], q.BasisFracBits),
	}
}

func makeCamera(q model.Quantization, window int, time int64, pos [3]float64,
	yaw, pitch float64, tanX, tanY float64, width, height int) model.CameraSample {

	cp := math.Cos(pitch)
	forward := normalize([3]float64{
		math.Sin(yaw) * cp,
		math.Sin(pitch),
		math.Cos(yaw) * cp,
	})
	right := normalize([3]float64{math.Cos(yaw), 0, -math.Sin(yaw)})
	up := normalize(cross(forward, right))
	tanXQ := quant(tanX, q.ParamFracBits)
	tanYQ := quant(tanY, q.ParamFracBits)
	focalX := int64(math.Round(
		(float64(width) * 0.5 * float64(int64(1)<<q.ParamFracBits) / float64(tanXQ)) *
			float64(int64(1)<<q.ScreenFracBits)))
	focalY := int64(math.Round(
		(float64(height) * 0.5 * float64(int64(1)<<q.ParamFracBits) / float64(tanYQ)) *
			float64(int64(1)<<q.ScreenFracBits)))
	return model.CameraSample{
		Window:       window,
		Time:         time,
		Position:     vec(q, pos),
		Right:        basisVec(q, right),
		Up:           basisVec(q, up),
		Forward:      basisVec(q, forward),
		Near:         quant(24, q.WorldFracBits),
		Far:          quant(4800, q.WorldFracBits),
		TanHalfX:     tanXQ,
		TanHalfY:     tanYQ,
		FocalX:       focalX,
		FocalY:       focalY,
		CenterX:      int64(width) * int64(1) << q.ScreenFracBits / 2,
		CenterY:      int64(height) * int64(1) << q.ScreenFracBits / 2,
		ScreenWidth:  width,
		ScreenHeight: height,
	}
}

func makeChunk(q model.Quantization, index int, label string, x, y, z, hx, hy, hz float64,
	refs []string, complexity, loadCost int, color string) model.Chunk {

	min := vec(q, [3]float64{x - hx, y - hy, z - hz})
	max := vec(q, [3]float64{x + hx, y + hy, z + hz})
	box := model.AABB{Min: min, Max: max}
	return model.Chunk{
		Index:        index,
		Label:        label,
		Bounds:       box,
		Anchor:       box.Center(),
		OccluderRefs: append([]string(nil), refs...),
		Complexity:   complexity,
		LoadCost:     loadCost,
		Color:        color,
	}
}

func makeOccluder(q model.Quantization, id, kind string, x0, y0, z0, x1, y1, z1 float64,
	source int, color string) model.Occluder {

	return model.Occluder{
		ID:          id,
		Kind:        kind,
		Bounds:      model.AABB{Min: vec(q, [3]float64{x0, y0, z0}), Max: vec(q, [3]float64{x1, y1, z1})},
		SourceChunk: source,
		Color:       color,
	}
}

func Generate(cfg Config) (*model.Project, *model.AppError) {
	if err := schema.ValidateQuantization(cfg.Quantization); err != nil {
		return nil, err
	}
	if cfg.TolerancePxQ <= 0 {
		return nil, model.ParamError("tolerance_px_q", "容差必须为正定点像素值")
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	q := cfg.Quantization

	occluders := []model.Occluder{
		makeOccluder(q, "wall_left", "wall", -600, 24, 64, -64, 336, 128, 0, "#7f8ea3"),
		makeOccluder(q, "wall_right", "wall", 64, 24, 64, 600, 336, 128, 4, "#7f8ea3"),
		makeOccluder(q, "pillar", "pillar", -28, 24, 640, 28, 204, 704, 7, "#9b8ac4"),
	}

	refs := []string{"wall_left", "wall_right", "pillar"}
	columns := []float64{-480, -160, 0, 160, 480}
	rows := []float64{96, 180, 264}
	colors := []string{
		"#56b4e9", "#009e73", "#e69f00", "#cc79a7", "#8da0cb",
		"#66c2a5", "#fc8d62", "#a6d854", "#ffd92f", "#e78ac3",
		"#8dd3c7", "#bebada", "#fb8072", "#80b1d3", "#b3de69",
	}
	chunks := make([]model.Chunk, 0, len(columns)*len(rows))
	index := 0
	for row, y := range rows {
		for col, x := range columns {
			seedX := float64((cfg.Seed%3)-1) * 20
			seedZ := float64((cfg.Seed%5)-2) * 18
			hx := 58 + float64((int(cfg.Seed)+index)%7)
			hy := 36 + float64((int(cfg.Seed)+index)%5)
			hz := 46 + float64((int(cfg.Seed)+index)%9)
			chunks = append(chunks, makeChunk(
				q, index, fmt.Sprintf("C%02d-R%dC%d", index, row, col),
				x+seedX, y+float64(row*4), 1400+seedZ+float64(col*12),
				hx, hy, hz, refs,
				800+index*173+int(cfg.Seed%97),
				1+(index%5)+col,
				colors[index%len(colors)],
			))
			index++
		}
	}
	camX := []float64{-160, 0, 160, 240, 120, 0}
	camYaw := []float64{-0.18, -0.08, 0.02, 0.12, 0.20, 0.08}
	camPitch := []float64{0, 0.01, 0, -0.01, 0.01, 0}
	camYJitter := float64(rng.Intn(7)-3) * 0.75
	cameras := make([]model.CameraSample, len(camX))
	for i := range camX {
		cameras[i] = makeCamera(
			q, i, int64(1000+i*250),
			[3]float64{camX[i], 180 + camYJitter, -720},
			camYaw[i], camPitch[i],
			0.64, 0.36, 1280, 720,
		)
	}

	project := &model.Project{
		Meta: model.ProjectMeta{
			SchemaVersion:    model.SchemaVersion,
			GeneratorVersion: model.GeneratorVersion,
			Seed:             cfg.Seed,
			Title:            fmt.Sprintf("Canyon streaming sample #%d", cfg.Seed),
			Precision:        q,
			TolerancePxQ:     cfg.TolerancePxQ,
		},
		World: model.World{
			WorldBounds: model.AABB{
				Min: vec(q, [3]float64{-800, -128, -1024}),
				Max: vec(q, [3]float64{800, 512, 2048}),
			},
			Chunks:    chunks,
			Occluders: occluders,
			Cameras:   cameras,
		},
		Truth: model.Truth{
			TolerancePxQ: cfg.TolerancePxQ,
			OcclusionRule: "exact anchor containment in the rational projection of each referenced AABB, " +
				"with occluder max depth strictly closer than the chunk anchor",
			ProjectionRule: "rational projection using the exact declared quantized integers",
		},
	}
	if err := BuildTruth(project); err != nil {
		return nil, err
	}
	project.ContentHash = model.ProjectContentHash(project.Meta, project.World, project.Truth)
	if err := schema.ValidateProject(project); err != nil {
		return nil, err
	}
	return project, nil
}

func GenerateToDir(dir string, cfg Config) (*model.Project, *model.AppError) {
	project, err := Generate(cfg)
	if err != nil {
		return nil, err
	}
	if err := schema.WriteProject(dir, project); err != nil {
		return nil, err
	}
	return project, nil
}

// BuildTruth computes ground truth solely from generator-owned construction
// data using exact rational arithmetic.
func BuildTruth(project *model.Project) *model.AppError {
	if project == nil {
		return model.SchemaError(0, "project", "项目为空")
	}
	q := project.Meta.Precision
	occByID := make(map[string]model.Occluder, len(project.World.Occluders))
	for _, occ := range project.World.Occluders {
		occByID[occ.ID] = occ
	}
	known := make([]model.KnownProjection, 0, len(project.World.Cameras)*len(project.World.Chunks))
	samples := make([]model.TruthSample, 0, len(project.World.Cameras))

	for window, cam := range project.World.Cameras {
		sample := model.TruthSample{Window: window, Time: cam.Time}
		for _, chunk := range project.World.Chunks {
			exact, ok := geometry.ExactProject(cam, chunk.Anchor, q.BasisFracBits)
			if !ok {
				sample.Chunks = append(sample.Chunks, model.TruthChunk{
					Chunk: chunk.Index, Visible: false, Reason: "outside_depth_range",
					UncertaintyMargin: 0,
				})
				continue
			}
			expectedX := geometry.RatToInt64Rounded(exact.ScreenX)
			expectedY := geometry.RatToInt64Rounded(exact.ScreenY)
			known = append(known, model.KnownProjection{
				ID:        fmt.Sprintf("W%02d-C%02d", window, chunk.Index),
				Window:    window,
				Chunk:     chunk.Index,
				World:     chunk.Anchor,
				ExpectedX: expectedX,
				ExpectedY: expectedY,
				Note:      "chunk anchor, exact rational projection",
			})

			screenW := int64(cam.ScreenWidth) << q.ScreenFracBits
			screenH := int64(cam.ScreenHeight) << q.ScreenFracBits
			if exact.ScreenX.Sign() < 0 || exact.ScreenX.Cmp(big.NewRat(screenW, 1)) > 0 ||
				exact.ScreenY.Sign() < 0 || exact.ScreenY.Cmp(big.NewRat(screenH, 1)) > 0 {
				sample.Chunks = append(sample.Chunks, model.TruthChunk{
					Chunk: chunk.Index, Visible: false, Reason: "outside_frustum",
					UncertaintyMargin: nearestBoundaryMargin(exact.ScreenX, exact.ScreenY, screenW, screenH),
				})
				continue
			}

			hidden := false
			bestRef := ""
			var bestScreenMargin int64 = int64(^uint64(0) >> 1)
			for _, ref := range chunk.OccluderRefs {
				occ, exists := occByID[ref]
				if !exists {
					continue
				}
				rect, rectOK := geometry.ExactOccluderRect(cam, occ.Bounds, q.BasisFracBits)
				if !rectOK || !rect.Contains(exact.ScreenX, exact.ScreenY) {
					continue
				}
				if exact.Depth.Cmp(rect.MaxDepth) > 0 {
					margin := rectScreenMargin(rect, exact.ScreenX, exact.ScreenY)
					if !hidden || margin < bestScreenMargin {
						hidden = true
						bestRef = occ.ID
						bestScreenMargin = margin
					}
				}
			}
			if hidden {
				sample.Chunks = append(sample.Chunks, model.TruthChunk{
					Chunk: chunk.Index, Visible: false, ReferenceOccluder: bestRef,
					Reason: "anchor_occluded", UncertaintyMargin: bestScreenMargin,
				})
			} else {
				sample.Chunks = append(sample.Chunks, model.TruthChunk{
					Chunk: chunk.Index, Visible: true, Reason: "anchor_visible",
					UncertaintyMargin: nearestBoundaryMargin(exact.ScreenX, exact.ScreenY, screenW, screenH),
				})
			}
		}
		sort.Slice(sample.Chunks, func(i, j int) bool { return sample.Chunks[i].Chunk < sample.Chunks[j].Chunk })
		samples = append(samples, sample)
	}
	project.Truth.Samples = samples
	project.Truth.KnownProjections = known
	return nil
}

func rectScreenMargin(rect geometry.ExactRect, x, y *big.Rat) int64 {
	dx := minRatSub(x, rect.MinX, rect.MaxX)
	dy := minRatSub(y, rect.MinY, rect.MaxY)
	if dx < dy {
		return dx
	}
	return dy
}

func minRatSub(v, min, max *big.Rat) int64 {
	a := geometry.RatToInt64Rounded(geometry.RatAbsSub(v, min))
	b := geometry.RatToInt64Rounded(geometry.RatAbsSub(v, max))
	if a < b {
		return a
	}
	return b
}

func nearestBoundaryMargin(x, y *big.Rat, w, h int64) int64 {
	left := geometry.RatToInt64Rounded(x)
	right := w - left
	top := geometry.RatToInt64Rounded(y)
	bottom := h - top
	m := left
	if right < m {
		m = right
	}
	if top < m {
		m = top
	}
	if bottom < m {
		m = bottom
	}
	return m
}
