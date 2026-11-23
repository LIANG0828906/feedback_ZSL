package engine

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"storyworkbench/story"
)

// CompareRuns performs the acceptance comparison at shot and final-state
// granularity. Presentation annotations (reused/invalidated/recomputed) are
// intentionally ignored because they are UI provenance rather than playback.
func CompareRuns(expected, actual *RunResult) error {
	if expected == nil || actual == nil {
		return story.DivergeError(-1, "", "result", "one of the run results is nil")
	}
	if expected.ProjectID != actual.ProjectID {
		return story.DivergeError(-1, "", "project_id", fmt.Sprintf("expected %q, got %q", expected.ProjectID, actual.ProjectID))
	}
	if expected.ContentHash != actual.ContentHash {
		return story.DivergeError(-1, "", "content_hash", "run content hash differs")
	}
	if expected.AlgorithmVersion != actual.AlgorithmVersion {
		return story.DivergeError(-1, "", "algorithm_version", fmt.Sprintf("expected %q, got %q", expected.AlgorithmVersion, actual.AlgorithmVersion))
	}
	minShots := len(expected.Shots)
	if len(actual.Shots) < minShots {
		minShots = len(actual.Shots)
	}
	for i := 0; i < minShots; i++ {
		a := expected.Shots[i]
		b := actual.Shots[i]
		if a.NodeID != b.NodeID {
			return story.DivergeError(i, a.NodeID, "node_id", fmt.Sprintf("expected %q, got %q", a.NodeID, b.NodeID))
		}
		if a.NodeTitle != b.NodeTitle {
			return story.DivergeError(i, a.NodeID, "node_title", fmt.Sprintf("expected %q, got %q", a.NodeTitle, b.NodeTitle))
		}
		if a.NodeShotIndex != b.NodeShotIndex {
			return story.DivergeError(i, a.NodeID, "node_shot_index", fmt.Sprintf("expected %d, got %d", a.NodeShotIndex, b.NodeShotIndex))
		}
		if a.DurationMS != b.DurationMS {
			return story.DivergeError(i, a.NodeID, "duration_ms", fmt.Sprintf("expected %d, got %d", a.DurationMS, b.DurationMS))
		}
		if !sameStrings(a.AspectPriority, b.AspectPriority) {
			return story.DivergeError(i, a.NodeID, "aspect_priority", fmt.Sprintf("expected %v, got %v", a.AspectPriority, b.AspectPriority))
		}
		if a.Description != b.Description {
			return story.DivergeError(i, a.NodeID, "description", fmt.Sprintf("expected %q, got %q", a.Description, b.Description))
		}
		if a.PresentedAtMS != b.PresentedAtMS {
			return story.DivergeError(i, a.NodeID, "presented_at_ms", fmt.Sprintf("expected %d, got %d", a.PresentedAtMS, b.PresentedAtMS))
		}
		if a.ChoiceIndex != b.ChoiceIndex {
			return story.DivergeError(i, a.NodeID, "choice_index", fmt.Sprintf("expected %d, got %d", a.ChoiceIndex, b.ChoiceIndex))
		}
		if a.ChoiceAtMS != b.ChoiceAtMS {
			return story.DivergeError(i, a.NodeID, "choice_at_ms", fmt.Sprintf("expected %d, got %d", a.ChoiceAtMS, b.ChoiceAtMS))
		}
		if a.ContentHash != b.ContentHash {
			return story.DivergeError(i, a.NodeID, "content_hash", fmt.Sprintf("expected %s, got %s", a.ContentHash, b.ContentHash))
		}
		if a.StateHash != b.StateHash {
			return story.DivergeError(i, a.NodeID, "state_hash", fmt.Sprintf("expected %s, got %s", a.StateHash, b.StateHash))
		}
	}
	if len(expected.Shots) != len(actual.Shots) {
		seq := minShots
		nodeID := ""
		if seq < len(expected.Shots) {
			nodeID = expected.Shots[seq].NodeID
		} else if seq < len(actual.Shots) {
			nodeID = actual.Shots[seq].NodeID
		}
		return story.DivergeError(seq, nodeID, "shot_count",
			fmt.Sprintf("expected %d shots, got %d", len(expected.Shots), len(actual.Shots)))
	}
	lastSeq := len(expected.Shots) - 1
	lastNode := ""
	if lastSeq >= 0 {
		lastNode = expected.Shots[lastSeq].NodeID
	}
	if err := compareValueMaps("final_flags", expected.FinalFlags, actual.FinalFlags); err != nil {
		return story.DivergeError(lastSeq, lastNode, err.field, err.message)
	}
	if err := compareCharacterMaps(expected.FinalCharacters, actual.FinalCharacters); err != nil {
		return story.DivergeError(lastSeq, lastNode, err.field, err.message)
	}
	if !reflect.DeepEqual(expected.EventOrder, actual.EventOrder) {
		return story.DivergeError(lastSeq, lastNode, "event_order",
			fmt.Sprintf("expected %v, got %v", expected.EventOrder, actual.EventOrder))
	}
	if !reflect.DeepEqual(expected.ReachableNodes, actual.ReachableNodes) {
		return story.DivergeError(lastSeq, lastNode, "reachable_nodes",
			fmt.Sprintf("expected %v, got %v", expected.ReachableNodes, actual.ReachableNodes))
	}
	if !reflect.DeepEqual(expected.BlockedChoices, actual.BlockedChoices) {
		return story.DivergeError(lastSeq, lastNode, "blocked_choices",
			fmt.Sprintf("expected %v, got %v", expected.BlockedChoices, actual.BlockedChoices))
	}
	if expected.TotalDurationMS != actual.TotalDurationMS {
		return story.DivergeError(lastSeq, lastNode, "total_duration_ms",
			fmt.Sprintf("expected %d, got %d", expected.TotalDurationMS, actual.TotalDurationMS))
	}
	if !reflect.DeepEqual(expected.DurationStats, actual.DurationStats) {
		return story.DivergeError(lastSeq, lastNode, "duration_stats",
			fmt.Sprintf("expected %v, got %v", expected.DurationStats, actual.DurationStats))
	}
	if err := compareNodeStates(expected.NodeStates, actual.NodeStates, lastSeq); err != nil {
		return err
	}
	if expected.FinalStateHash != actual.FinalStateHash {
		return story.DivergeError(lastSeq, lastNode, "final_state_hash",
			fmt.Sprintf("expected %s, got %s", expected.FinalStateHash, actual.FinalStateHash))
	}
	if expected.ChoiceOffset != actual.ChoiceOffset {
		return story.DivergeError(lastSeq, lastNode, "choice_offset",
			fmt.Sprintf("expected %d, got %d", expected.ChoiceOffset, actual.ChoiceOffset))
	}
	if expected.Done != actual.Done {
		return story.DivergeError(lastSeq, lastNode, "done",
			fmt.Sprintf("expected %v, got %v", expected.Done, actual.Done))
	}
	return nil
}

