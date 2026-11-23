package bake

import (
	"math"
	"sort"

	"terrain-bake/project"
)

const (
	hashSalt = uint64(0x9E3779B97F4A7C15)
)

// BakeBlock is a pure, deterministic block bake. It only reads the project.
func BakeBlock(p *project.Project, blockID string, params Params) (*Artifact, *project.Error) {
	if e := params.Validate(); e != nil {
		return nil, e
	}
	bd, ok := p.Blocks[blockID]
	if !ok {
		return nil, project.NewError(project.CodeSchema, blockID, "block", "unknown block")
	}
	def := bd.Def
	layers := def.Material.Layers
	quant := params.QuantMax
	if quant != def.Material.QuantMax {
		return nil, project.NewError(project.CodeSchema, blockID, "material.quantMax", "block quantMax does not match bake params")
	}
	gfx := (def.Grid.Cols-1)*params.Subdivisions + 1
	gfz := (def.Grid.Rows-1)*params.Subdivisions + 1
	vertexCount := gfx * gfz
	if vertexCount > params.MaxVerticesPerThread {
		return nil, project.NewError(project.CodeResource, blockID, "vertices",
			"block requires %d vertices, per-thread limit is %d", vertexCount, params.MaxVerticesPerThread)
	}
	step := p.Manifest.Grid.Spacing / float64(params.Subdivisions)
	seams := seamsForBlock(p, blockID)

	fineHeights := make([]float64, vertexCount)
	for r := 0; r < gfz; r++ {
		for c := 0; c < gfx; c++ {
			fineHeights[r*gfx+c] = fineHeightAt(p, bd, c, r, params.Subdivisions)
		}
	}

	positions := make([]Vec3, vertexCount)
	for r := 0; r < gfz; r++ {
		for c := 0; c < gfx; c++ {
			localX := float64(c) / float64(params.Subdivisions) * p.Manifest.Grid.Spacing
			localZ := float64(r) / float64(params.Subdivisions) * p.Manifest.Grid.Spacing
			positions[r*gfx+c] = Vec3{
				X: def.Origin.X + localX,
				Y: fineHeights[r*gfx+c],
				Z: def.Origin.Z + localZ,
			}
		}
	}

	normals := make([]Vec3, vertexCount)
	for r := 0; r < gfz; r++ {
		for c := 0; c < gfx; c++ {
			normals[r*gfx+c] = localFineNormal(fineHeights, gfx, gfz, c, r, step)
		}
	}

	baseWeights := make([]float64, vertexCount*layers)
	for r := 0; r < gfz; r++ {
		for c := 0; c < gfx; c++ {
			raw := localMaterialWeights(p, bd, c, r, params.Subdivisions)
			copy(baseWeights[(r*gfx+c)*layers:(r*gfx+c+1)*layers], raw)
		}
	}

	if params.SeamMaxIterations > 0 {
		stitchSeamNormals(p, bd, params, gfx, gfz, step, seams, normals)
		stitchSeamMaterials(p, bd, params, gfx, gfz, seams, baseWeights)
	}

	matWeights := make([]uint16, vertexCount*layers)
	for i := 0; i < vertexCount; i++ {
		q := quantizeWeights(baseWeights[i*layers:(i+1)*layers], quant)
		copy(matWeights[i*layers:(i+1)*layers], q)
	}

	ratios := materialRatios(matWeights, vertexCount, layers, quant)
	indices := buildIndices(gfx, gfz)
	instances := buildInstances(p, bd)
	bounds := computeBounds(positions, instances)

	stats, seamErr := seamStatistics(p, blockID, params.Subdivisions, seams, params)
	if seamErr != nil {
		return nil, seamErr
	}

	return &Artifact{
		BlockID:         blockID,
		Positions:       positions,
		Normals:         normals,
		Indices:         indices,
		MaterialWeights: matWeights,
		MaterialRatios:  ratios,
		Instances:       instances,
		Bounds:          bounds,
		Seams:           stats,
	}, nil
}

