package engine

import (
	"fmt"
	"reflect"

	"storyboardstudio/internal/model"
)

type Mismatch struct {
	OK        bool   `json:"ok"`
	ShotIndex int    `json:"shot_index"`
	Field     string `json:"field,omitempty"`
	Left      any    `json:"left,omitempty"`
	Right     any    `json:"right,omitempty"`
	Message   string `json:"message,omitempty"`
}

func Match() *Mismatch {
	return &Mismatch{OK: true, ShotIndex: -1}
}

func CompareResults(left, right *model.RunResult) *Mismatch {
	if left == nil || right == nil {
		return mismatch(-1, "result", left != nil, right != nil, "one result is nil")
	}
	if m := compareStringSlices(-1, "event_order", left.EventOrder, right.EventOrder); m != nil {
		return m
	}
	if m := compareStringSlices(-1, "reachable_nodes", left.ReachableNodes, right.ReachableNodes); m != nil {
		return m
	}
	if m := compareFlags(left.TerminalFlags, right.TerminalFlags); m != nil {
		return m
	}
	if !reflect.DeepEqual(left.Characters, right.Characters) {
		return mismatch(-1, "characters", left.Characters, right.Characters, "character state differs")
	}
	if len(left.Shots) != len(right.Shots) {
		index := min(len(left.Shots), len(right.Shots))
		if index > 0 {
			index--
		}
		return mismatch(index, "shot_count", len(left.Shots), len(right.Shots), "presented shot counts differ")
	}
	for i := range left.Shots {
		if m := compareShot(i, left.Shots[i], right.Shots[i]); m != nil {
			return m
		}
	}
	return Match()
}

func CompareTruth(project *model.Project, result *model.RunResult) *Mismatch {
	if project == nil || result == nil {
		return mismatch(-1, "result", nil, nil, "project and result are required")
	}
	truth := project.Truth
	if m := compareFlags(result.TerminalFlags, truth.TerminalFlags); m != nil {
		return m
	}
	if m := compareStringSlices(-1, "event_order", result.EventOrder, truth.EventOrder); m != nil {
		return m
	}
	if m := compareStringSlices(-1, "reachable_nodes", result.ReachableNodes, truth.ReachableNodes); m != nil {
		return m
	}
	if len(result.Shots) != len(truth.ShotSamples) {
		index := min(len(result.Shots), len(truth.ShotSamples))
		if index > 0 {
			index--
		}
		return mismatch(index, "truth.shot_count", len(result.Shots), len(truth.ShotSamples), "shot count differs from truth")
	}
	for i := range truth.ShotSamples {
		shot := result.Shots[i]
		expected := truth.ShotSamples[i]
		if shot.GlobalIndex != expected.GlobalIndex {
			return mismatch(i, "global_index", shot.GlobalIndex, expected.GlobalIndex, "global shot index differs")
		}
		if shot.NodeID != expected.NodeID {
			return mismatch(i, "node_id", shot.NodeID, expected.NodeID, "shot owner differs")
		}
		if shot.ShotID != expected.ShotID {
			return mismatch(i, "shot_id", shot.ShotID, expected.ShotID, "shot id differs")
		}
		if len(shot.Samples) != len(expected.SamplesMS) {
			return mismatch(i, "samples.length", len(shot.Samples), len(expected.SamplesMS), "sample count differs")
		}
		for j, expectedMS := range expected.SamplesMS {
			actualMS := shot.Samples[j].LocalMS
			delta := actualMS - expectedMS
			if delta < 0 {
				delta = -delta
			}
			if delta > project.ToleranceMS {
				return mismatch(i, fmt.Sprintf("samples[%d].local_ms", j), actualMS, expectedMS,
					fmt.Sprintf("sample deviation %dms exceeds tolerance %dms", delta, project.ToleranceMS))
			}
		}
	}
	return Match()
}

func compareShot(index int, left, right model.PresentedShot) *Mismatch {
	fields := []struct {
		name  string
		left  any
		right any
	}{
		{"global_index", left.GlobalIndex, right.GlobalIndex},
		{"node_id", left.NodeID, right.NodeID},
		{"local_index", left.LocalIndex, right.LocalIndex},
		{"shot_id", left.ShotID, right.ShotID},
		{"duration_ms", left.DurationMS, right.DurationMS},
		{"aspect_path", left.AspectPath, right.AspectPath},
		{"camera", left.Camera, right.Camera},
		{"motion", left.Motion, right.Motion},
		{"transition", left.Transition, right.Transition},
		{"start_ms", left.StartMS, right.StartMS},
		{"end_ms", left.EndMS, right.EndMS},
		{"content_hash", left.ContentHash, right.ContentHash},
	}
	for _, field := range fields {
		if field.left != field.right {
			return mismatch(index, field.name, field.left, field.right, "field differs between paths")
		}
	}
	if len(left.Samples) != len(right.Samples) {
		return mismatch(index, "samples.length", len(left.Samples), len(right.Samples), "sample count differs")
	}
	for j := range left.Samples {
		a := left.Samples[j]
		b := right.Samples[j]
		sampleFields := []struct {
			name  string
			left  any
			right any
		}{
			{"local_ms", a.LocalMS, b.LocalMS},
			{"global_ms", a.GlobalMS, b.GlobalMS},
			{"node_id", a.NodeID, b.NodeID},
			{"shot_id", a.ShotID, b.ShotID},
		}
		for _, field := range sampleFields {
			if field.left != field.right {
				return mismatch(index, fmt.Sprintf("samples[%d].%s", j, field.name), field.left, field.right, "sample differs")
			}
		}
	}
	return nil
}

func compareStringSlices(shotIndex int, field string, left, right []string) *Mismatch {
	if len(left) != len(right) {
		return mismatch(shotIndex, field+".length", len(left), len(right), "slice length differs")
	}
	for i := range left {
		if left[i] != right[i] {
			return mismatch(shotIndex, fmt.Sprintf("%s[%d]", field, i), left[i], right[i], "slice item differs")
		}
	}
	return nil
}

func compareFlags(left, right map[string]bool) *Mismatch {
	keys := map[string]struct{}{}
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}
	for _, key := range model.SortedKeys(keys) {
		leftValue, leftOK := left[key]
		rightValue, rightOK := right[key]
		if !leftOK || !rightOK || leftValue != rightValue {
			return mismatch(-1, "terminal_flags."+key, fmt.Sprintf("%v(present=%v)", leftValue, leftOK),
				fmt.Sprintf("%v(present=%v)", rightValue, rightOK), "terminal flag differs")
		}
	}
	return nil
}

func mismatch(shotIndex int, field string, left, right any, message string) *Mismatch {
	return &Mismatch{OK: false, ShotIndex: shotIndex, Field: field, Left: left, Right: right, Message: message}
}
