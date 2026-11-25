package acoustic

import (
	"encoding/json"
	"fmt"
)

type RunDifference struct {
	Field    string `json:"field"`
	Expected any    `json:"expected"`
	Actual   any    `json:"actual"`
}

func CompareRunResults(expected, actual *RunResult) (bool, []RunDifference) {
	if expected == nil || actual == nil {
		return false, []RunDifference{{Field: "result", Expected: expected != nil, Actual: actual != nil}}
	}
	var differences []RunDifference
	if !equalIntSlices(expected.CommitSequence, actual.CommitSequence) {
		differences = append(differences, RunDifference{Field: "commitSequence", Expected: expected.CommitSequence, Actual: actual.CommitSequence})
	}
	if len(expected.Snapshots) != len(actual.Snapshots) {
		differences = append(differences, RunDifference{Field: "snapshotCount", Expected: len(expected.Snapshots), Actual: len(actual.Snapshots)})
		return len(differences) == 0, differences
	}
	for i := range expected.Snapshots {
		left, right := expected.Snapshots[i], actual.Snapshots[i]
		if left.RoomID != right.RoomID || left.CommitSequence != right.CommitSequence || left.ContentHash != right.ContentHash {
			differences = append(differences, RunDifference{
				Field:    fmt.Sprintf("snapshot[%d]", i),
				Expected: []any{left.RoomID, left.CommitSequence, left.ContentHash},
				Actual:   []any{right.RoomID, right.CommitSequence, right.ContentHash},
			})
		}
		if !jsonEqual(left.ChainSummaries, right.ChainSummaries) {
			differences = append(differences, RunDifference{Field: fmt.Sprintf("snapshot[%d].chains", i), Expected: left.ChainSummaries, Actual: right.ChainSummaries})
		}
		if !jsonEqual(left.ListenerSolutions, right.ListenerSolutions) {
			differences = append(differences, RunDifference{Field: fmt.Sprintf("snapshot[%d].solutions", i), Expected: left.ListenerSolutions, Actual: right.ListenerSolutions})
		}
	}
	if expected.Progress != actual.Progress {
		differences = append(differences, RunDifference{Field: "progress", Expected: expected.Progress, Actual: actual.Progress})
	}
	return len(differences) == 0, differences
}

func equalIntSlices(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func jsonEqual(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(left) == string(right)
}
