package macrokit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

const (
	FormatFlatFrameV1 = 1
	FormatNestedMSV2  = 2
	FormatTreeFrameV3 = 3

	FormatVersionV1 = "flat-frame-v1.0"
	FormatVersionV2 = "nested-ms-v2.0"
	FormatVersionV3 = "tree-frame-v3.0"
)

type RoundingDirection string

const (
	RoundFloor RoundingDirection = "floor"
	RoundCeil  RoundingDirection = "ceil"
)

// Project is the source-format-independent macro orchestration model.
// All time fields are integer frames after loading.
type Project struct {
	FormatNo      int                `json:"format_no"`
	FormatName    string             `json:"format_name"`
	FormatVersion string             `json:"format_version"`
	SourcePath    string             `json:"source_path,omitempty"`
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Seed          int64              `json:"seed,omitempty"`
	FrameRate     int                `json:"frame_rate"`
	TimePolicy    TimePolicy         `json:"time_policy"`
	Flags         map[string]bool    `json:"flags,omitempty"`
	Branches      []*Branch          `json:"branches"`
	BranchByID    map[string]*Branch `json:"-"`
}

type TimePolicy struct {
	FrameToMS RoundingDirection `json:"frame_to_ms"`
	MSToFrame RoundingDirection `json:"ms_to_frame"`
}

type Branch struct {
	ID                  string     `json:"id"`
	MacroID             string     `json:"macro_id"`
	Name                string     `json:"name,omitempty"`
	Priority            int        `json:"priority"`
	HasPriority         bool       `json:"has_priority"`
	DelayFrames         int64      `json:"delay_frames"`
	TriggerWindowFrames int64      `json:"trigger_window_frames"`
	Condition           *Condition `json:"condition"`
	CancelKeys          []string   `json:"cancel_keys,omitempty"`
	Steps               []*Step    `json:"steps"`
}

type ConditionKind string

const (
	CondAll      ConditionKind = "all"
	CondAny      ConditionKind = "any"
	CondNot      ConditionKind = "not"
	CondKey      ConditionKind = "key"
	CondFlag     ConditionKind = "flag"
	CondBranch   ConditionKind = "branch"
	CondConstant ConditionKind = "constant"
)

type KeyState string

const (
	KeyDown    KeyState = "down"
	KeyPressed KeyState = "pressed"
)

// Condition is the normalized nested tree. Flat constant tables are resolved
// into this same shape while retaining their row ID in Ref.
type Condition struct {
	Kind     ConditionKind `json:"kind"`
	Ref      string        `json:"ref,omitempty"`
	Constant string        `json:"constant_id,omitempty"`
	Value    *bool         `json:"value,omitempty"`

	Key      string   `json:"key,omitempty"`
	KeyState KeyState `json:"key_state,omitempty"`

	Flag string `json:"flag,omitempty"`

	BranchID string `json:"branch_id,omitempty"`
	Outcome  string `json:"outcome,omitempty"`

	Children []*Condition `json:"children,omitempty"`
}

type Step struct {
	ID         string `json:"id"`
	Action     string `json:"action"`
	Lane       string `json:"lane,omitempty"`
	GapFrames  int64  `json:"gap_frames"`
	HoldFrames int64  `json:"hold_frames"`
}

type KeyEvent struct {
	Frame int64  `json:"frame"`
	Key   string `json:"key"`
	Kind  string `json:"kind"` // down or up
}

type EventStream struct {
	FrameCount int64      `json:"frame_count"`
	Events     []KeyEvent `json:"events"`
}

type FrameInput struct {
	Frame  int64      `json:"frame"`
	Events []KeyEvent `json:"events,omitempty"`
}

type KeySnapshot struct {
	Frame          int64            `json:"frame"`
	Down           map[string]bool  `json:"down"`
	Pressed        map[string]bool  `json:"pressed"`
	Released       map[string]bool  `json:"released"`
	PressedAt      map[string]int64 `json:"pressed_at,omitempty"`
	ConsumedOffset int              `json:"consumed_offset"`
}

type StepExecution struct {
	Frame     int64  `json:"frame"`
	BranchID  string `json:"branch_id"`
	MacroID   string `json:"macro_id"`
	StepIndex int    `json:"step_index"`
	StepID    string `json:"step_id"`
	Action    string `json:"action"`
	Lane      string `json:"lane,omitempty"`
}

