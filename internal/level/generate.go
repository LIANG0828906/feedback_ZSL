package level

import "fmt"

const heightLattice = 8

// GeneratePartition deterministically builds one partition from its
// constraint and the declared sub-seed derivation rule. The result is a
// pure function of (global seed, sub-seed version, partition constraint,
// density); it never depends on scheduling, thread identity or time.
func GeneratePartition(proj *Project, pc *PartitionConstraint, density int) (*PartitionResult, error) {
	seed := DeriveSubSeed(proj.GlobalSeed, pc.Row, pc.Col, pc.SubSeedSalt, proj.SubSeedVersion)
	rng := NewRNG(seed)
	res := &PartitionResult{PartitionID: pc.ID, Density: density}

	lattice := make([]int32, (heightLattice+1)*(heightLattice+1))
	for i := range lattice {
		lattice[i] = int32(rng.Intn(1001))
	}
	res.Heights = make([]int32, density*density)
	for y := 0; y < density; y++ {
		for x := 0; x < density; x++ {
			res.Heights[y*density+x] = sampleHeight(lattice, x, y, density)
		}
	}

	total := pc.ObstacleQuota * 2
	candidates := make([]Obstacle, 0, total)
	cells := RefDensity * RefDensity
	for i := 0; i < total; i++ {
		cost := int64(200 + rng.Intn(800))
		cell := rng.Intn(cells)
		candidates = append(candidates, Obstacle{
			ID:   fmt.Sprintf("p%d-ob%d", pc.ID, i),
			Cell: cell,
			Cost: cost,
		})
	}
	res.Obstacles = append([]Obstacle(nil), candidates[:pc.ObstacleQuota]...)
	res.Reserve = append([]Obstacle(nil), candidates[pc.ObstacleQuota:]...)

	desired := 0
	if pc.ResourceCap > 0 {
		desired = pc.ResourceCap/2 + rng.Intn(pc.ResourceCap-pc.ResourceCap/2+1)
	}
	for i := 0; i < desired; i++ {
		res.Resources = append(res.Resources, Resource{
			ID:       fmt.Sprintf("p%d-rs%d", pc.ID, i),
			Cell:     rng.Intn(cells),
			Category: rng.Intn(3),
		})
	}

	for k := 0; k < 4; k++ {
		res.Edges[k] = rng.Intn(100) < proj.EdgeOpenPct
	}

	recomputePassable(res)
	if err := enforceBudget(proj, pc, res); err != nil {
		return res, err
	}
	return res, nil
}

func sampleHeight(lattice []int32, x, y, density int) int32 {
	fx := float64(x) * float64(heightLattice) / float64(density)
	fy := float64(y) * float64(heightLattice) / float64(density)
	x0 := int(fx)
	y0 := int(fy)
	if x0 >= heightLattice {
		x0 = heightLattice - 1
	}
	if y0 >= heightLattice {
		y0 = heightLattice - 1
	}
	tx := fx - float64(x0)
	ty := fy - float64(y0)
	stride := heightLattice + 1
	v00 := float64(lattice[y0*stride+x0])
	v10 := float64(lattice[y0*stride+x0+1])
	v01 := float64(lattice[(y0+1)*stride+x0])
	v11 := float64(lattice[(y0+1)*stride+x0+1])
	h := v00*(1-tx)*(1-ty) + v10*tx*(1-ty) + v01*(1-tx)*ty + v11*tx*ty
	return int32(h + 0.5)
}

func recomputePassable(res *PartitionResult) {
	blocked := map[int]bool{}
	for _, ob := range res.Obstacles {
		blocked[ob.Cell] = true
	}
	cells := RefDensity * RefDensity
	res.PassablePct = 100 - len(blocked)*100/cells
}

func DifficultyOf(res *PartitionResult) int64 {
	d := int64(len(res.Resources)) * ResourceDifficulty
	for _, ob := range res.Obstacles {
		d += ob.Cost
	}
	return d
}
