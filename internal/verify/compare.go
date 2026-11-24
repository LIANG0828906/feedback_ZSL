package verify

import (
	"fmt"
	"sort"

	"levelacoustic/internal/model"
)

func CompareExact(expected, actual *model.SolveResult) error {
	if expected == nil || actual == nil {
		return model.NewError(model.ErrDiff, "one side of comparison is nil")
	}
	if !sameStrings(expected.CommitSequence, actual.CommitSequence) {
		return &model.Error{
			Code:     model.ErrDiff,
			Message:  "room commit sequences differ",
			Field:    "commit_sequence",
			Expected: expected.CommitSequence,
			Actual:   actual.CommitSequence,
		}
	}
	expectedResults := resultMap(expected.ListenerResults)
	actualResults := resultMap(actual.ListenerResults)
	keys := unionKeys(expectedResults, actualResults)
	for _, key := range keys {
		want, okWant := expectedResults[key]
		got, okGot := actualResults[key]
		if !okWant || !okGot {
			room := want.ListenerRoomID
			if room == "" {
				room = got.ListenerRoomID
			}
			return &model.Error{
				Code:       model.ErrDiff,
				Message:    fmt.Sprintf("listener result %s is missing on one path", key),
				RoomID:     room,
				ListenerID: choose(want.ListenerID, got.ListenerID),
				Field:      "listener_result",
			}
		}
		if err := compareOne(want, got); err != nil {
			return err
		}
	}
	return nil
}

func CompareTruthTolerance(p *model.Project, result *model.SolveResult) error {
	if p.Truth == nil {
		return model.SchemaError("truth", "truth is required")
	}
	actualResults := resultMap(result.ListenerResults)
	for _, truth := range p.Truth.Entries {
		key := model.PairID(truth.SourceID, truth.ListenerID)
		got, ok := actualResults[key]
		if !ok {
			return &model.Error{
				Code:       model.ErrDiff,
				Message:    fmt.Sprintf("solver is missing truth pair %s", key),
				RoomID:     truth.ListenerRoomID,
				ListenerID: truth.ListenerID,
				Field:      "listener_result",
			}
		}
		if truth.Reachable != got.Reachable {
			return &model.Error{
				Code:            model.ErrChain,
				Message:         fmt.Sprintf("reachability differs for %s", key),
				SourceID:        truth.SourceID,
				ListenerID:      truth.ListenerID,
				FirstDivergence: first(truth.RoomChain),
				Field:           "reachable",
			}
		}
		if !truth.Reachable {
			continue
		}
		if idx, room := firstDifferent(truth.RoomChain, got.RoomChain); idx >= 0 {
			return &model.Error{
				Code:            model.ErrChain,
				Message:         fmt.Sprintf("room chain differs for %s at index %d", key, idx),
				RoomID:          truth.ListenerRoomID,
				SourceID:        truth.SourceID,
				ListenerID:      truth.ListenerID,
				FirstDivergence: room,
				Field:           fmt.Sprintf("room_chain[%d]", idx),
				Expected:        truth.RoomChain,
				Actual:          got.RoomChain,
			}
		}
		if !sameStrings(truth.DoorChain, got.DoorChain) {
			return &model.Error{
				Code:            model.ErrChain,
				Message:         fmt.Sprintf("door chain differs for %s", key),
				RoomID:          truth.ListenerRoomID,
				SourceID:        truth.SourceID,
				ListenerID:      truth.ListenerID,
				FirstDivergence: first(truth.RoomChain),
				Field:           "door_chain",
				Expected:        truth.DoorChain,
				Actual:          got.DoorChain,
			}
		}
		if absDiff(truth.PathLengthMicro, got.PathLengthMicro) > p.Tolerance.PathLengthMicro {
			return &model.Error{
				Code:            model.ErrChain,
				Message:         fmt.Sprintf("path length differs for %s beyond tolerance %d", key, p.Tolerance.PathLengthMicro),
				RoomID:          truth.ListenerRoomID,
				SourceID:        truth.SourceID,
				ListenerID:      truth.ListenerID,
				FirstDivergence: first(truth.RoomChain),
				Field:           "path_length_micro",
				Expected:        truth.PathLengthMicro,
				Actual:          got.PathLengthMicro,
			}
		}
		n := len(truth.CurveDBMicro)
		if len(got.CurveDBMicro) < n {
			n = len(got.CurveDBMicro)
		}
		for i := 0; i < n; i++ {
			if absDiff(truth.CurveDBMicro[i], got.CurveDBMicro[i]) > p.Tolerance.CurveDBMicro {
				return &model.Error{
					Code:       model.ErrDiff,
					Message:    fmt.Sprintf("curve sample %d differs for %s beyond tolerance %d", i, key, p.Tolerance.CurveDBMicro),
					RoomID:     truth.ListenerRoomID,
					ListenerID: truth.ListenerID,
					Field:      fmt.Sprintf("curve_db_micro[%d]", i),
					Expected:   truth.CurveDBMicro[i],
					Actual:     got.CurveDBMicro[i],
				}
			}
		}
		if len(truth.CurveDBMicro) != len(got.CurveDBMicro) {
			return &model.Error{
				Code:       model.ErrDiff,
				Message:    fmt.Sprintf("curve length differs for %s", key),
				RoomID:     truth.ListenerRoomID,
				ListenerID: truth.ListenerID,
				Field:      "curve_db_micro",
				Expected:   len(truth.CurveDBMicro),
				Actual:     len(got.CurveDBMicro),
			}
		}
		if absDiff(truth.SPLDBMicro, got.SPLDBMicro) > p.Tolerance.SPLDBMicro {
			return &model.Error{
				Code:       model.ErrDiff,
				Message:    fmt.Sprintf("SPL differs for %s beyond tolerance %d", key, p.Tolerance.SPLDBMicro),
				RoomID:     truth.ListenerRoomID,
				ListenerID: truth.ListenerID,
				Field:      "spl_db_micro",
				Expected:   truth.SPLDBMicro,
				Actual:     got.SPLDBMicro,
			}
		}
		if absDiff(truth.OcclusionDBMicro, got.OcclusionDBMicro) > p.Tolerance.SPLDBMicro {
			return &model.Error{
				Code:       model.ErrDiff,
				Message:    fmt.Sprintf("occlusion differs for %s beyond tolerance %d", key, p.Tolerance.SPLDBMicro),
				RoomID:     truth.ListenerRoomID,
				ListenerID: truth.ListenerID,
				Field:      "occlusion_db_micro",
				Expected:   truth.OcclusionDBMicro,
				Actual:     got.OcclusionDBMicro,
			}
		}
	}
	return nil
}

