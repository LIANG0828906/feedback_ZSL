// Package model contains the stable wardrobe batch domain documents.
package model

import "wardrobekernel/internal/xerr"

const (
	SchemaVersion      = "wardrobe.project/v1"
	TruthSchemaVersion = "wardrobe.truth/v1"
	AlgorithmVersion   = "batch-kernel-v1.0.0"
	ConstraintVersion  = "constraints-sample-v1"
	FixedSeed          = int64(20261127)

	ConflictAttachPoint = "ATTACH_POINT_CONTENTION"
	ConflictMutex       = "EXPLICIT_MUTEX"
	ConflictSkin        = "SKIN_INCOMPATIBLE"

	PressureSplit = "split"
	PressureDefer = "defer"

	TieLexicographicID = "lexicographic_id"
	TieStrict          = "strict"
)

// Bundle is the complete deterministic sample input and truth.
type Bundle struct {
	SchemaVersion     string  `json:"schema_version"`
	Seed              int64   `json:"seed"`
	AlgorithmVersion  string  `json:"algorithm_version"`
	ConstraintVersion string  `json:"constraint_version"`
	Project           Project `json:"project"`
	Truth             Truth   `json:"truth"`
}

// Project contains all authoring input except the expected truth.
type Project struct {
	SchemaVersion     string           `json:"schema_version"`
	Seed              int64            `json:"seed"`
	AlgorithmVersion  string           `json:"algorithm_version"`
	ConstraintVersion string           `json:"constraint_version"`
	Character         Character        `json:"character"`
	AttachPoints      []AttachPoint    `json:"attach_points"`
	Parts             []Part           `json:"parts"`
	MutexRules        []MutexRule      `json:"mutex_rules"`
	Constraints       BatchConstraints `json:"batch_constraints"`
	CandidateAssembly []string         `json:"candidate_assembly"`
}

type Character struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	SamplingPose string `json:"sampling_pose"`
}

type AttachPoint struct {
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`
	Required bool   `json:"required,omitempty"`
}

type MaterialSlot struct {
	SlotID          string `json:"slot_id"`
	MaterialID      string `json:"material_id"`
	MaterialVersion string `json:"material_version"`
}

type Submesh struct {
	ID             string `json:"id"`
	MaterialSlotID string `json:"material_slot_id"`
	VertexStart    int    `json:"vertex_start"`
	VertexCount    int    `json:"vertex_count"`
	IndexStart     int    `json:"index_start"`
	IndexCount     int    `json:"index_count"`
}

type Part struct {
	ID                  string         `json:"id"`
	Label               string         `json:"label,omitempty"`
	AttachPointID       string         `json:"attach_point_id"`
	Vertices            int            `json:"vertices"`
	Triangles           int            `json:"triangles"`
	MaterialSlots       []MaterialSlot `json:"material_slots"`
	Submeshes           []Submesh      `json:"submeshes"`
	Skinned             bool           `json:"skinned"`
	SkinGroup           string         `json:"skin_group,omitempty"`
	ArbitrationPriority int            `json:"arbitration_priority"`
	PressureAction      string         `json:"pressure_action"`
}

// IndexCount returns the part index-buffer length implied by triangle count.
func (p Part) IndexCount() int { return p.Triangles * 3 }

// SkinKey identifies batching/assembly skin regime.
func (p Part) SkinKey() string {
	if !p.Skinned || p.SkinGroup == "" {
		return "unskinned"
	}
	return "skin:" + p.SkinGroup
}

type MutexRule struct {
	ID      string   `json:"id"`
	PartIDs []string `json:"part_ids"`
	Reason  string   `json:"reason,omitempty"`
}

type ArbitrationConfig struct {
	TiePolicy string `json:"tie_policy"`
}

type SchedulerConfig struct {
	MaxParallelism           int `json:"max_parallelism"`
	CandidateBufferPerWorker int `json:"candidate_buffer_per_worker"`
	PendingQueueBudget       int `json:"pending_queue_budget"`
}

type BatchConstraints struct {
	MaxMaterialSlots    int               `json:"max_material_slots"`
	MaxVertexBudget     int               `json:"max_vertex_budget"`
	MaxIndexBudget      int               `json:"max_index_budget"`
	AllowCrossSkinBatch bool              `json:"allow_cross_skin_batching"`
	Arbitration         ArbitrationConfig `json:"arbitration"`
	Scheduler           SchedulerConfig   `json:"scheduler"`
}

type Conflict struct {
	Category      string   `json:"category"`
	Code          string   `json:"code"`
	PartIDs       []string `json:"part_ids"`
	WinnerID      string   `json:"winner_id,omitempty"`
	LoserID       string   `json:"loser_id,omitempty"`
	AttachPointID string   `json:"attach_point_id,omitempty"`
	RuleID        string   `json:"rule_id,omitempty"`
	Message       string   `json:"message"`
}

type AssemblyResult struct {
	CandidateIDs  []string   `json:"candidate_ids"`
	Legal         bool       `json:"legal"`
	LegalAssembly []string   `json:"legal_assembly"`
	Conflicts     []Conflict `json:"conflicts"`
}

type SlotAllocation struct {
	SlotID          string   `json:"slot_id"`
	LogicalSlotID   string   `json:"logical_slot_id"`
	BatchSlotIndex  int      `json:"batch_slot_index"`
	MaterialID      string   `json:"material_id"`
	MaterialVersion string   `json:"material_version"`
	OwnerPartID     string   `json:"owner_part_id"`
	SharedWith      []string `json:"shared_with,omitempty"`
	RemappedFor     string   `json:"remapped_for,omitempty"`
}

type SubmeshPlacement struct {
	SubmeshID         string `json:"submesh_id"`
	MaterialSlotID    string `json:"material_slot_id"`
	BatchSlotIndex    int    `json:"batch_slot_index"`
	PartID            string `json:"part_id"`
	VertexBase        int    `json:"vertex_base"`
	SourceVertexStart int    `json:"source_vertex_start"`
	VertexStart       int    `json:"vertex_start"`
	VertexEnd         int    `json:"vertex_end"`
	SourceIndexStart  int    `json:"source_index_start"`
	IndexStart        int    `json:"index_start"`
	IndexEnd          int    `json:"index_end"`
	IndexCount        int    `json:"index_count"`
}

type PartPlacement struct {
	PartID     string             `json:"part_id"`
	VertexBase int                `json:"vertex_base"`
	IndexBase  int                `json:"index_base"`
	Submeshes  []SubmeshPlacement `json:"submeshes"`
}

type Batch struct {
	Number            int               `json:"batch_number"`
	SkinKey           string            `json:"skin_key"`
	PartIDs           []string          `json:"part_ids"`
	Slots             []SlotAllocation  `json:"slots"`
	VertexUsed        int               `json:"vertex_used"`
	IndexUsed         int               `json:"index_used"`
	Placements        []PartPlacement   `json:"placements"`
	Dependencies      []string          `json:"dependencies,omitempty"`
	PartHashes        map[string]string `json:"part_hashes,omitempty"`
	ContentHash       string            `json:"content_hash,omitempty"`
	CacheKey          string            `json:"cache_key,omitempty"`
	ConstraintVersion string            `json:"constraint_version,omitempty"`
	AlgorithmVersion  string            `json:"algorithm_version,omitempty"`
}

type Decision struct {
	Sequence    int      `json:"sequence"`
	BatchNumber int      `json:"batch_number"`
	CandidateID string   `json:"candidate_id"`
	IncumbentID string   `json:"incumbent_id,omitempty"`
	Kind        string   `json:"kind"`
	Result      string   `json:"result"`
	Basis       []string `json:"basis"`
}

type WaitRecord struct {
	Reason     string `json:"reason"`
	PartID     string `json:"part_id,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	DurationNS int64  `json:"duration_ns"`
	QueueDepth int    `json:"queue_depth,omitempty"`
	Capacity   int    `json:"capacity,omitempty"`
}

