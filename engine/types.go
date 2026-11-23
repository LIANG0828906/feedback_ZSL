package engine

import (
	"fmt"

	"storyworkbench/story"
)

const (
	AlgorithmVersion  = "story-engine/1"
	SaveSchemaVersion = 1
)

type ShotPresentation struct {
	Sequence       int      `json:"sequence"`
	NodeID         string   `json:"node_id"`
	NodeTitle      string   `json:"node_title"`
	NodeShotIndex  int      `json:"node_shot_index"`
	DurationMS     int64    `json:"duration_ms"`
	AspectPriority []string `json:"aspect_priority"`
	Description    string   `json:"description,omitempty"`
	PresentedAtMS  int64    `json:"presented_at_ms"`
	ChoiceIndex    int      `json:"choice_index"`
	ChoiceAtMS     int64    `json:"choice_at_ms"`
	ContentHash    string   `json:"content_hash"`
	StateHash      string   `json:"state_hash"`

	// These annotations are deliberately excluded from semantic comparison.
	// They let the workbench label reused and invalidated results without
	// rewriting the original presentation record.
	Reused              bool   `json:"reused,omitempty"`
	Invalidated         bool   `json:"invalidated,omitempty"`
	Recomputed          bool   `json:"recomputed,omitempty"`
	OriginalSequence    *int   `json:"original_sequence,omitempty"`
	OriginalContentHash string `json:"original_content_hash,omitempty"`
}

type NodeRuntime struct {
	Activated       bool `json:"activated"`
	Completed       bool `json:"completed"`
	ActivationOrder int  `json:"activation_order,omitempty"`
	ChoiceIndex     int  `json:"choice_index,omitempty"`
}

type BlockedChoice struct {
	ChoiceIndex int    `json:"choice_index"`
	AtMS        int64  `json:"at_ms"`
	Target      string `json:"target"`
	Reason      string `json:"reason"`
}

type NodeDurationStat struct {
	NodeID     string `json:"node_id"`
	ShotCount  int    `json:"shot_count"`
	DurationMS int64  `json:"duration_ms"`
}

type RunResult struct {
	ProjectID        string                    `json:"project_id"`
	ProjectTitle     string                    `json:"project_title"`
	SchemaVersion    int                       `json:"schema_version"`
	AlgorithmVersion string                    `json:"algorithm_version"`
	ContentHash      string                    `json:"content_hash"`
	Shots            []ShotPresentation        `json:"shots"`
	FinalFlags       map[string]any            `json:"final_flags"`
	FinalCharacters  map[string]map[string]any `json:"final_characters"`
	NodeStates       map[string]NodeRuntime    `json:"node_states"`
	ReachableNodes   []string                  `json:"reachable_nodes"`
	EventOrder       []string                  `json:"event_order"`
	BlockedChoices   []BlockedChoice           `json:"blocked_choices"`
	ChoiceOffset     int                       `json:"choice_offset"`
	ShotCount        int                       `json:"shot_count"`
	TotalDurationMS  int64                     `json:"total_duration_ms"`
	DurationStats    []NodeDurationStat        `json:"duration_stats"`
	FinalStateHash   string                    `json:"final_state_hash"`
	Done             bool                      `json:"done"`
}

func (r *RunResult) TotalDurationForNode(nodeID string) int64 {
	for _, stat := range r.DurationStats {
		if stat.NodeID == nodeID {
			return stat.DurationMS
		}
	}
	return 0
}

func (r *RunResult) ShotBySequence(sequence int) (*ShotPresentation, error) {
	if sequence < 0 || sequence >= len(r.Shots) {
		return nil, story.DivergeError(sequence, "", "sequence", fmt.Sprintf("shot sequence is outside [0,%d)", len(r.Shots)))
	}
	return &r.Shots[sequence], nil
}
