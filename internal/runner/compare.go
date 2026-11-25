package runner

import (
	"fmt"

	"obsproj/internal/kernel"
	"obsproj/internal/model"
)

func CompareResults(left, right model.RunResult) *model.KernelError {
	if len(left.Commits) != len(right.Commits) {
		window := 0
		if len(left.Commits) > len(right.Commits) {
			window = left.Commits[len(right.Commits)].Window
		} else if len(right.Commits) > len(left.Commits) {
			window = right.Commits[len(left.Commits)].Window
		}
		return diffError(window, "", "window_sequence", fmt.Sprintf("%d", len(left.Commits)), fmt.Sprintf("%d", len(right.Commits)))
	}
	for i := range left.Commits {
		if err := compareCommit(left.Commits[i], right.Commits[i]); err != nil {
			return err
		}
	}
	if err := compareSnapshot(left.Final, right.Final); err != nil {
		return err
	}
	return nil
}

func compareCommit(left, right model.WindowCommit) *model.KernelError {
	window := left.Window
	if left.Window != right.Window {
		return diffError(window, "", "window", fmt.Sprintf("%d", left.Window), fmt.Sprintf("%d", right.Window))
	}
	if left.At != right.At {
		return diffError(window, "", "at_tick", fmt.Sprintf("%d", left.At), fmt.Sprintf("%d", right.At))
	}
	if left.SourceRegion != right.SourceRegion {
		return diffError(window, "", "source_region", fmt.Sprint(left.SourceRegion), fmt.Sprint(right.SourceRegion))
	}
	if err := compareStrings(window, "", "active_screens", left.ActiveScreens, right.ActiveScreens); err != nil {
		return err
	}
	if left.TopologyVersion != right.TopologyVersion {
		return diffError(window, "", "topology_version", fmt.Sprintf("%d", left.TopologyVersion), fmt.Sprintf("%d", right.TopologyVersion))
	}
	if left.SessionHash != right.SessionHash {
		return diffError(window, "", "session_hash", left.SessionHash, right.SessionHash)
	}
	if left.AlgorithmVersion != right.AlgorithmVersion {
		return diffError(window, "", "algorithm_version", left.AlgorithmVersion, right.AlgorithmVersion)
	}
	if err := compareDecisions(window, left.Decisions, right.Decisions); err != nil {
		return err
	}
	if len(left.Screens) != len(right.Screens) {
		return diffError(window, "", "screens", fmt.Sprintf("%d", len(left.Screens)), fmt.Sprintf("%d", len(right.Screens)))
	}
	for i := range left.Screens {
		if err := compareScreen(left.Screens[i], right.Screens[i]); err != nil {
			return err
		}
	}
	if err := compareSnapshot(left.Snapshot, right.Snapshot); err != nil {
		return err
	}
	if left.ContentHash != right.ContentHash {
		return diffError(window, "", "content_hash", left.ContentHash, right.ContentHash)
	}
	return nil
}

func compareDecisions(window int, left, right []model.Decision) *model.KernelError {
	if len(left) != len(right) {
		return diffError(window, "", "decisions", fmt.Sprintf("%d", len(left)), fmt.Sprintf("%d", len(right)))
	}
	for i := range left {
		if left[i].ScreenID != right[i].ScreenID {
			return diffError(window, left[i].ScreenID, "decisions.screen_id", left[i].ScreenID, right[i].ScreenID)
		}
		if left[i].At != right[i].At {
			return diffError(window, left[i].ScreenID, "decisions.at_tick", fmt.Sprintf("%d", left[i].At), fmt.Sprintf("%d", right[i].At))
		}
		if left[i].Category != right[i].Category {
			return diffError(window, left[i].ScreenID, "decisions.category", string(left[i].Category), string(right[i].Category))
		}
		if left[i].Winner != right[i].Winner {
			return diffError(window, left[i].ScreenID, "decisions.winner", left[i].Winner, right[i].Winner)
		}
		if err := compareStrings(window, left[i].ScreenID, "decisions.suppressed", left[i].Suppressed, right[i].Suppressed); err != nil {
			return err
		}
	}
	return nil
}

