package pipeline

import (
	"errors"

	"prosodyalign/internal/checkpoint"
	"prosodyalign/internal/dag"
	"prosodyalign/internal/errs"
	"prosodyalign/internal/model"
)

const AlgorithmVersion = "prosody-pipeline-v1.0.0"
const SnapshotID = "prosody-local-diagnostic-snapshot-v1"

var ErrSimulatedKill = errors.New("simulated forced termination after durable commit")

type PhonemeWeightEdit struct {
	DialogID     string `json:"dialog_id,omitempty"`
	PhonemeIndex int    `json:"phoneme_index"`
	WeightPer10k int64  `json:"weight_per_10k"`
}

type Edit struct {
	Description           string             `json:"description"`
	TargetDurationSamples *int64             `json:"target_duration_samples,omitempty"`
	DefaultMaxStretch     *int64             `json:"default_max_stretch_samples,omitempty"`
	AccentTolerance       *int64             `json:"accent_tolerance_samples,omitempty"`
	PhonemeWeight         *PhonemeWeightEdit `json:"phoneme_weight,omitempty"`
}

type InvalidationItem struct {
	DialogID      string `json:"dialog_id"`
	PhonemeIndex  *int   `json:"phoneme_index,omitempty"`
	NodeKey       string `json:"node_key"`
	NodeKind      string `json:"node_kind"`
	Reason        string `json:"reason"`
	Scope         string `json:"scope"`
	Reused        bool   `json:"reused"`
	ReusedFromKey string `json:"reused_from_key,omitempty"`
}

type InvalidationManifest struct {
	Epoch       int                `json:"epoch"`
	Edit        string             `json:"edit"`
	Items       []InvalidationItem `json:"items"`
	ReusedItems []InvalidationItem `json:"reused_items"`
}

type Outcome struct {
	FragmentID  string              `json:"fragment_id"`
	DialogID    string              `json:"dialog_id"`
	Ordinal     int                 `json:"ordinal"`
	OK          bool                `json:"ok"`
	Result      *model.DialogResult `json:"result,omitempty"`
	ErrorDetail *errs.Detail        `json:"error_detail,omitempty"`
}

type EpochOutput struct {
	Epoch              int                  `json:"epoch"`
	Edit               Edit                 `json:"edit"`
	OrderedFragmentIDs []string             `json:"ordered_fragment_ids"`
	Outcomes           []Outcome            `json:"outcomes"`
	Manifest           InvalidationManifest `json:"manifest"`
	GraphNodes         []dag.Node           `json:"graph_nodes,omitempty"`
	CommitSequence     []string             `json:"commit_sequence"`
}

type PathRun struct {
	Name             string                       `json:"name"`
	DBPath           string                       `json:"db_path,omitempty"`
	Epochs           []*EpochOutput               `json:"epochs"`
	CommitSequence   []string                     `json:"commit_sequence"`
	RecoveryBoundary *checkpoint.RecoveryBoundary `json:"recovery_boundary,omitempty"`
	Terminated       bool                         `json:"terminated,omitempty"`
}

type Mismatch struct {
	Epoch         int    `json:"epoch"`
	FragmentIndex int    `json:"fragment_index"`
	FragmentID    string `json:"fragment_id,omitempty"`
	FrameIndex    int    `json:"frame_index"`
	Field         string `json:"field"`
	Expected      any    `json:"expected,omitempty"`
	Actual        any    `json:"actual,omitempty"`
	Path          string `json:"path,omitempty"`
}

type ComparisonReport struct {
	AllEqual             bool       `json:"all_equal"`
	ComparedEpochs       int        `json:"compared_epochs"`
	FirstMismatch        *Mismatch  `json:"first_mismatch,omitempty"`
	Mismatches           []Mismatch `json:"mismatches"`
	CommitSequencesEqual bool       `json:"commit_sequences_equal"`
}
