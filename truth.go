package fogkernel

import "math"

const (
	StopNoIntersection = "no_intersection"
	StopCompleted      = "completed"
	StopThreshold      = "transmittance_threshold"
	StopBlockLimit     = "block_sample_limit"
	StopSignal         = "signal_checkpoint"
)

type BlockTruth struct {
	BlockIndex int      `json:"block_index"`
	ChunkBin   int64    `json:"chunk_bin"`
	T0         float64  `json:"t0"`
	T1         float64  `json:"t1"`
	TauA       float64  `json:"tau_a"`
	TauS       float64  `json:"tau_s"`
	Tau        float64  `json:"tau"`
	TBlock     float64  `json:"t_block"`
	TIn        float64  `json:"t_in"`
	TOut       float64  `json:"t_out"`
	Energy     float64  `json:"energy"`
	Terminal   bool     `json:"terminal"`
	VolumeIDs  []string `json:"volume_ids"`
}

type RayTruth struct {
	RayIndex      int          `json:"ray_index"`
	RayID         string       `json:"ray_id"`
	CameraIndex   int          `json:"camera_index"`
	Hit           bool         `json:"hit"`
	Transmittance float64      `json:"transmittance"`
	TerminalBlock int          `json:"terminal_block"`
	StopReason    string       `json:"stop_reason"`
	Blocks        []BlockTruth `json:"blocks"`
}

type TruthSet struct {
	Seed         int64      `json:"seed"`
	SceneVersion string     `json:"scene_version"`
	Rays         []RayTruth `json:"rays"`
}

// BuildTruth is the generator's analytic reference construction. It is called
// only while producing the deterministic bundle; verification compares kernel
// output against these recorded values.
func BuildTruth(scene *Scene) *TruthSet {
	set := &TruthSet{Seed: scene.Seed, SceneVersion: scene.Version()}
	threshold := scene.Recipe.TerminationThreshold()
	for _, ray := range scene.Rays {
		plan := PlanRay(scene, ray)
		rt := RayTruth{RayIndex: ray.Index, RayID: ray.ID, CameraIndex: ray.CameraIndex, TerminalBlock: -1}
		if !plan.Hit || len(plan.Blocks) == 0 {
			rt.Hit = false
			rt.Transmittance = 1
			rt.StopReason = StopNoIntersection
			set.Rays = append(set.Rays, rt)
			continue
		}
		rt.Hit = true
		t := 1.0
		limit := scene.Recipe.BlockSampleLimit
		for _, bp := range plan.Blocks {
			if len(rt.Blocks) >= limit {
				rt.StopReason = StopBlockLimit
				break
			}
			ph := ComputeBlockPhysics(scene, ray, bp)
			tBlock := expNeg(ph.Tau)
			energy := blockEnergy(t, ph)
			tOut := t * tBlock
			bt := BlockTruth{
				BlockIndex: bp.BlockIndex, ChunkBin: bp.Bin, T0: bp.T0, T1: bp.T1,
				TauA: ph.TauA, TauS: ph.TauS, Tau: ph.Tau, TBlock: tBlock,
				TIn: t, TOut: tOut, Energy: energy, VolumeIDs: ph.VolumeIDs,
			}
			t = tOut
			rt.Transmittance = t
			rt.TerminalBlock = bp.BlockIndex
			rt.Blocks = append(rt.Blocks, bt)
			if t < threshold {
				rt.StopReason = StopThreshold
				bt.Terminal = true
				rt.Blocks[len(rt.Blocks)-1].Terminal = true
				break
			}
		}
		if rt.StopReason == "" {
			if len(plan.Blocks) > limit && len(rt.Blocks) == limit {
				rt.StopReason = StopBlockLimit
			} else {
				rt.StopReason = StopCompleted
			}
		}
		set.Rays = append(set.Rays, rt)
	}
	return set
}

func expNeg(x float64) float64 {
	return math.Exp(-x)
}