type ToleranceCount struct {
	Tolerance int64 `json:"tolerance"`
	Failed    int   `json:"failed_listeners"`
	Passed    int   `json:"passed_listeners"`
}

// ToleranceSweep returns listener-level failure counts for tolerances ordered
// from strict to loose. As the tolerance loosens, the failed-listener count is
// monotonically non-increasing (equivalently, the passed count is
// non-decreasing).
func ToleranceSweep(p *model.Project, result *model.SolveResult, tolerances []int64) []ToleranceCount {
	actual := resultMap(result.ListenerResults)
	byListener := make(map[string][]model.TruthEntry)
	for _, truth := range p.Truth.Entries {
		byListener[truth.ListenerID] = append(byListener[truth.ListenerID], truth)
	}
	listenerIDs := make([]string, 0, len(byListener))
	for id := range byListener {
		listenerIDs = append(listenerIDs, id)
	}
	sort.Strings(listenerIDs)
	out := make([]ToleranceCount, 0, len(tolerances))
	for _, tol := range tolerances {
		failed := 0
		for _, listenerID := range listenerIDs {
			if listenerFailsTolerance(byListener[listenerID], actual, tol) {
				failed++
			}
		}
		out = append(out, ToleranceCount{
			Tolerance: tol,
			Failed:    failed,
			Passed:    len(listenerIDs) - failed,
		})
	}
	return out
}

func listenerFailsTolerance(truths []model.TruthEntry, actual map[string]model.ListenerResult, tol int64) bool {
	for _, truth := range truths {
		key := model.PairID(truth.SourceID, truth.ListenerID)
		got, ok := actual[key]
		if !ok || truth.Reachable != got.Reachable {
			return true
		}
		if !truth.Reachable {
			continue
		}
		if idx, _ := firstDifferent(truth.RoomChain, got.RoomChain); idx >= 0 {
			return true
		}
		if absDiff(truth.PathLengthMicro, got.PathLengthMicro) > tol {
			return true
		}
		if len(truth.CurveDBMicro) != len(got.CurveDBMicro) {
			return true
		}
		for i := range truth.CurveDBMicro {
			if absDiff(truth.CurveDBMicro[i], got.CurveDBMicro[i]) > tol {
				return true
			}
		}
		if absDiff(truth.SPLDBMicro, got.SPLDBMicro) > tol {
			return true
		}
	}
	return false
}

