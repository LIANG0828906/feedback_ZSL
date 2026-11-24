package sample

import (
	"fmt"
	"math"
	"sort"

	"terrainnormalverify/internal/model"
)

const DefaultSeed int64 = 20261125

type blockSpec struct {
	id      int
	name    string
	width   int
	height  int
	originX int
	originY int
	phase   float64
}

type seamSpec struct {
	id      string
	a       int
	b       int
	axis    string
	edgeA   string
	edgeB   string
	originA [2]int
	originB [2]int
}

var blockSpecs = []blockSpec{
	{1, "west-highland", 20, 18, 0, 0, 0.20},
	{2, "east-ridge", 18, 18, 19, 0, 1.45},
	{3, "south-basin", 20, 16, 0, 17, 2.70},
	{4, "south-terrace", 18, 16, 19, 17, 2.70},
	{5, "isolated-mesa", 12, 10, 43, 0, 5.20},
	{6, "isolated-slope", 10, 8, 43, 12, 0.90},
}

var seamSpecs = []seamSpec{
	{"s1-west-ridge", 1, 2, "vertical", "east", "west", [2]int{0, 0}, [2]int{19, 0}},
	{"s2-basin-terrace", 3, 4, "vertical", "east", "west", [2]int{0, 17}, [2]int{19, 17}},
	{"s3-highland-basin", 1, 3, "horizontal", "south", "north", [2]int{0, 0}, [2]int{0, 17}},
	{"s4-ridge-terrace", 2, 4, "horizontal", "south", "north", [2]int{19, 0}, [2]int{19, 17}},
}

func Generate(seed int64) *model.Project {
	p := &model.Project{
		SchemaVersion: model.SchemaVersion,
		Seed:          seed,
		Revision:      1,
		World: model.World{
			Unit:        "sample-grid",
			HeightScale: 1,
		},
		Parameters: model.BakeParameters{
			DifferenceOperator: "central",
			Scale:              1.0,
			MaterialMix:        "weighted",
			SeamRelaxation:     0.35,
		},
		Layers: []model.MaterialLayer{
			{ID: "rock", Name: "Rock", NormalBiasX: 0.00, NormalBiasY: 0.00, NormalBiasZ: 0.00, Strength: 1.00, ParameterTag: "rock-v1"},
			{ID: "sand", Name: "Sand", NormalBiasX: 0.22, NormalBiasY: -0.11, NormalBiasZ: 0.00, Strength: 0.90, ParameterTag: "sand-v1"},
			{ID: "grass", Name: "Grass", NormalBiasX: -0.16, NormalBiasY: 0.19, NormalBiasZ: 0.00, Strength: 1.10, ParameterTag: "grass-v1"},
		},
	}
	for _, spec := range blockSpecs {
		p.Blocks = append(p.Blocks, generateBlock(p, seed, spec))
	}
	for _, spec := range seamSpecs {
		threshold := 3.0
		geometryThreshold := 22.0
		if spec.id == "s1-west-ridge" {
			geometryThreshold = 5.5
		}
		if spec.id == "s2-basin-terrace" {
			threshold = 0.1
		}
		p.Seams = append(p.Seams, model.Seam{
			ID:                        spec.id,
			BlockA:                    spec.a,
			BlockB:                    spec.b,
			Axis:                      spec.axis,
			EdgeA:                     spec.edgeA,
			EdgeB:                     spec.edgeB,
			ThresholdDeg:              threshold,
			MaxIterations:             64,
			GeometrySlopeThresholdDeg: geometryThreshold,
		})
	}
	p.Truth = generateTruth(p)
	return p
}

func generateBlock(p *model.Project, seed int64, spec blockSpec) model.Block {
	b := model.Block{
		ID:      spec.id,
		Name:    spec.name,
		Width:   spec.width,
		Height:  spec.height,
		OriginX: spec.originX,
		OriginY: spec.originY,
		Heights: make([]float64, spec.width*spec.height),
		Weights: make([][]int, spec.width*spec.height),
	}
	for y := 0; y < spec.height; y++ {
		for x := 0; x < spec.width; x++ {
			gx := float64(spec.originX + x)
			gy := float64(spec.originY + y)
			idx := y*spec.width + x
			b.Heights[idx] = round(height(seed, gx, gy), 9)
			b.Weights[idx] = quantizeWeights(seed, spec, gx, gy)
		}
	}
	return b
}

func height(seed int64, x, y float64) float64 {
	phase := float64(seed%100003) * 0.000071
	t1 := math.Sin(0.17*x+0.9*phase) * math.Cos(0.13*y-0.4*phase)
	t2 := math.Cos(0.09*x - 0.11*y + 0.5*phase)
	t3 := math.Sin(0.31*x + 0.23*y + 1.3*phase)
	return 0.35*t1 + 0.18*t2 + 0.065*t3
}

func heightGradient(seed int64, x, y float64) (float64, float64) {
	phase := float64(seed%100003) * 0.000071
	a, b := 0.17, 0.13
	c, d := 0.09, 0.11
	e, f := 0.31, 0.23
	t1x := a * math.Cos(a*x+0.9*phase) * math.Cos(b*y-0.4*phase)
	t1y := -b * math.Sin(a*x+0.9*phase) * math.Sin(b*y-0.4*phase)
	t2x := -c * math.Sin(c*x-d*y+0.5*phase)
	t2y := d * math.Sin(c*x-d*y+0.5*phase)
	t3x := e * math.Cos(e*x+f*y+1.3*phase)
	t3y := f * math.Cos(e*x+f*y+1.3*phase)
	return 0.35*t1x + 0.18*t2x + 0.065*t3x,
		0.35*t1y + 0.18*t2y + 0.065*t3y
}

