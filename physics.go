package fogkernel

import "math"

// BlockPhysics contains the schema/path-independent optical quantities used by
// exact, preview reconstruction, diagnostics and the generator's truth writer.
type BlockPhysics struct {
	TauA         float64  `json:"tau_a"`
	TauS         float64  `json:"tau_s"`
	Tau          float64  `json:"tau"`
	Albedo       float64  `json:"albedo"`
	Phase        float64  `json:"phase"`
	SegmentCount int      `json:"segment_count"`
	VolumeIDs    []string `json:"volume_ids"`
}

func mediumByID(scene *Scene, id string) *Medium {
	for _, m := range scene.Media {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func ComputeBlockPhysics(scene *Scene, ray Ray, block BlockPlan) BlockPhysics {
	ph := BlockPhysics{SegmentCount: len(block.Segments), VolumeIDs: append([]string(nil), block.VolumeIDs...)}
	phaseWeighted := 0.0
	for _, seg := range block.Segments {
		m := mediumByID(scene, seg.Ref.VolumeID)
		if m == nil {
			continue
		}
		densityValue := seg.Ref.Density
		if seg.Ref.X >= 0 && seg.Ref.X < int(m.Density.Size.X) &&
			seg.Ref.Y >= 0 && seg.Ref.Y < int(m.Density.Size.Y) &&
			seg.Ref.Z >= 0 && seg.Ref.Z < int(m.Density.Size.Z) {
			densityValue = m.Density.Get(seg.Ref.X, seg.Ref.Y, seg.Ref.Z)
		}
		densityFactor := float64(densityValue) / float64(m.Density.Max)
		length := seg.Length
		tauA := m.SigmaA() * densityFactor * length
		tauS := m.SigmaS() * densityFactor * length
		ph.TauA += tauA
		ph.TauS += tauS
		cosTheta := ray.Direction.Dot(scene.LightDir)
		if cosTheta > 1 {
			cosTheta = 1
		} else if cosTheta < -1 {
			cosTheta = -1
		}
		phaseWeighted += tauS * HGPhase(m.G(), cosTheta)
	}
	ph.Tau = ph.TauA + ph.TauS
	if ph.Tau > 0 {
		ph.Albedo = ph.TauS / ph.Tau
	}
	if ph.TauS > 0 {
		ph.Phase = phaseWeighted / ph.TauS
	} else {
		ph.Phase = HGPhase(0, ray.Direction.Dot(scene.LightDir))
	}
	return ph
}

func blockEnergy(tIn float64, ph BlockPhysics) float64 {
	extinguished := -math.Expm1(-ph.Tau)
	return tIn * extinguished * ph.Albedo * ph.Phase
}
