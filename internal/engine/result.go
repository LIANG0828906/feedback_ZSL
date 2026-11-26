package engine

import (
	"screenrelay/internal/dispatch"
	"screenrelay/internal/mapping"
	"screenrelay/internal/model"
	"screenrelay/internal/topology"
)

type BudgetConfig struct {
	MaxParallel           int   `json:"max_parallel"`
	BufferPixelsPerThread int64 `json:"buffer_pixels_per_thread"`
}

func DefaultBudget() BudgetConfig {
	return BudgetConfig{MaxParallel: 2, BufferPixelsPerThread: 65536}
}

type BudgetRecord struct {
	WindowSeq    int    `json:"window_seq"`
	ScreenID     string `json:"screen_id"`
	Wave         int    `json:"wave"`
	Slot         int    `json:"slot"`
	Queued       bool   `json:"queued"`
	Reason       string `json:"reason"`
	BufferPixels int64  `json:"buffer_pixels"`
	MaxParallel  int    `json:"max_parallel"`
}

type ReplayBundle struct {
	TargetWindowSeq int               `json:"target_window_seq"`
	SourceWindowSeq int               `json:"source_window_seq"`
	ScreenID        string            `json:"screen_id"`
	Packets         []dispatch.Packet `json:"packets"`
}

type ScreenResult struct {
	ScreenID         string                   `json:"screen_id"`
	JoinedThisWindow bool                     `json:"joined_this_window"`
	ScreenState      string                   `json:"screen_state"`
	Mapping          *mapping.Result          `json:"mapping,omitempty"`
	Dispatch         *dispatch.ScreenDispatch `json:"dispatch,omitempty"`
	Replay           *ReplayBundle            `json:"replay,omitempty"`
	Budget           BudgetRecord             `json:"budget"`
}

type WindowResult struct {
	Seq             int                 `json:"seq"`
	ID              string              `json:"id"`
	Tick            int64               `json:"tick"`
	State           string              `json:"state"`
	ActiveScreenIDs []string            `json:"active_screen_ids"`
	ScreenResults   []ScreenResult      `json:"screen_results"`
	Decisions       []topology.Decision `json:"decisions"`
	ContentHash     string              `json:"content_hash"`
	Budget          []BudgetRecord      `json:"budget"`
}

type RunResult struct {
	SessionID        string              `json:"session_id"`
	Seed             int64               `json:"seed"`
	SourceVersion    string              `json:"source_version"`
	SchemaMajor      int                 `json:"schema_major"`
	AlgorithmVersion string              `json:"algorithm_version"`
	Windows          []WindowResult      `json:"windows"`
	Decisions        []topology.Decision `json:"decisions"`
	FinalScreenIDs   []string            `json:"final_screen_ids"`
	CommitSequence   []int               `json:"commit_sequence"`
	SessionState     string              `json:"session_state"`
}

type PersistentState struct {
	Result             RunResult               `json:"result"`
	Screens            map[string]model.Screen `json:"screens"`
	AppliedEventIDs    map[string]bool         `json:"applied_event_ids"`
	PrevCheckpointHash string                  `json:"prev_checkpoint_hash"`
	CompletedWindows   int                     `json:"completed_windows"`
}
