package sample

import (
	"encoding/json"
	"os"
	"path/filepath"

	"combatjudge/internal/model"
	"combatjudge/internal/schema"
)

const Seed int64 = 428691

func U(v int64) int64 { return v * model.InternalScale }

func BuildProject() *model.Project {
	colliders := []model.Collider{
		{Index: 1, ID: "box_front", Kind: model.KindAABB, Part: "torso", Min: model.Vec3{X: U(3), Y: -U(1), Z: -U(1)}, Max: model.Vec3{X: U(4), Y: U(1), Z: U(1)}},
		{Index: 2, ID: "capsule_pylon", Kind: model.KindCapsule, Part: "pylon_shaft", P0: model.Vec3{X: 7_500_000, Y: -1_500_000}, P1: model.Vec3{X: 7_500_000, Y: 1_500_000}, Radius: 500_000},
		{Index: 3, ID: "box_rear", Kind: model.KindAABB, Part: "rear_core", Min: model.Vec3{X: U(9), Y: -U(1), Z: -U(1)}, Max: model.Vec3{X: U(10), Y: U(1), Z: U(1)}},
	}

	for i := 0; i < 11; i++ {
		centerZ := U(21 + int64(i))
		colliders = append(colliders, model.Collider{
			Index: 4 + i,
			ID:    "exhaust_gate_" + string(rune('a'+i)),
			Kind:  model.KindAABB,
			Part:  "gate_body",
			Min:   model.Vec3{X: -U(1), Y: -U(1), Z: centerZ - 100_000},
			Max:   model.Vec3{X: U(1), Y: U(1), Z: centerZ + 100_000},
		})
	}

	sweeps := []model.Sweep{
		{
			Index: 1, ID: "sw_primary", From: model.Vec3{X: 0, Y: 0, Z: 0}, To: model.Vec3{X: U(10)},
			Radius: 500_000, StartFrame: 1, EndFrame: 6, StepMS: 16, DamageQ: 40 * model.InternalScale,
			Penetration: true, DecayQ: 500_000, MultiHit: true, MaxSegments: 12,
			RepeatPolicy: model.RepeatOncePerSweep, EndpointMode: "absolute",
		},
		{
			Index: 2, ID: "sw_exhaust", From: model.Vec3{Z: U(20)}, To: model.Vec3{Z: U(32)},
			Radius: 250_000, StartFrame: 1, EndFrame: 7, StepMS: 16, DamageQ: 1000,
			Penetration: true, DecayQ: 500_000, MultiHit: true, MaxSegments: 12,
			RepeatPolicy: model.RepeatOncePerSweep, EndpointMode: "absolute",
		},
	}

	requests := []model.FrameRequest{}
	for f := 1; f <= 6; f++ {
		requests = append(requests, model.FrameRequest{Index: len(requests) + 1, Frame: f, SweepID: "sw_primary", AtMS: int64(f-1) * 16})
	}
	for f := 1; f <= 7; f++ {
		requests = append(requests, model.FrameRequest{Index: len(requests) + 1, Frame: f, SweepID: "sw_exhaust", AtMS: int64(f-1) * 16})
	}

	return &model.Project{SchemaVersion: model.SchemaV3, BattleID: "deterministic-battle-001", StepMS: 16, Colliders: colliders, Sweeps: sweeps, Requests: requests}
}

func BuildTruth() *model.TruthFile {
	truth := &model.TruthFile{
		Schema: "truth-v1", BattleID: "deterministic-battle-001", ToleranceQ: 5_000, GeneratorSeed: Seed,
		Samples: []model.TruthSample{
			{Frame: 2, SweepID: "sw_primary", Hit: false, RemainingDamageQ: 40_000_000, NearestDistanceQ: 500_000},
			{Frame: 3, SweepID: "sw_primary", Hit: true, FirstColliderNo: 1, FirstPart: "torso", FirstSurface: "aabb:face:-X", RemainingDamageQ: 20_000_000, NearestDistanceQ: 0},
			{Frame: 5, SweepID: "sw_primary", Hit: true, FirstColliderNo: 2, FirstPart: "pylon_shaft", FirstSurface: "capsule:shaft", RemainingDamageQ: 10_000_000, NearestDistanceQ: 0},
			{Frame: 6, SweepID: "sw_primary", Hit: true, FirstColliderNo: 3, FirstPart: "rear_core", FirstSurface: "aabb:face:-X", RemainingDamageQ: 5_000_000, NearestDistanceQ: 0},
		},
		Sequences: []model.TruthSequence{
			{SweepID: "sw_primary", Segments: []model.TruthHit{
				{Segment: 1, Status: "hit", ColliderNo: 1, ColliderID: "box_front", Part: "torso", Surface: "aabb:face:-X", RemainingDamageQ: 20_000_000, NearestDistanceQ: 0},
				{Segment: 2, Status: "hit", ColliderNo: 2, ColliderID: "capsule_pylon", Part: "pylon_shaft", Surface: "capsule:shaft", RemainingDamageQ: 10_000_000, NearestDistanceQ: 0},
				{Segment: 3, Status: "hit", ColliderNo: 3, ColliderID: "box_rear", Part: "rear_core", Surface: "aabb:face:-X", RemainingDamageQ: 5_000_000, NearestDistanceQ: 0},
			}},
			{SweepID: "sw_exhaust", Segments: exhaustTruth()},
		},
	}
	return truth
}

func exhaustTruth() []model.TruthHit {
	remaining := []int64{500, 250, 125, 62, 31, 15, 7, 3, 1, 0}
	out := []model.TruthHit{}
	for i := 0; i < 11; i++ {
		no := 4 + i
		h := model.TruthHit{Segment: i + 1, ColliderNo: no, ColliderID: "exhaust_gate_" + string(rune('a'+i)), Part: "gate_body", Surface: "aabb:face:-Z", NearestDistanceQ: 0}
		if i < 10 {
			h.Status = "hit"
			h.RemainingDamageQ = remaining[i]
		} else {
			h.Status = "depleted"
			h.RemainingDamageQ = 0
		}
		out = append(out, h)
	}
	return out
}

func GenerateDirectory(dir string) (map[string]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	project := BuildProject()
	files := map[string][]byte{}
	paths := map[string]string{}
	for _, v := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		data, _, err := schema.Export(project, v)
		if err != nil {
			return nil, err
		}
		name := map[string]string{model.SchemaV1: "project.v1.json", model.SchemaV2: "project.v2.json", model.SchemaV3: "project.v3.json"}[v]
		files[name] = data
		paths[v] = filepath.Join(dir, name)
	}
	truthData, err := json.MarshalIndent(BuildTruth(), "", "  ")
	if err != nil {
		return nil, err
	}
	files["truth.json"] = truthData
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
	}
	return paths, nil
}
