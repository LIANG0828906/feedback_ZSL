package fog

import (
	"fmt"
	"sort"
)

type PixelTruth struct {
	PixelX                int     `json:"pixel_x"`
	PixelY                int     `json:"pixel_y"`
	BlockID               int     `json:"block_id"`
	RayID                 string  `json:"ray_id"`
	CameraID              string  `json:"camera_id"`
	MediumID              string  `json:"medium_id"`
	ExpectedTransmittance float64 `json:"expected_transmittance"`
	ExpectedRadiance      float64 `json:"expected_radiance"`
	DensityIntegral       float64 `json:"density_integral"`
	ToleranceT            float64 `json:"tolerance_t"`
	ToleranceL            float64 `json:"tolerance_l"`
}

type DensityIntegralTruth struct {
	RayID         string  `json:"ray_id"`
	PixelX        int     `json:"pixel_x"`
	PixelY        int     `json:"pixel_y"`
	BlockID       int     `json:"block_id"`
	MediumID      string  `json:"medium_id"`
	TMinQ         int64   `json:"t_min_q"`
	TMaxQ         int64   `json:"t_max_q"`
	ExpectedValue float64 `json:"expected_value"`
}

type InvalidationTruth struct {
	Edit           DensityEdit `json:"edit"`
	ExpectedBlocks []int       `json:"expected_blocks"`
	Reason         string      `json:"reason"`
}

type TruthBook struct {
	Seed                int64                  `json:"seed"`
	Pixels              []PixelTruth           `json:"pixels"`
	DensityIntegrals    []DensityIntegralTruth `json:"density_integrals"`
	DensityInvalidation InvalidationTruth      `json:"density_invalidation"`
}

func buildTruth(s *Scenario) error {
	vp := NewViewport(s.Width, s.Height, s.Recipe.TileSize)
	cam := s.ActiveCamera()
	truth := TruthBook{Seed: s.Seed}
	pixelCandidates := []PixelTruth{}

	chosen := map[string]int{}
	for y := 0; y < s.Height; y++ {
		for x := 0; x < s.Width; x++ {
			block := vp.PixelTile(x, y)
			ray := cam.RayForPixel(x, y, s.Width, s.Height, block)
			tr := TraceRay(ray, s.Media, s.FixedScale)

			if len(tr.Intervals) == 1 {
				mediumID := tr.Intervals[0].MediumID
				m, _ := s.Medium(mediumID)
				if m != nil && mediumID == "fog-uniform-left" {
					wantT, wantL, di, err := AnalyticRay(s, ray)
					if err != nil {
						return err
					}
					got, intErr := NewIntegrator(s, ModeFull).Integrate(tr)
					if intErr != nil {
						return intErr
					}
					stable := shadowFreeForAllBudgets(s, ray, tr) &&
						absFloat(got.Transmittance-wantT) <= 1e-10 &&
						absFloat(got.Radiance-wantL) <= 1e-10
					if stable {
						pixelCandidates = append(pixelCandidates, PixelTruth{
							PixelX: x, PixelY: y, BlockID: block, RayID: ray.ID,
							CameraID: cam.ID, MediumID: mediumID,
							ExpectedTransmittance: wantT, ExpectedRadiance: wantL,
							DensityIntegral: di,
							ToleranceT:      s.Recipe.TransmittanceTol,
							ToleranceL:      s.Recipe.RadianceTol,
						})
					}
				}
			}

			for _, iv := range tr.Intervals {
				countKey := "di:" + iv.MediumID
				if chosen[countKey] >= 3 {
					continue
				}
				m, ok := s.Medium(iv.MediumID)
				if !ok {
					continue
				}
				tMin := FromFixed(iv.TMinQ, s.FixedScale)
				tMax := FromFixed(iv.TMaxQ, s.FixedScale)
				value, _ := IntegrateDensity(m, ray, tMin, tMax)
				truth.DensityIntegrals = append(truth.DensityIntegrals, DensityIntegralTruth{
					RayID: ray.ID, PixelX: x, PixelY: y, BlockID: block,
					MediumID: iv.MediumID, TMinQ: iv.TMinQ, TMaxQ: iv.TMaxQ,
					ExpectedValue: value,
				})
				chosen[countKey]++
			}
		}
	}
	sort.Slice(pixelCandidates, func(i, j int) bool {
		if pixelCandidates[i].BlockID != pixelCandidates[j].BlockID {
			return pixelCandidates[i].BlockID < pixelCandidates[j].BlockID
		}
		return pixelCandidates[i].DensityIntegral > pixelCandidates[j].DensityIntegral
	})
	perBlock := map[int]int{}
	for _, candidate := range pixelCandidates {
		if perBlock[candidate.BlockID] >= 2 {
			continue
		}
		truth.Pixels = append(truth.Pixels, candidate)
		perBlock[candidate.BlockID]++
		if len(truth.Pixels) >= 8 {
			break
		}
	}
	if len(truth.Pixels) < 4 {
		return fmt.Errorf("truth generator selected only %d stable pixels", len(truth.Pixels))
	}

	blocks, err := ExpectedBlocksForVoxelEdit(s, s.DeclaredEdit)
	if err != nil {
		return err
	}
	truth.DensityInvalidation = InvalidationTruth{
		Edit: s.DeclaredEdit, ExpectedBlocks: blocks,
		Reason: "rays traverse the edited integer voxel",
	}
	s.Truth = truth
	return nil
}

