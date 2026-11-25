package samplegen

import (
	"math"
	"math/rand"

	"terrainpipeline/internal/model"
)

type column struct {
	id string
	nx int
}

func Generate(seed int64) *model.Project {
	rng := rand.New(rand.NewSource(seed))
	dx, dz := 1.0, 1.0
	columns := []column{{"B01", 8}, {"B02", 10}, {"B03", 6}}
	topNZ, bottomNZ := 6, 8
	rockStrength := 0.54 + rng.Float64()*0.06
	mossStrength := 0.20 + rng.Float64()*0.06

	type placement struct {
		id     string
		nx, nz int
		ox, oz float64
	}
	var placements []placement
	x := 0.0
	for _, col := range columns {
		placements = append(placements, placement{col.id, col.nx, topNZ, x, 0})
		x += float64(col.nx-1) * dx
	}
	colWidths := []float64{float64(columns[0].nx-1) * dx, float64(columns[1].nx-1) * dx, float64(columns[2].nx-1) * dx}
	x = 0
	bottomIDs := []string{"B04", "B05", "B06"}
	bottomNX := []int{8, 10, 6}
	for i := range columns {
		placements = append(placements, placement{bottomIDs[i], bottomNX[i], bottomNZ, x, float64(topNZ-1) * dz})
		x += colWidths[i]
	}
	placements = append(placements, placement{"B07", 7, 5, 0, float64(topNZ+bottomNZ) * dz})

	layers := []model.Layer{
		{ID: "base", Name: "压实壤土", Strength: 0, Frequency: 0},
		{ID: "rock", Name: "岩层", Strength: rockStrength, Frequency: 0.72},
		{ID: "moss", Name: "苔藓", Strength: mossStrength, Frequency: 0.41},
	}

	params := model.BakeParams{
		Differential:      "central",
		HeightScale:       1.15,
		SeamThresholdDeg:  6.0,
		WeldMaxCorrection: 9.0,
	}.WithDefaults()

	project := &model.Project{
		SchemaVersion: model.SchemaVersion,
		Seed:          seed,
		Params:        params,
		Layers:        layers,
		Truth:         model.TruthSet{},
	}

	for _, pl := range placements {
		block := &model.Block{
			ID:        pl.id,
			NX:        pl.nx,
			NZ:        pl.nz,
			DX:        dx,
			DZ:        dz,
			OriginX:   pl.ox,
			OriginZ:   pl.oz,
			Elevation: make([]float64, pl.nx*pl.nz),
			Weights:   make([][]uint8, pl.nx*pl.nz),
		}
		for j := 0; j < pl.nz; j++ {
			for i := 0; i < pl.nx; i++ {
				wx := pl.ox + float64(i)*dx
				wz := pl.oz + float64(j)*dz
				h := terrainHeight(wx, wz)
				if block.ID == "B03" {
					localX := float64(i) * dx
					h += 0.62 * localX
				}
				block.Elevation[block.Index(i, j)] = round6(h)
				block.Weights[block.Index(i, j)] = defaultWeights(len(layers))
			}
		}
		assignMaterialWeights(block, layers)
		project.Blocks = append(project.Blocks, block)
	}

	project.Seams = []model.SeamConstraint{
		{ID: "S01", BlockA: "B01", BlockB: "B02", EdgeA: model.EdgeRight, EdgeB: model.EdgeLeft, SampleCount: topNZ, ThresholdDeg: params.SeamThresholdDeg, Note: "平缓连续接缝"},
		{ID: "S02", BlockA: "B02", BlockB: "B03", EdgeA: model.EdgeRight, EdgeB: model.EdgeLeft, SampleCount: topNZ, ThresholdDeg: params.SeamThresholdDeg, Note: "东侧断层式真实折角"},
		{ID: "S03", BlockA: "B04", BlockB: "B05", EdgeA: model.EdgeRight, EdgeB: model.EdgeLeft, SampleCount: bottomNZ, ThresholdDeg: params.SeamThresholdDeg, Note: "权重混合伪突变样本"},
		{ID: "S04", BlockA: "B05", BlockB: "B06", EdgeA: model.EdgeRight, EdgeB: model.EdgeLeft, SampleCount: bottomNZ, ThresholdDeg: params.SeamThresholdDeg, Note: "二阶曲率差分伪突变样本"},
	}

	addTruth(project)
	project.ParamVersion = params.ParameterVersion()
	project.ManifestHash = model.ProjectContentHash(project)
	return project
}

func terrainHeight(x, z float64) float64 {
	return 0.045*x + 0.032*z +
		0.052*(x-9.0)*(x-9.0) -
		0.035*(z-6.5)*(z-6.5)
}

func terrainDerivative(x, z float64) (float64, float64) {
	dx := 0.045 + 2*0.052*(x-9.0)
	dz := 0.032 - 2*0.035*(z-6.5)
	return dx, dz
}

func defaultWeights(n int) []uint8 {
	w := make([]uint8, n)
	w[0] = 255
	return w
}