func compareOne(want, got model.ListenerResult) error {
	base := func(field string) *model.Error {
		return &model.Error{
			Code:       model.ErrDiff,
			Message:    fmt.Sprintf("listener %s/%s differs in %s", want.SourceID, want.ListenerID, field),
			RoomID:     want.ListenerRoomID,
			ListenerID: want.ListenerID,
			Field:      field,
		}
	}
	if want.ListenerRoomID != got.ListenerRoomID {
		e := base("listener_room_id")
		e.Expected, e.Actual = want.ListenerRoomID, got.ListenerRoomID
		return e
	}
	if want.Reachable != got.Reachable {
		e := base("reachable")
		e.Expected, e.Actual = want.Reachable, got.Reachable
		return e
	}
	if !sameStrings(want.RoomChain, got.RoomChain) {
		e := base("room_chain")
		e.Expected, e.Actual = want.RoomChain, got.RoomChain
		return e
	}
	if !sameStrings(want.DoorChain, got.DoorChain) {
		e := base("door_chain")
		e.Expected, e.Actual = want.DoorChain, got.DoorChain
		return e
	}
	if want.PathLengthMicro != got.PathLengthMicro {
		e := base("path_length_micro")
		e.Expected, e.Actual = want.PathLengthMicro, got.PathLengthMicro
		return e
	}
	if len(want.CurveDBMicro) != len(got.CurveDBMicro) {
		e := base("curve_db_micro.length")
		e.Expected, e.Actual = len(want.CurveDBMicro), len(got.CurveDBMicro)
		return e
	}
	for i := range want.CurveDBMicro {
		if want.CurveDBMicro[i] != got.CurveDBMicro[i] {
			e := base(fmt.Sprintf("curve_db_micro[%d]", i))
			e.Expected, e.Actual = want.CurveDBMicro[i], got.CurveDBMicro[i]
			return e
		}
	}
	if want.SPLDBMicro != got.SPLDBMicro {
		e := base("spl_db_micro")
		e.Expected, e.Actual = want.SPLDBMicro, got.SPLDBMicro
		return e
	}
	if want.OcclusionDBMicro != got.OcclusionDBMicro {
		e := base("occlusion_db_micro")
		e.Expected, e.Actual = want.OcclusionDBMicro, got.OcclusionDBMicro
		return e
	}
	if len(want.Segments) != len(got.Segments) {
		e := base("segments.length")
		e.Expected, e.Actual = len(want.Segments), len(got.Segments)
		return e
	}
	for i := range want.Segments {
		if want.Segments[i] != got.Segments[i] {
			e := base(fmt.Sprintf("segments[%d]", i))
			e.Expected, e.Actual = want.Segments[i], got.Segments[i]
			return e
		}
	}
	if len(want.Truncations) != len(got.Truncations) {
		e := base("truncations.length")
		e.Expected, e.Actual = len(want.Truncations), len(got.Truncations)
		return e
	}
	for i := range want.Truncations {
		if want.Truncations[i] != got.Truncations[i] {
			e := base(fmt.Sprintf("truncations[%d]", i))
			e.Expected, e.Actual = want.Truncations[i], got.Truncations[i]
			return e
		}
	}
	return nil
}

func resultMap(results []model.ListenerResult) map[string]model.ListenerResult {
	out := make(map[string]model.ListenerResult, len(results))
	for _, result := range results {
		out[model.PairID(result.SourceID, result.ListenerID)] = result
	}
	return out
}

func unionKeys(a, b map[string]model.ListenerResult) []string {
	set := make(map[string]bool, len(a)+len(b))
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstDifferent(a, b []string) (int, string) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i, a[i]
		}
	}
	if len(a) != len(b) {
		if len(a) > n {
			return n, a[n]
		}
		return n, b[n]
	}
	return -1, ""
}

func sameStrings(a, b []string) bool {
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

func absDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func choose(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