type PartFailure struct {
	PartID    string `json:"part_id"`
	Attempt   int    `json:"attempt"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Recovered bool   `json:"recovered,omitempty"`
}

type CommitRecord struct {
	Sequence          int    `json:"sequence"`
	BatchNumber       int    `json:"batch_number"`
	CacheKey          string `json:"cache_key"`
	ContentHash       string `json:"content_hash"`
	ConstraintVersion string `json:"constraint_version"`
	AlgorithmVersion  string `json:"algorithm_version"`
	CommittedAtNano   int64  `json:"committed_at_nano,omitempty"`
}

type Truth struct {
	SchemaVersion     string         `json:"schema_version"`
	Seed              int64          `json:"seed"`
	AlgorithmVersion  string         `json:"algorithm_version"`
	ConstraintVersion string         `json:"constraint_version"`
	SamplingPose      string         `json:"sampling_pose"`
	Assembly          AssemblyResult `json:"assembly"`
	Batches           []Batch        `json:"batches"`
	Decisions         []Decision     `json:"decisions"`
	CommitSequence    []int          `json:"commit_sequence"`
}

// Plan is the canonical result of assembly evaluation and batch planning.
type Plan struct {
	AlgorithmVersion string         `json:"algorithm_version"`
	Assembly         AssemblyResult `json:"assembly"`
	Batches          []Batch        `json:"batches"`
	Decisions        []Decision     `json:"decisions"`
	CommitSequence   []int          `json:"commit_sequence"`
}

// Report is the stable local diagnostic/demo output.
type Report struct {
	OK                 bool              `json:"ok"`
	Seed               int64             `json:"seed"`
	AlgorithmVersion   string            `json:"algorithm_version"`
	ConstraintVersion  string            `json:"constraint_version"`
	Assembly           AssemblyResult    `json:"assembly"`
	Batches            []Batch           `json:"batches"`
	Decisions          []Decision        `json:"decisions"`
	CommitSequence     []int             `json:"commit_sequence"`
	InvalidatedBatches []int             `json:"invalidated_batches,omitempty"`
	ReusedBatches      []int             `json:"reused_batches,omitempty"`
	WaitRecords        []WaitRecord      `json:"wait_records,omitempty"`
	Failures           []PartFailure     `json:"failures,omitempty"`
	CheckpointBatches  []int             `json:"checkpoint_batches,omitempty"`
	Comparison         *ComparisonReport `json:"comparison,omitempty"`
	SQLitePath         string            `json:"sqlite_path,omitempty"`
	CachePath          string            `json:"cache_path,omitempty"`
	Message            string            `json:"message,omitempty"`
}

type ComparisonReport struct {
	Equal         bool           `json:"equal"`
	Paths         []string       `json:"paths"`
	FirstMismatch *xerr.Mismatch `json:"first_mismatch,omitempty"`
}
