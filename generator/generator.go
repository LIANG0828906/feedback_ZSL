package generator

import (
	"fmt"
	"math/rand"

	"wardrobe/domain"
)

const DefaultSeed int64 = 20261126

// Generate creates a deterministic bundle. No clock, host, or network state is
// used, so the same seed always produces byte-identical JSON.
func Generate(seed int64) domain.Bundle {
	rng := rand.New(rand.NewSource(seed))
	projects := make([]domain.Project, 0, 4)
	for index := 0; index < 4; index++ {
		projects = append(projects, buildProject(index, rng))
	}
	return domain.Bundle{
		SchemaVersion:    domain.SchemaVersion,
		GeneratorVersion: domain.GeneratorVersion,
		AlgorithmVersion: domain.AlgorithmVersion,
		Seed:             seed,
		Projects:         projects,
	}
}

func buildProject(index int, rng *rand.Rand) domain.Project {
	baseVertices := 720 + index*90 + rng.Intn(31)
	baseTriangles := 940 + index*75 + rng.Intn(41)
	projectID := fmt.Sprintf("char-%02d-%s", index+1, []string{"hexguard", "scout", "raider", "warden"}[index])
	projectName := []string{"Hexguard", "Scout", "Raider", "Warden"}[index]

	mounts := []domain.Mount{
		{ID: "root", Name: "Root", SkinMode: "any"},
		{ID: "torso", Name: "Torso", ParentID: "root", SkinMode: "skinned"},
		{ID: "head", Name: "Head", ParentID: "torso", SkinMode: "skinned"},
		{ID: "hand_l", Name: "Left Hand", ParentID: "torso", SkinMode: "rigid"},
		{ID: "hand_r", Name: "Right Hand", ParentID: "torso", SkinMode: "rigid"},
		{ID: "back", Name: "Back", ParentID: "torso", SkinMode: "rigid"},
		{ID: "hip", Name: "Hip", ParentID: "root", SkinMode: "any"},
	}
	materials := []domain.MaterialSlot{
		{ID: "mat_cloth", Name: "Cloth", Version: 1},
		{ID: "mat_metal", Name: "Metal", Version: 1 + index},
		{ID: "mat_trim", Name: "Trim", Version: 1},
		{ID: "mat_leather", Name: "Leather", Version: 1},
		{ID: "mat_emissive", Name: "Emissive", Version: 1},
	}

	torso := makePart("torso_plate", "Torso Plate", "torso", true, 90, []submeshSpec{
		{Slot: "mat_cloth", Vertices: baseVertices, Triangles: baseTriangles},
		{Slot: "mat_metal", Vertices: max(1, baseVertices/4), Triangles: max(1, baseTriangles/5)},
	})
	helm := makePart("head_helm", "Helm", "head", true, 80, []submeshSpec{
		{Slot: "mat_metal", Vertices: max(1, baseVertices/2), Triangles: max(1, baseTriangles/2)},
		{Slot: "mat_trim", Vertices: max(1, baseVertices/10), Triangles: max(1, baseTriangles/12)},
	})
	leftGlove := makePart("hand_l_glove", "Left Glove", "hand_l", false, 70, []submeshSpec{
		{Slot: "mat_cloth", Vertices: max(1, baseVertices/5), Triangles: max(1, baseTriangles/6)},
	})
	rightGlove := makePart("hand_r_glove", "Right Glove", "hand_r", false, 70, []submeshSpec{
		{Slot: "mat_cloth", Vertices: max(1, baseVertices/5), Triangles: max(1, baseTriangles/6)},
	})
	maxVertices := torso.Mesh.VertexCount + max(32, baseVertices/10)
	bannerVertices := maxVertices - leftGlove.Mesh.VertexCount - rightGlove.Mesh.VertexCount + max(16, baseVertices/4)
	bannerTriangles := max(1, baseTriangles*13/10)
	banner := makePart("back_banner", "Back Banner", "back", false, 50, []submeshSpec{
		{Slot: "mat_cloth", Vertices: bannerVertices, Triangles: bannerTriangles},
	})
	cape := makePart("torso_cape", "Torso Cape", "torso", true, 88, []submeshSpec{
		{Slot: "mat_cloth", Vertices: max(1, baseVertices*9/10), Triangles: max(1, baseTriangles*9/10)},
		{Slot: "mat_leather", Vertices: max(1, baseVertices*3/20), Triangles: max(1, baseTriangles/7)},
	})
	mask := makePart("head_mask", "Rigid Mask", "head", false, 78, []submeshSpec{
		{Slot: "mat_metal", Vertices: max(1, baseVertices*3/10), Triangles: max(1, baseTriangles*3/10)},
	})
	blade := makePart("hand_l_blade", "Blade", "hand_l", false, 65, []submeshSpec{
		{Slot: "mat_metal", Vertices: max(1, baseVertices/4), Triangles: max(1, baseTriangles/3)},
		{Slot: "mat_emissive", Vertices: max(1, baseVertices/12), Triangles: max(1, baseTriangles/10)},
	})
	pouch := makePart("hip_pouch", "Hip Pouch", "hip", false, 40, []submeshSpec{
		{Slot: "mat_leather", Vertices: max(1, baseVertices*3/20), Triangles: max(1, baseTriangles/8)},
	})

	parts := []domain.Part{torso, helm, leftGlove, rightGlove, banner, cape, mask, blade, pouch}
	maxIndex := 0
	for _, part := range parts {
		if part.Mesh.TriangleCount*3 > maxIndex {
			maxIndex = part.Mesh.TriangleCount * 3
		}
	}
	maxVertices = max(maxVertices, banner.Mesh.VertexCount)
	extraIndexBudget := max(300, baseTriangles)
	if index%2 == 0 {
		// Even-numbered characters make index pressure the first blocker while
		// still leaving room for a small high-index accessory.
		maxVertices += max(64, baseVertices/4)
		extraIndexBudget = max(90, baseTriangles/8)
	}

	project := domain.Project{
		SchemaVersion: domain.SchemaVersion,
		ProjectID:     projectID,
		Name:          projectName,
		Revision:      1,
		Mounts:        mounts,
		Materials:     materials,
		Parts:         parts,
		ExclusionRules: []domain.ExclusionRule{
			{
				ID:          "rule_glove_blade",
				PartA:       "hand_l_glove",
				PartB:       "hand_l_blade",
				Description: "a blade cannot be worn over the glove socket",
			},
			{
				ID:          "rule_cape_banner",
				PartA:       "torso_cape",
				PartB:       "back_banner",
				Description: "cape and banner share the same rear cloth rig",
			},
			{
				ID:          "rule_mask_helm",
				PartA:       "head_mask",
				PartB:       "head_helm",
				Description: "rigid mask cannot be combined with the skinned helm",
			},
		},
		Constraints: domain.BatchConstraints{
			Version:          fmt.Sprintf("constraints-%02d-v1", index+1),
			MaxMaterialSlots: 2,
			MaxVertices:      maxVertices,
			MaxIndices:       maxIndex + extraIndexBudget,
			AllowCrossSkin:   false,
		},
	}
	project.Truth = buildTruth(project, []caseSpec{
		{
			ID:          "pose-main",
			Description: "Legal main pose; material, skin and budget boundaries all exercised.",
			Selected:    []string{"torso_plate", "head_helm", "hand_l_glove", "hand_r_glove", "back_banner"},
		},
		{
			ID:          "pose-tradeoff",
			Description: "Legal pose with material reuse plus repeated budget pressure.",
			Selected:    []string{"torso_plate", "head_helm", "hand_l_glove", "hand_r_glove", "back_banner", "hip_pouch"},
		},
		{
			ID:          "pose-conflicts",
			Description: "All candidates selected; mount, exclusion and skin conflicts are diagnosed.",
			Selected: []string{
				"torso_plate", "torso_cape", "head_helm", "head_mask",
				"hand_l_glove", "hand_r_glove", "hand_l_blade", "back_banner", "hip_pouch",
			},
		},
		{
			ID:          "pose-rigid-chain",
			Description: "Rigid-only pose showing material affinity followed by budget splits.",
			Selected:    []string{"hand_l_glove", "hand_r_glove", "back_banner", "hip_pouch"},
		},
	})
	return project
}

