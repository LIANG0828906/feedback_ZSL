package dressbatch

const (
	SchemaVersion    = "1"
	AlgorithmVersion = "pack-v1"
	PolicyPriority   = "priority-first-fit-v1"
)

type MaterialSlot struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type MountPoint struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type Submesh struct {
	ID           string `json:"id"`
	MaterialSlot string `json:"material_slot"`
	VertexStart  int    `json:"vertex_start"`
	VertexCount  int    `json:"vertex_count"`
	IndexStart   int    `json:"index_start"`
	IndexCount   int    `json:"index_count"`
}

type Part struct {
	ID            string    `json:"id"`
	Name          string    `json:"name,omitempty"`
	MountID       string    `json:"mount_id"`
	VertexCount   int       `json:"vertex_count"`
	TriangleCount int       `json:"triangle_count"`
	IndexCount    int       `json:"index_count"`
	MaterialSlots []string  `json:"material_slots"`
	Submeshes     []Submesh `json:"submeshes"`
	Skin          bool      `json:"skin"`
	SkinGroup     string    `json:"skin_group,omitempty"`
	Priority      int       `json:"priority"`
}

func (p Part) EffectiveIndexCount() int {
	if p.IndexCount > 0 {
		return p.IndexCount
	}
	return p.TriangleCount * 3
}

type MutualExclusion struct {
	ID     string `json:"id"`
	A      string `json:"a"`
	B      string `json:"b"`
	Reason string `json:"reason,omitempty"`
}

type BatchConstraints struct {
	MaxMaterialSlotsPerBatch int    `json:"max_material_slots_per_batch"`
	MaxVerticesPerBatch      int    `json:"max_vertices_per_batch"`
	MaxIndicesPerBatch       int    `json:"max_indices_per_batch"`
	AllowCrossSkin           bool   `json:"allow_cross_skin"`
	Policy                   string `json:"policy"`
}

type Truth struct {
	Assemblies []TruthAssembly `json:"assemblies"`
}

type TruthAssembly struct {
	ID                      string       `json:"id"`
	Pose                    string       `json:"pose"`
	CandidatePartIDs        []string     `json:"candidate_part_ids"`
	ExpectedLegal           bool         `json:"expected_legal"`
	ExpectedAcceptedPartIDs []string     `json:"expected_accepted_part_ids"`
	ExpectedConflicts       []Conflict   `json:"expected_conflicts"`
	ExpectedBatches         []BatchTruth `json:"expected_batches"`
	ExpectedCommitSequence  []string     `json:"expected_commit_sequence"`
}

type BatchTruth struct {
	BatchID       string          `json:"batch_id"`
	PartIDs       []string        `json:"part_ids"`
	MaterialSlots []string        `json:"material_slots"`
	Skin          bool            `json:"skin"`
	MixedSkin     bool            `json:"mixed_skin"`
	SkinGroups    []string        `json:"skin_groups"`
	VertexUsed    int             `json:"vertex_used"`
	IndexUsed     int             `json:"index_used"`
	Placements    []PartPlacement `json:"placements"`
}

type Project struct {
	SchemaVersion    string            `json:"schema_version"`
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Seed             int64             `json:"seed"`
	Materials        []MaterialSlot    `json:"materials"`
	Mounts           []MountPoint      `json:"mounts"`
	Parts            []Part            `json:"parts"`
	MutualExclusions []MutualExclusion `json:"mutual_exclusions"`
	Constraints      BatchConstraints  `json:"constraints"`
	Truth            Truth             `json:"truth"`
}

const (
	ConflictMount = "mount_conflict"
	ConflictRule  = "explicit_exclusion"
	ConflictSkin  = "skin_incompatible"
)

type Conflict struct {
	Category string   `json:"category"`
	PartIDs  []string `json:"part_ids"`
	MountID  string   `json:"mount_id,omitempty"`
	RuleID   string   `json:"rule_id,omitempty"`
	Detail   string   `json:"detail"`
}

type AssemblyResult struct {
	CandidatePartIDs []string   `json:"candidate_part_ids"`
	AcceptedPartIDs  []string   `json:"accepted_part_ids"`
	RejectedPartIDs  []string   `json:"rejected_part_ids"`
	Legal            bool       `json:"legal"`
	Conflicts        []Conflict `json:"conflicts"`
}

const (
	DecisionPlace = "place"
	DecisionSplit = "split"
	DecisionDefer = "defer"
)