type DelayState struct {
	BranchID  string `json:"branch_id"`
	StepID    string `json:"step_id,omitempty"`
	Kind      string `json:"kind"` // branch_delay or step_gap
	Total     int64  `json:"total_frames"`
	Remaining int64  `json:"remaining_frames"`
}

type Cancellation struct {
	Frame        int64  `json:"frame"`
	BranchID     string `json:"branch_id"`
	MacroID      string `json:"macro_id"`
	Reason       string `json:"reason"`
	SuppressedBy string `json:"suppressed_by,omitempty"`
}

type Adjudication struct {
	Frame             int64    `json:"frame"`
	WinnerID          string   `json:"winner_id"`
	WinnerMacroID     string   `json:"winner_macro_id"`
	SuppressedID      string   `json:"suppressed_id"`
	SuppressedMacroID string   `json:"suppressed_macro_id"`
	Basis             string   `json:"basis"`
	ConflictSet       []string `json:"conflict_set,omitempty"`
}

type FrameTrace struct {
	Frame             int64           `json:"frame"`
	Triggered         []string        `json:"triggered_branches,omitempty"`
	ExecutedSteps     []StepExecution `json:"executed_steps,omitempty"`
	RemainingDelays   []DelayState    `json:"remaining_delays,omitempty"`
	Cancellations     []Cancellation  `json:"cancellations,omitempty"`
	Adjudications     []Adjudication  `json:"adjudications,omitempty"`
	ConsumedOffset    int             `json:"consumed_offset"`
	ConditionSnapshot map[string]bool `json:"condition_snapshot,omitempty"`
	SnapshotHash      string          `json:"snapshot_hash"`
}

type BranchStatus string

const (
	StatusTriggered  BranchStatus = "triggered"
	StatusRunning    BranchStatus = "running"
	StatusCompleted  BranchStatus = "completed"
	StatusCanceled   BranchStatus = "canceled"
	StatusSuppressed BranchStatus = "suppressed"
)

type BranchState struct {
	BranchID      string          `json:"branch_id"`
	MacroID       string          `json:"macro_id"`
	Status        BranchStatus    `json:"status"`
	Priority      int             `json:"priority"`
	TriggerFrame  int64           `json:"trigger_frame"`
	CommitFrame   int64           `json:"commit_frame,omitempty"`
	ExecutedSteps []StepExecution `json:"executed_steps,omitempty"`
	CancelReason  string          `json:"cancel_reason,omitempty"`
}

type CommitRecord struct {
	Frame    int64        `json:"frame"`
	BranchID string       `json:"branch_id"`
	Status   BranchStatus `json:"status"`
}

type RunResult struct {
	ProjectID     string                  `json:"project_id"`
	FormatNo      int                     `json:"format_no"`
	FormatVersion string                  `json:"format_version"`
	FrameCount    int64                   `json:"frame_count"`
	Frames        []FrameTrace            `json:"frames"`
	Adjudications []Adjudication          `json:"adjudications,omitempty"`
	Commits       []CommitRecord          `json:"commits,omitempty"`
	Branches      map[string]*BranchState `json:"branches,omitempty"`
}

func (p *Project) Index() {
	p.BranchByID = make(map[string]*Branch, len(p.Branches))
	for _, b := range p.Branches {
		p.BranchByID[b.ID] = b
	}
}

func (p *Project) SortedBranches() []*Branch {
	out := append([]*Branch(nil), p.Branches...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (b *Branch) EffectivePriority() int {
	if b.HasPriority {
		return b.Priority
	}
	return 100
}

// CanonicalJSON emits deterministic JSON for hashing and persistence.
func CanonicalJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func HashCanonical(v any) string {
	b, err := CanonicalJSON(v)
	if err != nil {
		// All persisted domain values are JSON-safe. A failure here is a
		// programming error rather than a user-input error.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashSnapshot(s KeySnapshot) string {
	return HashCanonical(struct {
		Frame     int64            `json:"frame"`
		Down      map[string]bool  `json:"down"`
		Pressed   map[string]bool  `json:"pressed"`
		Released  map[string]bool  `json:"released"`
		PressedAt map[string]int64 `json:"pressed_at"`
		Offset    int              `json:"offset"`
	}{s.Frame, s.Down, s.Pressed, s.Released, s.PressedAt, s.ConsumedOffset})
}