func compareScreen(left, right model.ScreenCommit) *model.KernelError {
	window := left.SourceWindow
	if left.ScreenID != right.ScreenID {
		return diffError(window, left.ScreenID, "screen_id", left.ScreenID, right.ScreenID)
	}
	if left.SourceWindow != right.SourceWindow {
		return diffError(window, left.ScreenID, "source_window", fmt.Sprintf("%d", left.SourceWindow), fmt.Sprintf("%d", right.SourceWindow))
	}
	if left.Mapped.Source != right.Mapped.Source {
		return diffError(window, left.ScreenID, "mapped.source", fmt.Sprint(left.Mapped.Source), fmt.Sprint(right.Mapped.Source))
	}
	if left.Mapped.RawPixels != right.Mapped.RawPixels {
		return diffError(window, left.ScreenID, "mapped.raw_pixels", fmt.Sprint(left.Mapped.RawPixels), fmt.Sprint(right.Mapped.RawPixels))
	}
	if left.Mapped.Pixels != right.Mapped.Pixels {
		return diffError(window, left.ScreenID, "mapped.pixels", fmt.Sprint(left.Mapped.Pixels), fmt.Sprint(right.Mapped.Pixels))
	}
	if left.Mapped.CropLeft != right.Mapped.CropLeft ||
		left.Mapped.CropTop != right.Mapped.CropTop ||
		left.Mapped.CropRight != right.Mapped.CropRight ||
		left.Mapped.CropBottom != right.Mapped.CropBottom {
		return diffError(window, left.ScreenID, "mapped.crop", fmt.Sprintf("%d/%d/%d/%d",
			left.Mapped.CropLeft, left.Mapped.CropTop, left.Mapped.CropRight, left.Mapped.CropBottom),
			fmt.Sprintf("%d/%d/%d/%d", right.Mapped.CropLeft, right.Mapped.CropTop, right.Mapped.CropRight, right.Mapped.CropBottom))
	}
	if left.Mapped.Params != right.Mapped.Params {
		return diffError(window, left.ScreenID, "mapped.params", fmt.Sprint(left.Mapped.Params), fmt.Sprint(right.Mapped.Params))
	}
	if left.Metrics != right.Metrics {
		return diffError(window, left.ScreenID, "metrics", fmt.Sprint(left.Metrics), fmt.Sprint(right.Metrics))
	}
	if len(left.Packets) != len(right.Packets) {
		return diffError(window, left.ScreenID, "packets", fmt.Sprintf("%d", len(left.Packets)), fmt.Sprintf("%d", len(right.Packets)))
	}
	for i := range left.Packets {
		if left.Packets[i] != right.Packets[i] {
			return diffError(window, left.ScreenID, "packets", fmt.Sprint(left.Packets[i]), fmt.Sprint(right.Packets[i]))
		}
	}
	if err := compareInts(window, left.ScreenID, "replay_windows", left.ReplayWindows, right.ReplayWindows); err != nil {
		return err
	}
	if err := kernel.ValidateMetricBound(left.ScreenID, left.Metrics.ActualDeviation, left.Metrics.TotalBound); err != nil {
		return err
	}
	if left.ContentHash != right.ContentHash {
		return diffError(window, left.ScreenID, "content_hash", left.ContentHash, right.ContentHash)
	}
	return nil
}

func compareSnapshot(left, right model.RuntimeSnapshot) *model.KernelError {
	if left.Window != right.Window {
		return diffError(left.Window, "", "snapshot.window", fmt.Sprintf("%d", left.Window), fmt.Sprintf("%d", right.Window))
	}
	if left.SessionState != right.SessionState {
		return diffError(left.Window, "", "snapshot.session_state", string(left.SessionState), string(right.SessionState))
	}
	if err := compareStrings(left.Window, "", "snapshot.active_screens", left.ActiveScreens, right.ActiveScreens); err != nil {
		return err
	}
	if left.TopologyVersion != right.TopologyVersion {
		return diffError(left.Window, "", "snapshot.topology_version", fmt.Sprintf("%d", left.TopologyVersion), fmt.Sprintf("%d", right.TopologyVersion))
	}
	if left.LastCommittedWindow != right.LastCommittedWindow {
		return diffError(left.Window, "", "snapshot.last_committed_window", fmt.Sprintf("%d", left.LastCommittedWindow), fmt.Sprintf("%d", right.LastCommittedWindow))
	}
	if left.EventCursor != right.EventCursor {
		return diffError(left.Window, "", "snapshot.event_cursor", fmt.Sprintf("%d", left.EventCursor), fmt.Sprintf("%d", right.EventCursor))
	}
	if (left.LastSourceRegion == nil) != (right.LastSourceRegion == nil) {
		return diffError(left.Window, "", "snapshot.last_source_region", fmt.Sprint(left.LastSourceRegion), fmt.Sprint(right.LastSourceRegion))
	}
	if left.LastSourceRegion != nil && *left.LastSourceRegion != *right.LastSourceRegion {
		return diffError(left.Window, "", "snapshot.last_source_region", fmt.Sprint(*left.LastSourceRegion), fmt.Sprint(*right.LastSourceRegion))
	}
	if len(left.Screens) != len(right.Screens) {
		return diffError(left.Window, "", "snapshot.screens", fmt.Sprintf("%d", len(left.Screens)), fmt.Sprintf("%d", len(right.Screens)))
	}
	for i := range left.Screens {
		if model.HashJSON(left.Screens[i]) != model.HashJSON(right.Screens[i]) {
			return diffError(left.Window, left.Screens[i].Screen.ID, "snapshot.screens", fmt.Sprint(left.Screens[i]), fmt.Sprint(right.Screens[i]))
		}
	}
	return nil
}

func compareStrings(window int, screenID, field string, left, right []string) *model.KernelError {
	if len(left) != len(right) {
		return diffError(window, screenID, field, fmt.Sprintf("%d", len(left)), fmt.Sprintf("%d", len(right)))
	}
	for i := range left {
		if left[i] != right[i] {
			return diffError(window, screenID, fmt.Sprintf("%s[%d]", field, i), left[i], right[i])
		}
	}
	return nil
}

func compareInts(window int, screenID, field string, left, right []int) *model.KernelError {
	if len(left) != len(right) {
		return diffError(window, screenID, field, fmt.Sprintf("%d", len(left)), fmt.Sprintf("%d", len(right)))
	}
	for i := range left {
		if left[i] != right[i] {
			return diffError(window, screenID, fmt.Sprintf("%s[%d]", field, i), fmt.Sprintf("%d", left[i]), fmt.Sprintf("%d", right[i]))
		}
	}
	return nil
}

func diffError(window int, screenID, field, left, right string) *model.KernelError {
	return &model.KernelError{
		Code:     model.CodeDiff,
		Message:  "replay outputs diverged",
		Window:   window,
		ScreenID: screenID,
		Field:    field,
		Diff: &model.Diff{
			Window:   window,
			ScreenID: screenID,
			Field:    field,
			Left:     left,
			Right:    right,
		},
	}
}