func assignMaterialWeights(block *model.Block, layers []model.Layer) {
	rock := layerIndex(layers, "rock")
	moss := layerIndex(layers, "moss")
	for j := 0; j < block.NZ; j++ {
		for i := 0; i < block.NX; i++ {
			idx := block.Index(i, j)
			u := float64(i) / float64(block.NX-1)
			v := float64(j) / float64(block.NZ-1)
			switch block.ID {
			case "B02":
				rockAmount := quantizeWeight(0.25 + 0.55*(0.5+0.5*math.Sin(5*u+1.7*v)))
				if i < 2 {
					rockAmount = 0
				}
				setLayerWeight(block.Weights[idx], 0, rock, rockAmount)
			case "B04":
				rockAmount := quantizeWeight(0.18 + 0.50*(0.5+0.5*math.Cos(4*v-2*u)))
				setLayerWeight(block.Weights[idx], 0, rock, rockAmount)
				if i == block.NX-1 && j == 2 {
					setLayerWeight(block.Weights[idx], 0, rock, 255)
				}
				if i == block.NX-1 && j != 2 {
					block.Weights[idx] = defaultWeights(len(layers))
				}
			case "B03":
				mossAmount := quantizeWeight(0.20 + 0.45*(0.5+0.5*math.Sin(6*v)))
				if i > block.NX-3 {
					mossAmount = 0
				}
				setLayerWeight(block.Weights[idx], 0, moss, mossAmount)
			case "B05":
				mossAmount := quantizeWeight(0.22 + 0.42*(0.5+0.5*math.Cos(5*u+3*v)))
				setLayerWeight(block.Weights[idx], 0, moss, mossAmount)
				if i == 0 && j == 2 {
					block.Weights[idx] = defaultWeights(len(layers))
				}
				if i == block.NX-1 {
					block.Weights[idx] = defaultWeights(len(layers))
				}
			}
		}
	}
}

func setLayerWeight(weights []uint8, baseLayer, targetLayer int, amount int) {
	if targetLayer <= 0 || amount <= 0 {
		return
	}
	if amount > 255 {
		amount = 255
	}
	weights[targetLayer] = uint8(amount)
	weights[baseLayer] = uint8(255 - amount)
}

func quantizeWeight(v float64) int {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return int(math.Round(v*255/8) * 8)
}

func layerIndex(layers []model.Layer, id string) int {
	for i, layer := range layers {
		if layer.ID == id {
			return i
		}
	}
	return -1
}

func addTruth(p *model.Project) {
	truthPixels := map[string][][2]int{
		"B01": {{2, 2}, {5, 3}},
		"B02": {{3, 2}, {7, 4}},
		"B03": {{2, 2}},
		"B04": {{2, 5}, {5, 3}},
		"B05": {{4, 4}, {8, 5}},
		"B06": {{2, 4}},
		"B07": {{3, 2}},
	}
	for id, points := range truthPixels {
		b := p.BlockByID(id)
		for _, pt := range points {
			i, j := pt[0], pt[1]
			if !b.InBounds(i, j) {
				continue
			}
			for k := range b.Weights[b.Index(i, j)] {
				b.Weights[b.Index(i, j)][k] = 0
			}
			b.Weights[b.Index(i, j)][0] = 255
			wx := b.OriginX + float64(i)*b.DX
			wz := b.OriginZ + float64(j)*b.DZ
			dhx, dhz := terrainDerivative(wx, wz)
			if id == "B03" {
				dhx += 0.62
			}
			nx := -p.Params.HeightScale * dhx
			nzComp := -p.Params.HeightScale * dhz
			normal := normalize(model.Vec3{nx, nzComp, 1})
			slope := math.Atan(math.Hypot(p.Params.HeightScale*dhx, p.Params.HeightScale*dhz)) * 180 / math.Pi
			p.Truth.Normals = append(p.Truth.Normals, model.NormalTruth{
				BlockID: id, I: i, J: j, Normal: normal, SlopeDeg: slope, Tolerance: 0.02,
			})
		}
	}

	b2 := p.BlockByID("B02")
	b3 := p.BlockByID("B03")
	j := 3
	wxSeam := b2.OriginX + float64(b2.NX-1)*b2.DX
	wz := b2.OriginZ + float64(j)*b2.DZ
	dhxCommon, dhz := terrainDerivative(wxSeam, wz)
	dhxB3 := dhxCommon + 0.62
	n2 := normalize(model.Vec3{-p.Params.HeightScale * dhxCommon, -p.Params.HeightScale * dhz, 1})
	n3 := normalize(model.Vec3{-p.Params.HeightScale * dhxB3, -p.Params.HeightScale * dhz, 1})
	p.Truth.Seams = append(p.Truth.Seams, model.SeamTruth{
		SeamID: "S02", Index: j, AngleDeg: angleDeg(n2, n3), Tolerance: 4.0, Metric: "raw_geometry_side",
	})
	_ = b3

	p.Truth.Seams = append(p.Truth.Seams, model.SeamTruth{
		SeamID: "S01", Index: 2, AngleDeg: 0, Tolerance: 0.001, Metric: "welded",
	})
}

func normalize(v model.Vec3) model.Vec3 {
	l := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	return model.Vec3{v[0] / l, v[1] / l, v[2] / l}
}

func angleDeg(a, b model.Vec3) float64 {
	dot := a[0]*b[0] + a[1]*b[1] + a[2]*b[2]
	if dot > 1 {
		dot = 1
	}
	if dot < -1 {
		dot = -1
	}
	return math.Acos(dot) * 180 / math.Pi
}

func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}
