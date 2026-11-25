package dressbatch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type deterministicRNG struct {
	state uint64
}

func newDeterministicRNG(seed uint64) *deterministicRNG {
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return &deterministicRNG{state: seed}
}

func (r *deterministicRNG) next() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 2685821657736338717
}

func (r *deterministicRNG) between(min, max int) int {
	if max <= min {
		return min
	}
	return min + int(r.next()%uint64(max-min+1))
}

func GenerateSampleSuite(seed int64, count int) ([]*Project, error) {
	if count <= 0 {
		return nil, newError(CodeSchema, "generator", "count", "must be greater than zero")
	}
	projects := make([]*Project, 0, count)
	for i := 0; i < count; i++ {
		project, err := GenerateProject(seed, i)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, nil
}

func GenerateProject(seed int64, index int) (*Project, error) {
	if index < 0 {
		return nil, newError(CodeSchema, "generator", "index", "must not be negative")
	}
	rng := newDeterministicRNG(uint64(seed) + uint64(index+1)*0x9e3779b97f4a7c15)
	projectID := fmt.Sprintf("char-%02d", index+1)
	materials := []MaterialSlot{
		{ID: "base", Version: "1"},
		{ID: "skin", Version: "1"},
		{ID: "cloth", Version: "1"},
		{ID: "metal", Version: "1"},
		{ID: "hair", Version: "1"},
		{ID: "trim", Version: "1"},
		{ID: "glow", Version: "1"},
	}
	mounts := []MountPoint{
		{ID: "body", Name: "Body"},
		{ID: "head", Name: "Head"},
		{ID: "hair", Name: "Hair"},
		{ID: "torso", Name: "Torso"},
		{ID: "back", Name: "Back"},
		{ID: "hand_l", Name: "Left Hand"},
		{ID: "hand_r", Name: "Right Hand"},
		{ID: "waist", Name: "Waist"},
		{ID: "aura", Name: "Aura Anchor"},
	}

	parts := []Part{
		generatedPart("body", "body", "body", true, "body", 100, rng.between(900, 1100), []string{"base"}, false),
		generatedPart("head", "head", "head", true, "body", 90, rng.between(380, 500), []string{"skin"}, false),
		generatedPart("hair_a", "hair_a", "hair", true, "body", 80, rng.between(140, 220), []string{"hair"}, false),
		generatedPart("hair_b", "hair_b", "hair", true, "body", 78, rng.between(150, 230), []string{"hair"}, false),
		generatedPart("torso_a", "torso_a", "torso", false, "", 70, rng.between(700, 900), []string{"cloth", "metal"}, true),
		generatedPart("torso_b", "torso_b", "torso", false, "", 68, rng.between(650, 850), []string{"cloth"}, false),
		generatedPart("cape", "cape", "back", false, "", 60, rng.between(300, 450), []string{"cloth", "trim"}, true),
		generatedPart("pack", "pack", "back", false, "", 58, rng.between(320, 500), []string{"cloth", "metal"}, true),
		generatedPart("hand_l", "hand_l", "hand_l", false, "", 55, rng.between(100, 180), []string{"metal"}, false),
		generatedPart("hand_r", "hand_r", "hand_r", false, "", 54, rng.between(100, 180), []string{"metal"}, false),
		generatedPart("aura", "aura", "aura", true, "fx", 40, rng.between(100, 200), []string{"glow"}, false),
		generatedPart("tail", "tail", "waist", true, "body", 50, rng.between(200, 350), []string{"skin"}, false),
	}
	for i := range parts {
		parts[i].Priority += index * 3
	}

	maxVertices := 0
	maxIndices := 0
	for _, part := range parts {
		if part.VertexCount > maxVertices {
			maxVertices = part.VertexCount
		}
		if part.EffectiveIndexCount() > maxIndices {
			maxIndices = part.EffectiveIndexCount()
		}
	}
	constraints := BatchConstraints{
		MaxMaterialSlotsPerBatch: rng.between(2, 3),
		MaxVerticesPerBatch:      maxVertices + rng.between(500, 900),
		MaxIndicesPerBatch:       maxIndices + rng.between(1500, 3000),
		AllowCrossSkin:           rng.next()%2 == 0,
		Policy:                   PolicyPriority,
	}

	project := &Project{
		SchemaVersion: SchemaVersion,
		ID:            projectID,
		Name:          fmt.Sprintf("Deterministic Character %02d", index+1),
		Seed:          seed,
		Materials:     materials,
		Mounts:        mounts,
		Parts:         parts,
		MutualExclusions: []MutualExclusion{
			{ID: "ex_armor_pack", A: "torso_a", B: "pack", Reason: "backpack clips through rigid armor"},
			{ID: "ex_robe_cape", A: "torso_b", B: "cape", Reason: "cape and robe share the same cloth binding"},
		},
		Constraints: constraints,
	}
	cases := generatedCases()
	project.Truth = Truth{Assemblies: make([]TruthAssembly, 0, len(cases))}
	for _, candidate := range cases {
		assembly, err := EvaluateAssembly(project, candidate.partIDs)
		if err != nil {
			return nil, err
		}
		truthCase := TruthAssembly{
			ID:                      candidate.id,
			Pose:                    candidate.pose,
			CandidatePartIDs:        sortedCopy(candidate.partIDs),
			ExpectedLegal:           assembly.Legal,
			ExpectedAcceptedPartIDs: append([]string{}, assembly.AcceptedPartIDs...),
			ExpectedConflicts:       append([]Conflict{}, assembly.Conflicts...),
		}
		if assembly.Legal {
			result, err := Pack(project, candidate.partIDs)
			if err != nil {
				return nil, err
			}
			truthCase.ExpectedBatches = make([]BatchTruth, 0, len(result.Batches))
			truthCase.ExpectedCommitSequence = append([]string(nil), result.CommitSequence...)
			for _, batch := range result.Batches {
				truthCase.ExpectedBatches = append(truthCase.ExpectedBatches, BatchTruth{
					BatchID:       batch.ID,
					PartIDs:       append([]string{}, batch.PartIDs...),
					MaterialSlots: append([]string{}, batch.MaterialSlots...),
					Skin:          batch.Skin,
					MixedSkin:     batch.MixedSkin,
					SkinGroups:    append([]string{}, batch.SkinGroups...),
					VertexUsed:    batch.VertexUsed,
					IndexUsed:     batch.IndexUsed,
					Placements:    append([]PartPlacement{}, batch.Placements...),
				})
			}
		} else {
			truthCase.ExpectedBatches = []BatchTruth{}
			truthCase.ExpectedCommitSequence = []string{}
		}
		project.Truth.Assemblies = append(project.Truth.Assemblies, truthCase)
	}
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	return project, nil
}

type generatedCase struct {
	id      string
	pose    string
	partIDs []string
}

func generatedCases() []generatedCase {
	return []generatedCase{
		{
			id:      "idle-legal",
			pose:    "idle",
			partIDs: []string{"body", "head", "hair_a", "torso_a", "hand_l", "hand_r", "tail"},
		},
		{
			id:      "run-legal",
			pose:    "run",
			partIDs: []string{"body", "head", "hair_b", "torso_a", "cape", "hand_l", "hand_r", "tail"},
		},
		{
			id:      "mount-conflict",
			pose:    "inspect",
			partIDs: []string{"hair_a", "hair_b", "hand_l"},
		},
		{
			id:      "explicit-exclusion",
			pose:    "combat",
			partIDs: []string{"head", "torso_a", "pack"},
		},
		{
			id:      "skin-conflict",
			pose:    "cast",
			partIDs: []string{"body", "aura"},
		},
		{
			id:      "combined-conflict",
			pose:    "combat",
			partIDs: []string{"body", "head", "hair_a", "hair_b", "torso_b", "cape", "aura"},
		},
	}
}

func generatedPart(id, name, mountID string, skin bool, skinGroup string, priority, vertexCount int, slots []string, split bool) Part {
	triangles := vertexCount / 2
	if triangles < 2 {
		triangles = 2
	}
	indexCount := triangles * 3
	return Part{
		ID:            id,
		Name:          name,
		MountID:       mountID,
		VertexCount:   vertexCount,
		TriangleCount: triangles,
		IndexCount:    indexCount,
		MaterialSlots: append([]string(nil), slots...),
		Submeshes:     generatedSubmeshes(id, vertexCount, indexCount, slots, split),
		Skin:          skin,
		SkinGroup:     skinGroup,
		Priority:      priority,
	}
}

func generatedSubmeshes(partID string, vertexCount, indexCount int, slots []string, split bool) []Submesh {
	if !split || len(slots) < 2 {
		return []Submesh{{
			ID:           partID + "-main",
			MaterialSlot: slots[0],
			VertexStart:  0,
			VertexCount:  vertexCount,
			IndexStart:   0,
			IndexCount:   indexCount,
		}}
	}
	vertexSplit := vertexCount * 3 / 5
	if vertexSplit <= 0 || vertexSplit >= vertexCount {
		vertexSplit = vertexCount / 2
	}
	triangleCount := indexCount / 3
	triangleSplit := triangleCount / 2
	indexSplit := triangleSplit * 3
	return []Submesh{
		{
			ID:           partID + "-primary",
			MaterialSlot: slots[0],
			VertexStart:  0,
			VertexCount:  vertexSplit,
			IndexStart:   0,
			IndexCount:   indexSplit,
		},
		{
			ID:           partID + "-secondary",
			MaterialSlot: slots[1],
			VertexStart:  vertexSplit,
			VertexCount:  vertexCount - vertexSplit,
			IndexStart:   indexSplit,
			IndexCount:   indexCount - indexSplit,
		},
	}
}

type sampleManifest struct {
	Seed             int64           `json:"seed"`
	Count            int             `json:"count"`
	AlgorithmVersion string          `json:"algorithm_version"`
	Files            []sampleFileRef `json:"files"`
}

type sampleFileRef struct {
	ProjectID string `json:"project_id"`
	File      string `json:"file"`
	SHA256    string `json:"sha256"`
}

func WriteSampleSuite(dir string, seed int64, count int) ([]string, error) {
	projects, err := GenerateSampleSuite(seed, count)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, newError(CodeSchema, "generator", "output_dir", "cannot create %q: %v", dir, err)
	}
	manifest := sampleManifest{Seed: seed, Count: count, AlgorithmVersion: AlgorithmVersion}
	files := make([]string, 0, count)
	for i, project := range projects {
		data, err := MarshalProject(project)
		if err != nil {
			return nil, err
		}
		name := fmt.Sprintf("sample_%02d.json", i)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, newError(CodeSchema, "generator", "output_file", "cannot write %q: %v", path, err)
		}
		manifest.Files = append(manifest.Files, sampleFileRef{
			ProjectID: project.ID,
			File:      name,
			SHA256:    hashBytes(data),
		})
		files = append(files, path)
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, append(manifestData, '\n'), 0o644); err != nil {
		return nil, newError(CodeSchema, "generator", "output_file", "cannot write %q: %v", manifestPath, err)
	}
	return files, nil
}
