package pipeline

type InvalidTile struct {
	TileID        int      `json:"tile_id"`
	X             int      `json:"x"`
	Y             int      `json:"y"`
	Patches       []string `json:"patches"`
	Reasons       []string `json:"reasons"`
	FlippedProbes []string `json:"flipped_probes,omitempty"`
}

type InvalidationPlan struct {
	Tiles            []InvalidTile `json:"tiles"`
	TileIDs          []int         `json:"tile_ids"`
	AffectedPatches  []string      `json:"affected_patches"`
	TopologicalOrder []string      `json:"topological_order"`
	Recomputed       []int         `json:"recomputed,omitempty"`
	Reused           []int         `json:"reused,omitempty"`
}

func (p *Pipeline) newPlan() *InvalidationPlan {
	return &InvalidationPlan{}
}

func (plan *InvalidationPlan) add(p *Pipeline, tileID int, patchID, reason string, probes ...string) {
	for i := range plan.Tiles {
		if plan.Tiles[i].TileID == tileID {
			plan.Tiles[i].Patches = appendUnique(plan.Tiles[i].Patches, patchID)
			plan.Tiles[i].Reasons = appendUnique(plan.Tiles[i].Reasons, reason)
			for _, probe := range probes {
				plan.Tiles[i].FlippedProbes = appendUnique(plan.Tiles[i].FlippedProbes, probe)
			}
			return
		}
	}
	x, y := p.Camera.TileXY(tileID)
	plan.Tiles = append(plan.Tiles, InvalidTile{
		TileID: tileID, X: x, Y: y, Patches: []string{patchID},
		Reasons: []string{reason}, FlippedProbes: append([]string{}, probes...),
	})
}

func (plan *InvalidationPlan) finalize(p *Pipeline, patchIDs ...string) *InvalidationPlan {
	idSet := make(map[int]bool)
	for _, t := range plan.Tiles {
		idSet[t.TileID] = true
	}
	for id := range idSet {
		plan.TileIDs = append(plan.TileIDs, id)
	}
	sortInts(plan.TileIDs)
	for _, id := range patchIDs {
		plan.AffectedPatches = appendUnique(plan.AffectedPatches, id)
	}
	for _, tile := range plan.Tiles {
		for _, id := range tile.Patches {
			plan.AffectedPatches = appendUnique(plan.AffectedPatches, id)
		}
	}
	if len(plan.TileIDs) > 0 {
		plan.TopologicalOrder = BuildTopology(plan.TileIDs, plan.AffectedPatches)
	}
	return plan
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}