func seamsForBlock(p *project.Project, blockID string) []project.Seam {
	var out []project.Seam
	for _, s := range p.Manifest.Seams {
		if s.A == blockID || s.B == blockID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func seamSide(s project.Seam, blockID string) (edge, otherID string, ok bool) {
	switch blockID {
	case s.A:
		return s.EdgeA, s.B, true
	case s.B:
		return s.EdgeB, s.A, true
	default:
		return "", "", false
	}
}

func blockWorldMax(def project.Block, spacing float64) (maxX, maxZ float64) {
	return def.Origin.X + float64(def.Grid.Cols-1)*spacing,
		def.Origin.Z + float64(def.Grid.Rows-1)*spacing
}

func blockContains(def project.Block, spacing, x, z float64) bool {
	maxX, maxZ := blockWorldMax(def, spacing)
	const eps = 1e-9
	return x >= def.Origin.X-eps && x <= maxX+eps && z >= def.Origin.Z-eps && z <= maxZ+eps
}

func localCoords(def project.Block, spacing, x, z float64) (u, v float64) {
	return (x - def.Origin.X) / spacing, (z - def.Origin.Z) / spacing
}

func bilinearCell(cols, rows int, u, v float64) (c0, r0 int, tx, tz float64) {
	if u < 0 {
		u = 0
	}
	if v < 0 {
		v = 0
	}
	if u > float64(cols-1) {
		u = float64(cols - 1)
	}
	if v > float64(rows-1) {
		v = float64(rows - 1)
	}
	c0 = int(math.Floor(u))
	r0 = int(math.Floor(v))
	if c0 >= cols-1 {
		c0 = cols - 2
	}
	if r0 >= rows-1 {
		r0 = rows - 2
	}
	tx = u - float64(c0)
	tz = v - float64(r0)
	return c0, r0, tx, tz
}

func bilinearHeight(values []float64, cols, rows int, u, v float64) float64 {
	c0, r0, tx, tz := bilinearCell(cols, rows, u, v)
	h00 := values[r0*cols+c0]
	h10 := values[r0*cols+c0+1]
	h01 := values[(r0+1)*cols+c0]
	h11 := values[(r0+1)*cols+c0+1]
	return h00*(1-tx)*(1-tz) + h10*tx*(1-tz) + h01*(1-tx)*tz + h11*tx*tz
}

func bilinearWeights(values []uint16, cols, rows int, u, v float64, stride int) []float64 {
	c0, r0, tx, tz := bilinearCell(cols, rows, u, v)
	out := make([]float64, stride)
	base00 := (r0*cols + c0) * stride
	base10 := (r0*cols + c0 + 1) * stride
	base01 := ((r0+1)*cols + c0) * stride
	base11 := ((r0+1)*cols + c0 + 1) * stride
	for k := 0; k < stride; k++ {
		a := float64(values[base00+k])
		b := float64(values[base10+k])
		c := float64(values[base01+k])
		d := float64(values[base11+k])
		out[k] = a*(1-tx)*(1-tz) + b*tx*(1-tz) + c*(1-tx)*tz + d*tx*tz
	}
	return out
}

func localHeightAtWorld(p *project.Project, bd *project.BlockData, x, z float64) float64 {
	u, v := localCoords(bd.Def, p.Manifest.Grid.Spacing, x, z)
	return bilinearHeight(bd.Height, bd.Def.Grid.Cols, bd.Def.Grid.Rows, u, v)
}

func fineHeightAt(p *project.Project, bd *project.BlockData, c, r, subdiv int) float64 {
	spacing := p.Manifest.Grid.Spacing
	x := bd.Def.Origin.X + float64(c)/float64(subdiv)*spacing
	z := bd.Def.Origin.Z + float64(r)/float64(subdiv)*spacing
	return localHeightAtWorld(p, bd, x, z)
}

func localMaterialWeights(p *project.Project, bd *project.BlockData, c, r, subdiv int) []float64 {
	spacing := p.Manifest.Grid.Spacing
	x := bd.Def.Origin.X + float64(c)/float64(subdiv)*spacing
	z := bd.Def.Origin.Z + float64(r)/float64(subdiv)*spacing
	u, v := localCoords(bd.Def, spacing, x, z)
	return bilinearWeights(bd.Weights, bd.Def.Grid.Cols, bd.Def.Grid.Rows, u, v, bd.Def.Material.Layers)
}

func globalHeightAtWorld(p *project.Project, x, z float64) (float64, bool) {
	sum := 0.0
	count := 0
	for _, id := range p.BlockOrder {
		bd := p.Blocks[id]
		if !blockContains(bd.Def, p.Manifest.Grid.Spacing, x, z) {
			continue
		}
		sum += localHeightAtWorld(p, bd, x, z)
		count++
	}
	if count == 0 {
		return 0, false
	}
	return sum / float64(count), true
}

func globalNormalAtWorld(p *project.Project, x, z, step float64) (Vec3, bool) {
	h, ok := globalHeightAtWorld(p, x, z)
	if !ok {
		return Vec3{}, false
	}
	hp, okp := globalHeightAtWorld(p, x+step, z)
	hm, okm := globalHeightAtWorld(p, x-step, z)
	var dhdx float64
	switch {
	case okp && okm:
		dhdx = (hp - hm) / (2 * step)
	case okm:
		dhdx = (h - hm) / step
	case okp:
		dhdx = (hp - h) / step
	}
	zp, okzp := globalHeightAtWorld(p, x, z+step)
	zm, okzm := globalHeightAtWorld(p, x, z-step)
	var dhdz float64
	switch {
	case okzp && okzm:
		dhdz = (zp - zm) / (2 * step)
	case okzm:
		dhdz = (h - zm) / step
	case okzp:
		dhdz = (zp - h) / step
	}
	return normalize(Vec3{X: -dhdx, Y: 1, Z: -dhdz}), true
}

func globalMaterialAtWorld(p *project.Project, x, z float64, layers int) ([]float64, bool) {
	out := make([]float64, layers)
	count := 0
	for _, id := range p.BlockOrder {
		bd := p.Blocks[id]
		if !blockContains(bd.Def, p.Manifest.Grid.Spacing, x, z) {
			continue
		}
		u, v := localCoords(bd.Def, p.Manifest.Grid.Spacing, x, z)
		raw := bilinearWeights(bd.Weights, bd.Def.Grid.Cols, bd.Def.Grid.Rows, u, v, layers)
		for k := range out {
			out[k] += raw[k]
		}
		count++
	}
	if count == 0 {
		return nil, false
	}
	for k := range out {
		out[k] /= float64(count)
	}
	return out, true
}

func localFineNormal(fine []float64, gfx, gfz, c, r int, step float64) Vec3 {
	idx := r*gfx + c
	var dhdx float64
	switch {
	case c > 0 && c < gfx-1:
		dhdx = (fine[idx+1] - fine[idx-1]) / (2 * step)
	case c == 0 && gfx > 1:
		dhdx = (fine[idx+1] - fine[idx]) / step
	case c == gfx-1 && gfx > 1:
		dhdx = (fine[idx] - fine[idx-1]) / step
	}
	var dhdz float64
	switch {
	case r > 0 && r < gfz-1:
		dhdz = (fine[idx+gfx] - fine[idx-gfx]) / (2 * step)
	case r == 0 && gfz > 1:
		dhdz = (fine[idx+gfx] - fine[idx]) / step
	case r == gfz-1 && gfz > 1:
		dhdz = (fine[idx] - fine[idx-gfx]) / step
	}
	return normalize(Vec3{X: -dhdx, Y: 1, Z: -dhdz})
}

func seamVertexPosition(edge string, j int, def project.Block, subdiv int) (c, r int) {
	gfx := (def.Grid.Cols-1)*subdiv + 1
	gfz := (def.Grid.Rows-1)*subdiv + 1
	switch edge {
	case "left":
		return 0, j
	case "right":
		return gfx - 1, j
	case "bottom":
		return j, 0
	case "top":
		return j, gfz - 1
	default:
		return 0, 0
	}
}

func seamSampleCount(edge string, def project.Block, subdiv int) int {
	if edge == "left" || edge == "right" {
		return (def.Grid.Rows-1)*subdiv + 1
	}
	return (def.Grid.Cols-1)*subdiv + 1
}

func seamVertexWorld(p *project.Project, def project.Block, edge string, subdiv, j int) (x, z float64) {
	c, r := seamVertexPosition(edge, j, def, subdiv)
	spacing := p.Manifest.Grid.Spacing
	return def.Origin.X + float64(c)/float64(subdiv)*spacing,
		def.Origin.Z + float64(r)/float64(subdiv)*spacing
}

func stitchSeamNormals(p *project.Project, bd *project.BlockData, params Params, gfx, gfz int, step float64, seams []project.Seam, normals []Vec3) {
	edgeSet := map[string]bool{}
	for _, s := range seams {
		edge, _, ok := seamSide(s, bd.Def.ID)
		if ok {
			edgeSet[edge] = true
		}
	}
	for r := 0; r < gfz; r++ {
		for c := 0; c < gfx; c++ {
			onSeam := (c == 0 && edgeSet["left"]) ||
				(c == gfx-1 && edgeSet["right"]) ||
				(r == 0 && edgeSet["bottom"]) ||
				(r == gfz-1 && edgeSet["top"])
			if !onSeam {
				continue
			}
			x := bd.Def.Origin.X + float64(c)/float64(params.Subdivisions)*p.Manifest.Grid.Spacing
			z := bd.Def.Origin.Z + float64(r)/float64(params.Subdivisions)*p.Manifest.Grid.Spacing
			if n, ok := globalNormalAtWorld(p, x, z, step); ok {
				normals[r*gfx+c] = n
			}
		}
	}
}

func stitchSeamMaterials(p *project.Project, bd *project.BlockData, params Params, gfx, gfz int, seams []project.Seam, weights []float64) {
	layers := bd.Def.Material.Layers
	band := int(math.Round(params.SeamMaterialBandCells * float64(params.Subdivisions)))
	if band < 0 {
		band = 0
	}
	for r := 0; r < gfz; r++ {
		for c := 0; c < gfx; c++ {
			targetSum := make([]float64, layers)
			totalFactor := 0.0
			for _, s := range seams {
				edge, _, ok := seamSide(s, bd.Def.ID)
				if !ok {
					continue
				}
				var dist int
				var px, pz float64
				x := bd.Def.Origin.X + float64(c)/float64(params.Subdivisions)*p.Manifest.Grid.Spacing
				z := bd.Def.Origin.Z + float64(r)/float64(params.Subdivisions)*p.Manifest.Grid.Spacing
				switch edge {
				case "left":
					dist = c
					px, pz = bd.Def.Origin.X, z
				case "right":
					dist = gfx - 1 - c
					px, pz = bd.Def.Origin.X+float64(bd.Def.Grid.Cols-1)*p.Manifest.Grid.Spacing, z
				case "bottom":
					dist = r
					px, pz = x, bd.Def.Origin.Z
				case "top":
					dist = gfz - 1 - r
					px, pz = x, bd.Def.Origin.Z+float64(bd.Def.Grid.Rows-1)*p.Manifest.Grid.Spacing
				}
				if dist > band {
					continue
				}
				raw, ok := globalMaterialAtWorld(p, px, pz, layers)
				if !ok {
					continue
				}
				factor := 1.0
				if band > 0 {
					factor = 1 - float64(dist)/float64(band+1)
				}
				if dist == 0 {
					factor = 1
				}
				for k := range targetSum {
					targetSum[k] += raw[k] * factor
				}
				totalFactor += factor
			}
			if totalFactor == 0 {
				continue
			}
			blend := math.Min(1, totalFactor)
			for k := 0; k < layers; k++ {
				target := targetSum[k] / totalFactor
				weights[(r*gfx+c)*layers+k] = weights[(r*gfx+c)*layers+k]*(1-blend) + target*blend
			}
		}
	}
}

func normalize(v Vec3) Vec3 {
	n := math.Sqrt(v.X*v.X + v.Y*v.Y + v.Z*v.Z)
	if n == 0 {
		return Vec3{Y: 1}
	}
	return Vec3{X: v.X / n, Y: v.Y / n, Z: v.Z / n}
}

func angleDeg(a, b Vec3) float64 {
	if a.X == b.X && a.Y == b.Y && a.Z == b.Z {
		return 0
	}
	d := a.X*b.X + a.Y*b.Y + a.Z*b.Z
	if d > 1 {
		d = 1
	}
	if d < -1 {
		d = -1
	}
	return math.Acos(d) * 180 / math.Pi
}

func quantizeWeights(raw []float64, quant int) []uint16 {
	s := 0.0
	for _, v := range raw {
		if v > 0 {
			s += v
		}
	}
	if s <= 0 {
		out := make([]uint16, len(raw))
		if len(out) > 0 {
			out[0] = uint16(quant)
		}
		return out
	}
	type rem struct {
		idx  int
		frac float64
	}
	out := make([]uint16, len(raw))
	rems := make([]rem, len(raw))
	allocated := 0
	for i, v := range raw {
		scaled := v / s * float64(quant)
		if scaled < 0 {
			scaled = 0
		}
		floor := math.Floor(scaled)
		out[i] = uint16(floor)
		rems[i] = rem{idx: i, frac: scaled - floor}
		allocated += int(floor)
	}
	left := quant - allocated
	sort.Slice(rems, func(i, j int) bool {
		if rems[i].frac == rems[j].frac {
			return rems[i].idx < rems[j].idx
		}
		return rems[i].frac > rems[j].frac
	})
	for i := 0; i < left && i < len(rems); i++ {
		out[rems[i].idx]++
	}
	return out
}

func materialRatios(weights []uint16, vertices, layers, quant int) []float64 {
	out := make([]float64, layers)
	if vertices == 0 || layers == 0 || quant == 0 {
		return out
	}
	for i, w := range weights {
		out[i%layers] += float64(w)
	}
	for i := range out {
		out[i] /= float64(vertices * quant)
	}
	return out
}

func buildIndices(gfx, gfz int) []uint32 {
	if gfx < 2 || gfz < 2 {
		return nil
	}
	out := make([]uint32, 0, (gfx-1)*(gfz-1)*6)
	for r := 0; r < gfz-1; r++ {
		for c := 0; c < gfx-1; c++ {
			v00 := uint32(r*gfx + c)
			v10 := v00 + 1
			v01 := v00 + uint32(gfx)
			v11 := v01 + 1
			out = append(out, v00, v01, v10, v10, v01, v11)
		}
	}
	return out
}

func buildInstances(p *project.Project, bd *project.BlockData) []Instance {
	sc := bd.Def.Scatter
	if sc.MaxInstances == 0 || bd.Def.Grid.Cols < 2 || bd.Def.Grid.Rows < 2 {
		return nil
	}
	spacing := p.Manifest.Grid.Spacing
	baseDensity := int(math.Floor(sc.DensityPerCell))
	frac := sc.DensityPerCell - float64(baseDensity)
	max := sc.MaxInstances
	out := make([]Instance, 0, minInt(max, 256))
	for r := 0; r < bd.Def.Grid.Rows-1 && len(out) < max; r++ {
		for c := 0; c < bd.Def.Grid.Cols-1 && len(out) < max; c++ {
			mask := bd.Mask[r*bd.Def.Grid.Cols+c]
			if int(mask) < sc.MinMask {
				continue
			}
			cell := uint64(r*(bd.Def.Grid.Cols-1) + c)
			candidates := baseDensity
			h := splitmix64(uint64(sc.Seed) ^ (cell+1)*hashSalt)
			if frac > 0 && unitFloat(h) < frac {
				candidates++
			}
			for k := 0; k < candidates && len(out) < max; k++ {
				h0 := splitmix64(uint64(sc.Seed) ^ (cell+1)*hashSalt ^ uint64(k+1)*0x94D049BB133111EB)
				h1 := splitmix64(h0)
				h2 := splitmix64(h1)
				h3 := splitmix64(h2)
				h4 := splitmix64(h3)
				jx := unitFloat(h0)
				jz := unitFloat(h1)
				x := bd.Def.Origin.X + (float64(c)+jx)*spacing
				z := bd.Def.Origin.Z + (float64(r)+jz)*spacing
				y := localHeightAtWorld(p, bd, x, z)
				out = append(out, Instance{
					Position: Vec3{X: x, Y: y, Z: z},
					Yaw:      2 * math.Pi * unitFloat(h2),
					Scale:    0.65 + 0.7*unitFloat(h3),
					Variant:  uint32(h4 % 1024),
				})
			}
		}
	}
	return out
}

func computeBounds(positions []Vec3, instances []Instance) Bounds {
	if len(positions) == 0 {
		return Bounds{}
	}
	min := Vec3{X: math.Inf(1), Y: math.Inf(1), Z: math.Inf(1)}
	max := Vec3{X: math.Inf(-1), Y: math.Inf(-1), Z: math.Inf(-1)}
	expand := func(v Vec3) {
		if v.X < min.X {
			min.X = v.X
		}
		if v.Y < min.Y {
			min.Y = v.Y
		}
		if v.Z < min.Z {
			min.Z = v.Z
		}
		if v.X > max.X {
			max.X = v.X
		}
		if v.Y > max.Y {
			max.Y = v.Y
		}
		if v.Z > max.Z {
			max.Z = v.Z
		}
	}
	for _, v := range positions {
		expand(v)
	}
	for _, inst := range instances {
		expand(inst.Position)
	}
	return Bounds{Min: min, Max: max}
}

func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	z := x
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func unitFloat(x uint64) float64 {
	return float64(x>>11) / float64(uint64(1)<<53)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func seamStatistics(p *project.Project, blockID string, subdiv int, seams []project.Seam, params Params) (SeamStats, *project.Error) {
	stitched := params.SeamMaxIterations > 0
	used := 0
	if stitched {
		used = minInt(1, params.SeamMaxIterations)
	}
	var stats SeamStats
	for _, s := range seams {
		edgeA, _, ok := seamSide(s, s.A)
		edgeB, _, ok2 := seamSide(s, s.B)
		if !ok || !ok2 {
			continue
		}
		ad, bd := p.Blocks[s.A], p.Blocks[s.B]
		samplesA := seamSampleCount(edgeA, ad.Def, subdiv)
		samplesB := seamSampleCount(edgeB, bd.Def, subdiv)
		if samplesA != samplesB {
			return stats, project.NewError(project.CodeSize, blockID, "seam.samples", "seam %s sample counts differ", s.ID)
		}
		stat := SeamStat{SeamID: s.ID, A: s.A, B: s.B, EdgeA: edgeA, EdgeB: edgeB, Samples: samplesA, IterationsUsed: used}
		for j := 0; j < samplesA; j++ {
			nA := boundaryNormal(p, ad, edgeA, j, subdiv, params, stitched)
			nB := boundaryNormal(p, bd, edgeB, j, subdiv, params, stitched)
			mA := boundaryWeights(p, ad, edgeA, j, subdiv, params, stitched)
			mB := boundaryWeights(p, bd, edgeB, j, subdiv, params, stitched)
			angle := angleDeg(nA, nB)
			if angle > stat.MaxAngleDeg {
				stat.MaxAngleDeg = angle
			}
			diff := 0
			for k := 0; k < len(mA) && k < len(mB); k++ {
				d := int(mA[k]) - int(mB[k])
				if d < 0 {
					d = -d
				}
				if d > diff {
					diff = d
				}
			}
			if diff > stat.MaxMaterialDiff {
				stat.MaxMaterialDiff = diff
			}
			badAngle := angle > params.SeamAngleThresholdDeg+1e-12
			if badAngle || diff > 0 {
				stat.Violations = 1
			}
		}
		if stat.MaxAngleDeg > stats.MaxNormalAngleDeg {
			stats.MaxNormalAngleDeg = stat.MaxAngleDeg
		}
		if stat.MaxMaterialDiff > stats.MaxMaterialDiff {
			stats.MaxMaterialDiff = stat.MaxMaterialDiff
		}
		stats.ConstraintViolations += stat.Violations
		stats.PerSeam = append(stats.PerSeam, stat)
	}
	if stats.ConstraintViolations > 0 {
		var worst SeamStat
		for _, s := range stats.PerSeam {
			if s.Violations > 0 && (s.MaxAngleDeg >= worst.MaxAngleDeg) {
				worst = s
			}
		}
		return stats, project.NewError(project.CodeSeam, blockID, "seam:"+worst.SeamID,
			"seam %s between %s and %s violates constraints: angle %.9g deg (threshold %.9g), material delta %d",
			worst.SeamID, worst.A, worst.B, worst.MaxAngleDeg, params.SeamAngleThresholdDeg, worst.MaxMaterialDiff).
			WithDetail("a", worst.A).WithDetail("b", worst.B).
			WithDetail("seamId", worst.SeamID).
			WithDetail("angleDeg", worst.MaxAngleDeg).
			WithDetail("thresholdDeg", params.SeamAngleThresholdDeg).
			WithDetail("materialDiff", worst.MaxMaterialDiff)
	}
	return stats, nil
}

func boundaryNormal(p *project.Project, bd *project.BlockData, edge string, j, subdiv int, params Params, stitched bool) Vec3 {
	c, r := seamVertexPosition(edge, j, bd.Def, subdiv)
	if stitched {
		x, z := seamVertexWorld(p, bd.Def, edge, subdiv, j)
		if n, ok := globalNormalAtWorld(p, x, z, p.Manifest.Grid.Spacing/float64(subdiv)); ok {
			return n
		}
	}
	gfx := (bd.Def.Grid.Cols-1)*subdiv + 1
	gfz := (bd.Def.Grid.Rows-1)*subdiv + 1
	// Build only the nearby fine-height samples needed for the differential.
	fine := make([]float64, gfx*gfz)
	for rr := 0; rr < gfz; rr++ {
		for cc := 0; cc < gfx; cc++ {
			fine[rr*gfx+cc] = fineHeightAt(p, bd, cc, rr, subdiv)
		}
	}
	return localFineNormal(fine, gfx, gfz, c, r, p.Manifest.Grid.Spacing/float64(subdiv))
}

func boundaryWeights(p *project.Project, bd *project.BlockData, edge string, j, subdiv int, params Params, stitched bool) []uint16 {
	layers := bd.Def.Material.Layers
	if stitched {
		x, z := seamVertexWorld(p, bd.Def, edge, subdiv, j)
		if raw, ok := globalMaterialAtWorld(p, x, z, layers); ok {
			return quantizeWeights(raw, params.QuantMax)
		}
	}
	c, r := seamVertexPosition(edge, j, bd.Def, subdiv)
	raw := localMaterialWeights(p, bd, c, r, subdiv)
	return quantizeWeights(raw, params.QuantMax)
}

// ValidateTruth compares baked artifacts to every declared analytic truth
// sample in the project.
func ValidateTruth(p *project.Project, artifacts map[string]*Artifact) TruthReport {
	var report TruthReport
	addBad := func(m TruthMismatch) {
		report.Mismatches = append(report.Mismatches, m)
	}
	artOf := func(id string) *Artifact { return artifacts[id] }
	for _, t := range p.Truth.HeightSamples {
		report.Checks++
		a := artOf(t.Block)
		if a == nil {
			addBad(TruthMismatch{Block: t.Block, Kind: "height", Expected: t.Value, Tolerance: t.Tolerance})
			continue
		}
		gfx := (p.Blocks[t.Block].Def.Grid.Cols-1)*p.Manifest.Bake.Subdivisions + 1
		idx := (t.Row*p.Manifest.Bake.Subdivisions)*gfx + t.Col*p.Manifest.Bake.Subdivisions
		actual := a.Positions[idx].Y
		if math.Abs(actual-t.Value) > t.Tolerance {
			addBad(TruthMismatch{Block: t.Block, Kind: "height", Expected: t.Value, Actual: actual, Tolerance: t.Tolerance})
		}
	}
	for _, t := range p.Truth.MaterialRatios {
		report.Checks++
		a := artOf(t.Block)
		if a == nil || t.Layer < 0 || t.Layer >= len(a.MaterialRatios) {
			addBad(TruthMismatch{Block: t.Block, Kind: "materialRatio", Expected: t.Ratio, Tolerance: t.Tolerance, Layer: t.Layer})
			continue
		}
		actual := a.MaterialRatios[t.Layer]
		if math.Abs(actual-t.Ratio) > t.Tolerance {
			addBad(TruthMismatch{Block: t.Block, Kind: "materialRatio", Expected: t.Ratio, Actual: actual, Tolerance: t.Tolerance, Layer: t.Layer})
		}
	}
	for _, t := range p.Truth.InstanceCounts {
		report.Checks++
		a := artOf(t.Block)
		if a == nil {
			addBad(TruthMismatch{Block: t.Block, Kind: "instanceCount", Expected: float64(t.Count), Tolerance: float64(t.Tolerance)})
			continue
		}
		actual := float64(len(a.Instances))
		if math.Abs(actual-float64(t.Count)) > float64(t.Tolerance) {
			addBad(TruthMismatch{Block: t.Block, Kind: "instanceCount", Expected: float64(t.Count), Actual: actual, Tolerance: float64(t.Tolerance)})
		}
	}
	for _, t := range p.Truth.Seams {
		report.Checks++
		a := artOf(t.A)
		if a == nil {
			a = artOf(t.B)
		}
		if a == nil {
			addBad(TruthMismatch{Block: t.A, Kind: "seamAngle", Expected: t.MaxAngleDeg, Tolerance: t.Tolerance})
			continue
		}
		found := false
		for _, s := range a.Seams.PerSeam {
			if (s.A == t.A && s.B == t.B) || (s.A == t.B && s.B == t.A) {
				found = true
				if math.Abs(s.MaxAngleDeg-t.MaxAngleDeg) > t.Tolerance {
					addBad(TruthMismatch{Block: t.A, Kind: "seamAngle", Expected: t.MaxAngleDeg, Actual: s.MaxAngleDeg, Tolerance: t.Tolerance})
				}
				break
			}
		}
		if !found {
			addBad(TruthMismatch{Block: t.A, Kind: "seamAngle", Expected: t.MaxAngleDeg, Tolerance: t.Tolerance})
		}
	}
	report.OK = len(report.Mismatches) == 0
	return report
}