func numericalMatchesTruth(s *Scenario, tr TraceResult, wantT, wantL float64) bool {
	integ := NewIntegrator(s, ModeFull)
	got, err := integ.Integrate(tr)
	if err != nil {
		return false
	}
	return absFloat(got.Transmittance-wantT) <= s.Recipe.TransmittanceTol &&
		absFloat(got.Radiance-wantL) <= s.Recipe.RadianceTol
}

func shadowFreeForAllBudgets(s *Scenario, ray Ray, tr TraceResult) bool {
	budgets := []int{2, 3, 4, 6, 8, 12, 18, 32}
	shadow := DeterministicShadow(s.Media, s.Light)
	for _, steps := range budgets {
		for _, span := range tr.Spans {
			if span.Kind != SpanMedium {
				continue
			}
			for k := 0; k < steps; k++ {
				t := span.TMin + (float64(k)+0.5)*(span.TMax-span.TMin)/float64(steps)
				p := ray.Point(t)
				for sample := 0; sample < s.Recipe.ShadowSamples; sample++ {
					if shadow(ray, p, k, sample, span.MediumIDs) < 0.999 {
						return false
					}
				}
			}
		}
	}
	return true
}

// ExpectedBlocksForVoxelEdit is an independent AABB oracle used by generated
// truth. Dependency-graph tests compare their voxel key propagation to this.
func ExpectedBlocksForVoxelEdit(s *Scenario, edit DensityEdit) ([]int, error) {
	m, ok := s.Medium(edit.MediumID)
	if !ok {
		return nil, &Error{Code: EDens, MediaID: edit.MediumID, Field: "medium_id", Message: "unknown medium"}
	}
	if !m.Field.InBounds(edit.X, edit.Y, edit.Z) {
		return nil, &Error{Code: EDens, MediaID: m.ID, Field: "voxel", Message: "voxel out of bounds"}
	}
	box := m.Field.VoxelAABB(m.Bounds, edit.X, edit.Y, edit.Z)
	vp := NewViewport(s.Width, s.Height, s.Recipe.TileSize)
	cam := s.ActiveCamera()
	set := map[int]struct{}{}
	for ty := 0; ty < vp.TilesY; ty++ {
		for tx := 0; tx < vp.TilesX; tx++ {
			tile := vp.Tiles[ty*vp.TilesX+tx]
			hit := false
			for y := tile.Y0; y < tile.Y0+tile.Height && !hit; y++ {
				for x := tile.X0; x < tile.X0+tile.Width; x++ {
					ray := cam.RayForPixel(x, y, s.Width, s.Height, tile.ID)
					t0, t1, ok := box.IntersectRay(ray.Origin, ray.Dir)
					if ok && t1 >= ray.Near && t0 <= ray.Far {
						hit = true
						break
					}
				}
			}
			if hit {
				set[tile.ID] = struct{}{}
			}
		}
	}
	out := make([]int, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Ints(out)
	return out, nil
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