func compareNodeStates(expected, actual map[string]NodeRuntime, lastSeq int) error {
	ids := make(map[string]bool, len(expected)+len(actual))
	for id := range expected {
		ids[id] = true
	}
	for id := range actual {
		ids[id] = true
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		a, aok := expected[id]
		b, bok := actual[id]
		if aok != bok {
			return story.DivergeError(lastSeq, id, "node_states", fmt.Sprintf("node presence differs: expected=%v actual=%v", aok, bok))
		}
		if a.Activated != b.Activated {
			return story.DivergeError(lastSeq, id, "node_states.activated", fmt.Sprintf("expected %v, got %v", a.Activated, b.Activated))
		}
		if a.Completed != b.Completed {
			return story.DivergeError(lastSeq, id, "node_states.completed", fmt.Sprintf("expected %v, got %v", a.Completed, b.Completed))
		}
		if a.ActivationOrder != b.ActivationOrder {
			return story.DivergeError(lastSeq, id, "node_states.activation_order", fmt.Sprintf("expected %d, got %d", a.ActivationOrder, b.ActivationOrder))
		}
		if a.ChoiceIndex != b.ChoiceIndex {
			return story.DivergeError(lastSeq, id, "node_states.choice_index", fmt.Sprintf("expected %d, got %d", a.ChoiceIndex, b.ChoiceIndex))
		}
	}
	return nil
}

type fieldMismatch struct {
	field   string
	message string
}

func compareValueMaps(prefix string, expected, actual map[string]any) *fieldMismatch {
	keys := make(map[string]bool, len(expected)+len(actual))
	for k := range expected {
		keys[k] = true
	}
	for k := range actual {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		a, aok := expected[key]
		b, bok := actual[key]
		if aok != bok {
			return &fieldMismatch{field: prefix + "." + key, message: fmt.Sprintf("presence differs: expected_present=%v actual_present=%v", aok, bok)}
		}
		if !story.ScalarEqual(a, b) {
			return &fieldMismatch{field: prefix + "." + key, message: fmt.Sprintf("expected %v (%T), got %v (%T)", a, a, b, b)}
		}
	}
	return nil
}

func compareCharacterMaps(expected, actual map[string]map[string]any) *fieldMismatch {
	characters := make(map[string]bool, len(expected)+len(actual))
	for id := range expected {
		characters[id] = true
	}
	for id := range actual {
		characters[id] = true
	}
	ordered := make([]string, 0, len(characters))
	for id := range characters {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		a, aok := expected[id]
		b, bok := actual[id]
		if aok != bok {
			return &fieldMismatch{field: "final_characters." + id, message: fmt.Sprintf("presence differs: expected_present=%v actual_present=%v", aok, bok)}
		}
		if mismatch := compareValueMaps("final_characters."+id, a, b); mismatch != nil {
			return mismatch
		}
	}
	return nil
}

func FormatDiverge(err error) string {
	appErr, ok := story.AsError(err)
	if !ok {
		return err.Error()
	}
	parts := []string{appErr.Code}
	if appErr.ShotSequence != nil {
		parts = append(parts, fmt.Sprintf("shot=%d", *appErr.ShotSequence))
	}
	if appErr.NodeID != "" {
		parts = append(parts, "node="+appErr.NodeID)
	}
	if appErr.Field != "" {
		parts = append(parts, "field="+appErr.Field)
	}
	return strings.Join(parts, " ")
}