func quantizeWeights(seed int64, spec blockSpec, gx, gy float64) []int {
	phase := float64(seed%9973)*0.00037 + spec.phase
	rock := 0.34 + 0.20*math.Sin(0.21*gx+0.17*gy+phase)
	sand := 0.25 + 0.18*math.Cos(0.13*gx-0.19*gy+phase*1.7)
	grass := 0.31 + 0.20*math.Sin(0.07*gx+0.24*gy+phase*0.6)
	if spec.id == 6 {
		grass = 0
	}
	rock = math.Max(0.02, rock)
	sand = math.Max(0.02, sand)
	grass = math.Max(0, grass)
	total := rock + sand + grass
	w0 := int(math.Round(rock / total * model.WeightScale))
	w1 := int(math.Round(sand / total * model.WeightScale))
	if w0+w1 > model.WeightScale {
		w1 = model.WeightScale - w0
	}
	if w0 < 0 {
		w0 = 0
	}
	if w1 < 0 {
		w1 = 0
	}
	w2 := model.WeightScale - w0 - w1
	return []int{w0, w1, w2}
}

func generateTruth(p *model.Project) model.TruthManifest {
	var manifest model.TruthManifest
	for _, block := range p.SortedBlocks() {
		points := [][2]int{
			{block.Width / 4, block.Height / 4},
			{block.Width / 2, block.Height / 3},
			{(block.Width * 3) / 4, block.Height / 2},
			{block.Width / 3, (block.Height * 3) / 4},
		}
		var truth model.BlockTruth
		truth.BlockID = block.ID
		seen := make(map[[2]int]bool)
		for _, point := range points {
			if seen[point] {
				continue
			}
			seen[point] = true
			n := analyticNormal(p, &block, point[0], point[1])
			truth.Pixels = append(truth.Pixels, model.PixelTruth{
				X:         point[0],
				Y:         point[1],
				Normal:    n,
				SlopeDeg:  model.SlopeDeg(n),
				Tolerance: 0.035,
			})
		}
		manifest.Blocks = append(manifest.Blocks, truth)
	}
	for _, seam := range p.SortedSeams() {
		expectedAngle := analyticSeamAngle(p, seam)
		manifest.Seams = append(manifest.Seams, model.SeamTruth{
			SeamID:                   seam.ID,
			ThresholdDeg:             seam.ThresholdDeg,
			ExpectedContinuous:       true,
			ExpectedAdjacentAngleDeg: expectedAngle,
			ToleranceDeg:             1.0,
		})
	}
	return manifest
}

func analyticSeamAngle(p *model.Project, seam model.Seam) float64 {
	a, okA := p.Block(seam.BlockA)
	b, okB := p.Block(seam.BlockB)
	if !okA || !okB {
		return 0
	}
	maxAngle := 0.0
	switch seam.Axis {
	case "vertical":
		for y := 0; y < a.Height; y++ {
			na := analyticNormal(p, a, a.Width-1, y)
			nb := analyticNormal(p, b, 0, y)
			maxAngle = math.Max(maxAngle, model.AngleDeg(na, nb))
		}
	case "horizontal":
		for x := 0; x < a.Width; x++ {
			na := analyticNormal(p, a, x, a.Height-1)
			nb := analyticNormal(p, b, x, 0)
			maxAngle = math.Max(maxAngle, model.AngleDeg(na, nb))
		}
	}
	return round(maxAngle, 9)
}

func analyticNormal(p *model.Project, block *model.Block, x, y int) model.Vec3 {
	gx, gy := heightGradient(p.Seed, float64(block.OriginX+x), float64(block.OriginY+y))
	base := model.Vec3{
		X: -gx * p.Parameters.Scale,
		Y: -gy * p.Parameters.Scale,
		Z: 1,
	}.Normalize()
	weights := block.WeightsAt(x, y)
	total := 0
	for _, weight := range weights {
		total += weight
	}
	acc := model.Vec3{}
	for i, layer := range p.Layers {
		weight := float64(weights[i]) / float64(total)
		if weight == 0 {
			continue
		}
		layerNormal := model.Vec3{
			X: base.X + layer.NormalBiasX*layer.Strength,
			Y: base.Y + layer.NormalBiasY*layer.Strength,
			Z: base.Z + layer.NormalBiasZ*layer.Strength,
		}.Normalize()
		acc = acc.Add(layerNormal.Mul(weight))
	}
	return acc.Normalize()
}

func round(v float64, places int) float64 {
	pow := math.Pow10(places)
	return math.Round(v*pow) / pow
}

func Explain(p *model.Project) string {
	return fmt.Sprintf("seed=%d blocks=%d seams=%d truthPixels=%d algorithm=%s",
		p.Seed, len(p.Blocks), len(p.Seams), truthPixelCount(p), model.AlgorithmVersion)
}

func truthPixelCount(p *model.Project) int {
	total := 0
	for _, block := range p.Truth.Blocks {
		total += len(block.Pixels)
	}
	return total
}

func BlockIDs(p *model.Project) []int {
	ids := make([]int, 0, len(p.Blocks))
	for _, block := range p.Blocks {
		ids = append(ids, block.ID)
	}
	sort.Ints(ids)
	return ids
}
