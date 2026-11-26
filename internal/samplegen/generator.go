// Package samplegen creates the fixed-seed acceptance sample and truth.
package samplegen

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"wardrobekernel/internal/batching"
	"wardrobekernel/internal/depgraph"
	"wardrobekernel/internal/model"
)

// Generate builds the deterministic sample project. The seed drives stable
// material build hashes while geometry remains intentionally sized for the
// documented batch-pressure scenarios.
func Generate(seed int64) (*model.Project, error) {
	rng := rand.New(rand.NewSource(seed))
	materialBuild := func(prefix string) string {
		return fmt.Sprintf("%s.%04x", prefix, rng.Intn(0xffff))
	}
	ironVersion := materialBuild("v3")

	points := []model.AttachPoint{
		{ID: "face", Label: "Face"},
		{ID: "hair", Label: "Hair"},
		{ID: "torso", Label: "Torso"},
		{ID: "shoulder_l", Label: "Left shoulder"},
		{ID: "hands", Label: "Hands"},
		{ID: "finger_l", Label: "Left finger"},
		{ID: "waist", Label: "Waist"},
		{ID: "legs", Label: "Legs"},
		{ID: "feet", Label: "Feet"},
		{ID: "back", Label: "Back"},
	}

	type subSpec struct {
		id, slot            string
		vertices, triangles int
	}
	type partSpec struct {
		id, label, attach string
		priority          int
		action            string
		skinned           bool
		group             string
		materials         []model.MaterialSlot
		subs              []subSpec
	}

	specs := []partSpec{
		{
			id: "face_base", label: "Base face", attach: "face", priority: 100, action: model.PressureSplit,
			skinned: true, group: "human",
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_skin", MaterialVersion: materialBuild("v2")}},
			subs:      []subSpec{{id: "sm_face", slot: "main", vertices: 180, triangles: 96}},
		},
		{
			id: "chest_plate", label: "Priority chest plate", attach: "torso", priority: 90, action: model.PressureSplit,
			skinned: true, group: "human",
			materials: []model.MaterialSlot{
				{SlotID: "main", MaterialID: "mat_iron", MaterialVersion: ironVersion},
				{SlotID: "trim", MaterialID: "mat_gold", MaterialVersion: materialBuild("v1")},
			},
			subs: []subSpec{
				{id: "sm_chest_body", slot: "main", vertices: 420, triangles: 240},
				{id: "sm_chest_trim", slot: "trim", vertices: 200, triangles: 120},
			},
		},
		{
			id: "hair_long", label: "Long hair", attach: "hair", priority: 80, action: model.PressureSplit,
			skinned: true, group: "human",
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_hair", MaterialVersion: materialBuild("v4")}},
			subs:      []subSpec{{id: "sm_hair_long", slot: "main", vertices: 260, triangles: 160}},
		},
		{
			id: "cape_red", label: "Red cape", attach: "back", priority: 65, action: model.PressureSplit,
			skinned: true, group: "human",
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_cloth_red", MaterialVersion: materialBuild("v1")}},
			subs:      []subSpec{{id: "sm_cape", slot: "main", vertices: 360, triangles: 180}},
		},
		{
			id: "pauldron_l", label: "Left pauldron", attach: "shoulder_l", priority: 60, action: model.PressureSplit,
			skinned: true, group: "human",
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_iron", MaterialVersion: ironVersion}},
			subs:      []subSpec{{id: "sm_pauldron", slot: "main", vertices: 260, triangles: 140}},
		},
		{
			id: "boot_pair", label: "Boot pair", attach: "feet", priority: 55, action: model.PressureSplit,
			skinned: true, group: "human",
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_leather_black", MaterialVersion: materialBuild("v2")}},
			subs:      []subSpec{{id: "sm_boots", slot: "main", vertices: 240, triangles: 130}},
		},
		{
			id: "greave_l", label: "Left greave", attach: "legs", priority: 52, action: model.PressureDefer,
			skinned: true, group: "human",
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_steel", MaterialVersion: materialBuild("v2")}},
			subs:      []subSpec{{id: "sm_greave", slot: "main", vertices: 300, triangles: 160}},
		},
		{
			id: "gauntlet_pair", label: "Gauntlet pair", attach: "hands", priority: 50, action: model.PressureSplit,
			skinned: true, group: "human",
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_iron", MaterialVersion: ironVersion}},
			subs:      []subSpec{{id: "sm_gauntlets", slot: "main", vertices: 220, triangles: 120}},
		},
		{
			id: "waist_belt", label: "Waist belt", attach: "waist", priority: 48, action: model.PressureSplit,
			skinned: false, group: "",
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_leather_brown", MaterialVersion: materialBuild("v1")}},
			subs:      []subSpec{{id: "sm_belt", slot: "main", vertices: 140, triangles: 70}},
		},

		// Losing alternatives and explicit conflict fixtures.
		{
			id: "hair_short", label: "Short hair alternative", attach: "hair", priority: 40, action: model.PressureSplit,
			skinned: true, group: "human",
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_hair_dark", MaterialVersion: materialBuild("v1")}},
			subs:      []subSpec{{id: "sm_hair_short", slot: "main", vertices: 220, triangles: 120}},
		},
		{
			id: "torso_cloth", label: "Cloth torso alternative", attach: "torso", priority: 70, action: model.PressureSplit,
			skinned: true, group: "human",
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_linen", MaterialVersion: materialBuild("v2")}},
			subs:      []subSpec{{id: "sm_torso_cloth", slot: "main", vertices: 540, triangles: 300}},
		},
		{
			id: "pack_heavy", label: "Heavy backpack", attach: "back", priority: 45, action: model.PressureSplit,
			skinned:   false,
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_canvas", MaterialVersion: materialBuild("v4")}},
			subs:      []subSpec{{id: "sm_pack", slot: "main", vertices: 400, triangles: 220}},
		},
		{
			id: "glove_silk", label: "Silk gloves", attach: "hands", priority: 45, action: model.PressureSplit,
			skinned: true, group: "human",
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_silk", MaterialVersion: materialBuild("v1")}},
			subs:      []subSpec{{id: "sm_gloves", slot: "main", vertices: 200, triangles: 100}},
		},
		{
			id: "ring_left", label: "Left ring", attach: "finger_l", priority: 20, action: model.PressureSplit,
			skinned:   false,
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_gem", MaterialVersion: materialBuild("v1")}},
			subs:      []subSpec{{id: "sm_ring", slot: "main", vertices: 32, triangles: 18}},
		},
		{
			id: "sash_arcane", label: "Arcane sash rig", attach: "waist", priority: 35, action: model.PressureSplit,
			skinned: true, group: "mage",
			materials: []model.MaterialSlot{{SlotID: "main", MaterialID: "mat_arcane", MaterialVersion: materialBuild("v5")}},
			subs:      []subSpec{{id: "sm_sash", slot: "main", vertices: 180, triangles: 90}},
		},
	}

	parts := make([]model.Part, 0, len(specs))
	candidate := make([]string, 0, len(specs))
	for _, spec := range specs {
		part := model.Part{
			ID: spec.id, Label: spec.label, AttachPointID: spec.attach,
			ArbitrationPriority: spec.priority, PressureAction: spec.action,
			Skinned: spec.skinned, SkinGroup: spec.group, MaterialSlots: spec.materials,
		}
		vertexCursor, indexCursor := 0, 0
		for _, sub := range spec.subs {
			part.Submeshes = append(part.Submeshes, model.Submesh{
				ID: sub.id, MaterialSlotID: sub.slot,
				VertexStart: vertexCursor, VertexCount: sub.vertices,
				IndexStart: indexCursor, IndexCount: sub.triangles * 3,
			})
			vertexCursor += sub.vertices
			indexCursor += sub.triangles * 3
		}
		part.Vertices = vertexCursor
		part.Triangles = indexCursor / 3
		parts = append(parts, part)
		candidate = append(candidate, spec.id)
	}

	// A fixed but non-alphabetic candidate order proves order independence.
	rng.Shuffle(len(candidate), func(i, j int) { candidate[i], candidate[j] = candidate[j], candidate[i] })

	return &model.Project{
		SchemaVersion:     model.SchemaVersion,
		Seed:              seed,
		AlgorithmVersion:  model.AlgorithmVersion,
		ConstraintVersion: model.ConstraintVersion,
		Character: model.Character{
			ID: "char-aurora-01", Name: "Aurora / sample rig", SamplingPose: "T_pose_sample_01",
		},
		AttachPoints:      points,
		Parts:             parts,
		CandidateAssembly: candidate,
		MutexRules: []model.MutexRule{
			{ID: "mutex_gauntlet_covers_ring", PartIDs: []string{"gauntlet_pair", "ring_left"}, Reason: "heavy gauntlet covers the ring socket"},
		},
		Constraints: model.BatchConstraints{
			MaxMaterialSlots:    4,
			MaxVertexBudget:     1100,
			MaxIndexBudget:      2200,
			AllowCrossSkinBatch: false,
			Arbitration:         model.ArbitrationConfig{TiePolicy: model.TieLexicographicID},
			Scheduler: model.SchedulerConfig{
				MaxParallelism: 2, CandidateBufferPerWorker: 1, PendingQueueBudget: 3,
			},
		},
	}, nil
}