type Decision struct {
	Sequence               int    `json:"sequence"`
	BatchID                string `json:"batch_id"`
	PartID                 string `json:"part_id"`
	Action                 string `json:"action"`
	Reason                 string `json:"reason"`
	RemainingMaterialSlots int    `json:"remaining_material_slots"`
	RemainingVertexBudget  int    `json:"remaining_vertex_budget"`
	RemainingIndexBudget   int    `json:"remaining_index_budget"`
	PartVertexCount        int    `json:"part_vertex_count"`
	PartIndexCount         int    `json:"part_index_count"`
}

type SubmeshPlacement struct {
	PartID            string `json:"part_id"`
	SubmeshID         string `json:"submesh_id"`
	MaterialSlot      string `json:"material_slot"`
	VertexOffset      int    `json:"vertex_offset"`
	VertexStart       int    `json:"vertex_start"`
	VertexEnd         int    `json:"vertex_end"`
	IndexOffset       int    `json:"index_offset"`
	IndexStart        int    `json:"index_start"`
	IndexEnd          int    `json:"index_end"`
	SourceVertexStart int    `json:"source_vertex_start"`
	SourceVertexEnd   int    `json:"source_vertex_end"`
	SourceIndexStart  int    `json:"source_index_start"`
	SourceIndexEnd    int    `json:"source_index_end"`
}

type PartPlacement struct {
	PartID       string             `json:"part_id"`
	BatchID      string             `json:"batch_id"`
	VertexOffset int                `json:"vertex_offset"`
	VertexStart  int                `json:"vertex_start"`
	VertexEnd    int                `json:"vertex_end"`
	IndexOffset  int                `json:"index_offset"`
	IndexStart   int                `json:"index_start"`
	IndexEnd     int                `json:"index_end"`
	Submeshes    []SubmeshPlacement `json:"submeshes"`
}

type Batch struct {
	ID            string          `json:"batch_id"`
	Index         int             `json:"batch_index"`
	PartIDs       []string        `json:"part_ids"`
	MaterialSlots []string        `json:"material_slots"`
	Skin          bool            `json:"skin"`
	MixedSkin     bool            `json:"mixed_skin"`
	SkinGroups    []string        `json:"skin_groups"`
	VertexUsed    int             `json:"vertex_used"`
	IndexUsed     int             `json:"index_used"`
	Placements    []PartPlacement `json:"placements"`
	Decisions     []Decision      `json:"decisions"`
	Dependencies  []string        `json:"dependencies"`
	ContentHash   string          `json:"content_hash"`
	CacheKey      string          `json:"cache_key"`
}

type PackResult struct {
	ProjectID         string     `json:"project_id"`
	CandidatePartIDs  []string   `json:"candidate_part_ids"`
	AcceptedPartIDs   []string   `json:"accepted_part_ids"`
	Batches           []Batch    `json:"batches"`
	CommitSequence    []string   `json:"commit_sequence"`
	Conflicts         []Conflict `json:"conflicts"`
	ConstraintVersion string     `json:"constraint_version"`
	MaterialVersion   string     `json:"material_version"`
	AlgorithmVersion  string     `json:"algorithm_version"`
}

type InvalidationEntry struct {
	BatchID       string   `json:"batch_id"`
	Reasons       []string `json:"reasons"`
	AffectedParts []string `json:"affected_part_ids"`
}

type InvalidationReport struct {
	Entries        []InvalidationEntry `json:"entries"`
	ReusedBatchIDs []string            `json:"reused_batch_ids"`
	FirstInvalid   string              `json:"first_invalid_batch_id,omitempty"`
}

type IncrementalRequest struct {
	OldProject       *Project
	NewProject       *Project
	Previous         *PackResult
	CandidatePartIDs []string
	ReusedPrefix     []Batch
}

type IncrementalResult struct {
	Result       *PackResult
	Invalidation InvalidationReport
}

type TruthCaseReport struct {
	CaseID          string `json:"case_id"`
	Pose            string `json:"pose"`
	Legal           bool   `json:"legal"`
	ExpectedLegal   bool   `json:"expected_legal"`
	ConflictsMatch  bool   `json:"conflicts_match"`
	BatchesMatch    bool   `json:"batches_match"`
	PlacementsMatch bool   `json:"placements_match"`
}

type TruthReport struct {
	ProjectID string            `json:"project_id"`
	Passed    bool              `json:"passed"`
	Cases     []TruthCaseReport `json:"cases"`
}