type submeshSpec struct {
	Slot      string
	Vertices  int
	Triangles int
}

func makePart(id, name, mountID string, skinned bool, priority int, specs []submeshSpec) domain.Part {
	part := domain.Part{
		ID:            id,
		Name:          name,
		MountID:       mountID,
		Skinned:       skinned,
		BatchPriority: priority,
	}
	vertexStart := 0
	indexStart := 0
	for index, spec := range specs {
		part.Mesh.Submeshes = append(part.Mesh.Submeshes, domain.Submesh{
			ID:           fmt.Sprintf("%s_sm%d", id, index+1),
			MaterialSlot: spec.Slot,
			VertexStart:  vertexStart,
			VertexCount:  spec.Vertices,
			IndexStart:   indexStart,
			IndexCount:   spec.Triangles * 3,
		})
		vertexStart += spec.Vertices
		indexStart += spec.Triangles * 3
	}
	part.Mesh.VertexCount = vertexStart
	part.Mesh.TriangleCount = indexStart / 3
	return part
}

type caseSpec struct {
	ID          string
	Description string
	Selected    []string
}

func buildTruth(project domain.Project, specs []caseSpec) domain.ProjectTruth {
	truth := domain.ProjectTruth{}
	for _, spec := range specs {
		assembly := referenceEvaluate(project, spec.Selected)
		plan := referencePlan(project, assembly.AcceptedPartIDs)
		truth.Cases = append(truth.Cases, domain.ExpectedCase{
			CaseID:          spec.ID,
			Description:     spec.Description,
			SelectedPartIDs: assembly.SelectedPartIDs,
			Valid:           assembly.Valid,
			AcceptedPartIDs: assembly.AcceptedPartIDs,
			Conflicts:       assembly.Conflicts,
			Batches:         plan.Batches,
			Decisions:       plan.Decisions,
			CommitSequence:  plan.CommitSequence,
		})
	}
	return truth
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