// BuildTruth runs the canonical kernel and enriches cache/dependency fields.
func BuildTruth(project *model.Project) (*model.Truth, *model.Plan, error) {
	plan, err := batching.Plan(project)
	if err != nil {
		return nil, nil, err
	}
	depgraph.Enrich(project, plan)
	return &model.Truth{
		SchemaVersion:     model.TruthSchemaVersion,
		Seed:              project.Seed,
		AlgorithmVersion:  project.AlgorithmVersion,
		ConstraintVersion: project.ConstraintVersion,
		SamplingPose:      project.Character.SamplingPose,
		Assembly:          plan.Assembly,
		Batches:           plan.Batches,
		Decisions:         plan.Decisions,
		CommitSequence:    plan.CommitSequence,
	}, plan, nil
}

// WriteSampleDir writes project.json, truth.json and diagnostic fixtures.
func WriteSampleDir(dir string, seed int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	project, err := Generate(seed)
	if err != nil {
		return err
	}
	truth, _, err := BuildTruth(project)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "project.json"), project); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "truth.json"), truth); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "invalid_schema.json"), InvalidSchemaProject()); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "ambiguous_arb.json"), AmbiguousArbProject()); err != nil {
		return err
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// InvalidSchemaProject intentionally contains every supported field-path issue.
func InvalidSchemaProject() *model.Project {
	return &model.Project{
		SchemaVersion: model.SchemaVersion, Seed: 1, Character: model.Character{ID: "bad"},
		AttachPoints: []model.AttachPoint{{ID: "socket_a"}, {ID: "socket_a"}},
		Parts: []model.Part{
			{
				ID: "p_bad_ref", AttachPointID: "missing", Vertices: 10, Triangles: 4,
				MaterialSlots: []model.MaterialSlot{{SlotID: "m", MaterialVersion: "v"}},
				Submeshes: []model.Submesh{
					{ID: "sm1", MaterialSlotID: "m", VertexStart: 0, VertexCount: 8, IndexStart: 0, IndexCount: 6},
					{ID: "sm2", MaterialSlotID: "m", VertexStart: 6, VertexCount: 5, IndexStart: 3, IndexCount: 6},
				},
			},
		},
		MutexRules:  []model.MutexRule{{ID: "rule_bad", PartIDs: []string{"p_bad_ref", "ghost_part"}}},
		Constraints: model.BatchConstraints{MaxMaterialSlots: 0, MaxVertexBudget: -1, MaxIndexBudget: 0},
	}
}

// AmbiguousArbProject has equal-priority contenders and strict tie policy.
func AmbiguousArbProject() *model.Project {
	return &model.Project{
		SchemaVersion: model.SchemaVersion, Seed: 2, Character: model.Character{ID: "arb"},
		AlgorithmVersion: model.AlgorithmVersion, ConstraintVersion: "arb-v1",
		AttachPoints: []model.AttachPoint{{ID: "a"}, {ID: "b"}},
		Parts: []model.Part{
			{
				ID: "part_a", AttachPointID: "a", Vertices: 10, Triangles: 4, ArbitrationPriority: 50,
				MaterialSlots: []model.MaterialSlot{{SlotID: "shared_slot", MaterialID: "mat_a", MaterialVersion: "1"}},
				Submeshes:     []model.Submesh{{ID: "sm_a", MaterialSlotID: "shared_slot", VertexStart: 0, VertexCount: 10, IndexStart: 0, IndexCount: 12}},
			},
			{
				ID: "part_b", AttachPointID: "b", Vertices: 10, Triangles: 4, ArbitrationPriority: 50,
				MaterialSlots: []model.MaterialSlot{{SlotID: "shared_slot", MaterialID: "mat_b", MaterialVersion: "1"}},
				Submeshes:     []model.Submesh{{ID: "sm_b", MaterialSlotID: "shared_slot", VertexStart: 0, VertexCount: 10, IndexStart: 0, IndexCount: 12}},
			},
		},
		CandidateAssembly: []string{"part_a", "part_b"},
		Constraints: model.BatchConstraints{
			MaxMaterialSlots: 4, MaxVertexBudget: 100, MaxIndexBudget: 100,
			Arbitration: model.ArbitrationConfig{TiePolicy: model.TieStrict},
			Scheduler:   model.SchedulerConfig{MaxParallelism: 1, CandidateBufferPerWorker: 1, PendingQueueBudget: 1},
		},
	}
}
