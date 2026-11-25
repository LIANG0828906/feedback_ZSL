package kernel

import (
	"fmt"
	"sort"

	"example.com/multiscreen-cast/protocol"
)

func CompareTraces(left, right Trace) *protocol.KernelError {
	if len(left.Windows) != len(right.Windows) {
		return diffError(0, "", "windows", fmt.Sprint(len(left.Windows)), fmt.Sprint(len(right.Windows)))
	}
	for i := range left.Windows {
		if err := compareWindow(left.Windows[i], right.Windows[i]); err != nil {
			return err
		}
	}
	if len(left.EventEffects) != len(right.EventEffects) {
		return diffError(0, "", "event_effects", fmt.Sprint(len(left.EventEffects)), fmt.Sprint(len(right.EventEffects)))
	}
	for i := range left.EventEffects {
		if err := compareEventEffect(left.EventEffects[i], right.EventEffects[i]); err != nil {
			return err
		}
	}
	if len(left.Adjudications) != len(right.Adjudications) {
		return diffError(0, "", "adjudications", fmt.Sprint(len(left.Adjudications)), fmt.Sprint(len(right.Adjudications)))
	}
	for i := range left.Adjudications {
		if !adjudicationsEqual(left.Adjudications[i], right.Adjudications[i]) {
			return diffError(0, "", fmt.Sprintf("adjudications[%d]", i),
				fmt.Sprintf("%+v", left.Adjudications[i]), fmt.Sprintf("%+v", right.Adjudications[i]))
		}
	}
	if left.FinalState != right.FinalState {
		return diffError(0, "", "final_state", string(left.FinalState), string(right.FinalState))
	}
	if !equalStrings(left.FinalEndpoints, right.FinalEndpoints) {
		return diffError(0, "", "final_endpoints", fmt.Sprint(left.FinalEndpoints), fmt.Sprint(right.FinalEndpoints))
	}
	return nil
}

func compareWindow(left, right WindowSnapshot) *protocol.KernelError {
	window := left.Window
	if window != right.Window {
		return diffError(window, "", "window", fmt.Sprint(window), fmt.Sprint(right.Window))
	}
	if left.Time != right.Time {
		return diffError(window, "", "time", fmt.Sprint(left.Time), fmt.Sprint(right.Time))
	}
	if left.State != right.State {
		return diffError(window, "", "state", string(left.State), string(right.State))
	}
	if left.TopologyVersion != right.TopologyVersion {
		return diffError(window, "", "topology_version", fmt.Sprint(left.TopologyVersion), fmt.Sprint(right.TopologyVersion))
	}
	if !equalStrings(left.Endpoints, right.Endpoints) {
		return diffError(window, "", "endpoints", fmt.Sprint(left.Endpoints), fmt.Sprint(right.Endpoints))
	}
	if len(left.Screens) != len(right.Screens) {
		return diffError(window, "", "screens.count", fmt.Sprint(len(left.Screens)), fmt.Sprint(len(right.Screens)))
	}
	rightScreens := make(map[string]ScreenResult, len(right.Screens))
	for _, screen := range right.Screens {
		rightScreens[screen.DisplayID] = screen
	}
	for _, leftScreen := range left.Screens {
		rightScreen, ok := rightScreens[leftScreen.DisplayID]
		if !ok {
			return diffError(window, leftScreen.DisplayID, "screens."+leftScreen.DisplayID+".presence", "present", "missing")
		}
		if err := compareScreenResult(window, leftScreen, rightScreen); err != nil {
			return err
		}
	}
	if len(left.QueueRecords) != len(right.QueueRecords) {
		return diffError(window, "", "queue_records", fmt.Sprint(left.QueueRecords), fmt.Sprint(right.QueueRecords))
	}
	for i := range left.QueueRecords {
		if left.QueueRecords[i] != right.QueueRecords[i] {
			return diffError(window, left.QueueRecords[i].DisplayID, fmt.Sprintf("queue_records[%d]", i),
				fmt.Sprintf("%+v", left.QueueRecords[i]), fmt.Sprintf("%+v", right.QueueRecords[i]))
		}
	}
	if len(left.Adjudications) != len(right.Adjudications) {
		return diffError(window, "", "adjudications", fmt.Sprint(len(left.Adjudications)), fmt.Sprint(len(right.Adjudications)))
	}
	for i := range left.Adjudications {
		if !adjudicationsEqual(left.Adjudications[i], right.Adjudications[i]) {
			return diffError(window, "", fmt.Sprintf("adjudications[%d]", i),
				fmt.Sprintf("%+v", left.Adjudications[i]), fmt.Sprintf("%+v", right.Adjudications[i]))
		}
	}
	return nil
}

func compareScreenResult(window int, left, right ScreenResult) *protocol.KernelError {
	display := left.DisplayID
	if display != right.DisplayID {
		return diffError(window, display, "display_id", display, right.DisplayID)
	}
	if left.SourceWindow != right.SourceWindow {
		return diffError(window, display, "source_window", fmt.Sprint(left.SourceWindow), fmt.Sprint(right.SourceWindow))
	}
	if err := compareRect(window, display, "pixel", left.Pixel, right.Pixel); err != nil {
		return err
	}
	if err := compareRect(window, display, "world", left.World, right.World); err != nil {
		return err
	}
	if err := compareRect(window, display, "screen_world", left.ScreenWorld, right.ScreenWorld); err != nil {
		return err
	}
	if left.Mapping.TopologyVersion != right.Mapping.TopologyVersion {
		return diffError(window, display, "mapping.topology_version",
			fmt.Sprint(left.Mapping.TopologyVersion), fmt.Sprint(right.Mapping.TopologyVersion))
	}
	if left.Mapping.AlgorithmVersion != right.Mapping.AlgorithmVersion {
		return diffError(window, display, "mapping.algorithm_version",
			left.Mapping.AlgorithmVersion, right.Mapping.AlgorithmVersion)
	}
	if !topologyEqual(left.Mapping.Topology, right.Mapping.Topology) {
		return diffError(window, display, "mapping.topology",
			fmt.Sprintf("%+v", left.Mapping.Topology), fmt.Sprintf("%+v", right.Mapping.Topology))
	}
	if !capabilityEqual(left.Mapping.Capability, right.Mapping.Capability) {
		return diffError(window, display, "mapping.capability",
			fmt.Sprintf("%+v", left.Mapping.Capability), fmt.Sprintf("%+v", right.Mapping.Capability))
	}
	if left.SourceHash != right.SourceHash {
		return diffError(window, display, "source_hash", left.SourceHash, right.SourceHash)
	}
	if left.PixelBufferBytes != right.PixelBufferBytes {
		return diffError(window, display, "pixel_buffer_bytes",
			fmt.Sprint(left.PixelBufferBytes), fmt.Sprint(right.PixelBufferBytes))
	}
	if left.BufferChunks != right.BufferChunks {
		return diffError(window, display, "buffer_chunks", fmt.Sprint(left.BufferChunks), fmt.Sprint(right.BufferChunks))
	}
	return nil
}

func compareRect(window int, display, field string, left, right protocol.Rect) *protocol.KernelError {
	if left.X != right.X {
		return diffError(window, display, field+".x", fmt.Sprint(left.X), fmt.Sprint(right.X))
	}
	if left.Y != right.Y {
		return diffError(window, display, field+".y", fmt.Sprint(left.Y), fmt.Sprint(right.Y))
	}
	if left.Width != right.Width {
		return diffError(window, display, field+".width", fmt.Sprint(left.Width), fmt.Sprint(right.Width))
	}
	if left.Height != right.Height {
		return diffError(window, display, field+".height", fmt.Sprint(left.Height), fmt.Sprint(right.Height))
	}
	return nil
}

func compareEventEffect(left, right EventEffect) *protocol.KernelError {
	window := left.CommittedWindow
	if window == 0 {
		window = left.ReplayWindow
	}
	if window == 0 {
		window = right.CommittedWindow
	}
	if window == 0 {
		window = right.ReplayWindow
	}
	if left.Seq != right.Seq {
		return diffError(window, left.DisplayID, "event.seq", fmt.Sprint(left.Seq), fmt.Sprint(right.Seq))
	}
	if left.Time != right.Time {
		return diffError(window, left.DisplayID, "event.time", fmt.Sprint(left.Time), fmt.Sprint(right.Time))
	}
	if left.Kind != right.Kind {
		return diffError(window, left.DisplayID, "event.kind", string(left.Kind), string(right.Kind))
	}
	if left.DisplayID != right.DisplayID {
		return diffError(window, left.DisplayID, "event.display_id", left.DisplayID, right.DisplayID)
	}
	if left.EffectiveTime != right.EffectiveTime {
		return diffError(window, left.DisplayID, "event.effective_time", fmt.Sprint(left.EffectiveTime), fmt.Sprint(right.EffectiveTime))
	}
	if left.CommittedWindow != right.CommittedWindow {
		return diffError(window, left.DisplayID, "event.committed_window", fmt.Sprint(left.CommittedWindow), fmt.Sprint(right.CommittedWindow))
	}
	if left.StateBefore != right.StateBefore {
		return diffError(window, left.DisplayID, "event.state_before", string(left.StateBefore), string(right.StateBefore))
	}
	if left.StateAfter != right.StateAfter {
		return diffError(window, left.DisplayID, "event.state_after", string(left.StateAfter), string(right.StateAfter))
	}
	if left.TopologyVersion != right.TopologyVersion {
		return diffError(window, left.DisplayID, "event.topology_version", fmt.Sprint(left.TopologyVersion), fmt.Sprint(right.TopologyVersion))
	}
	if !equalStrings(left.EndpointsAfter, right.EndpointsAfter) {
		return diffError(window, left.DisplayID, "event.endpoints_after", fmt.Sprint(left.EndpointsAfter), fmt.Sprint(right.EndpointsAfter))
	}
	if left.ReplayWindow != right.ReplayWindow {
		return diffError(window, left.DisplayID, "event.replay_window", fmt.Sprint(left.ReplayWindow), fmt.Sprint(right.ReplayWindow))
	}
	if (left.Replay == nil) != (right.Replay == nil) {
		return diffError(window, left.DisplayID, "event.replay", fmt.Sprint(left.Replay != nil), fmt.Sprint(right.Replay != nil))
	}
	if left.Replay != nil {
		return compareScreenResult(window, *left.Replay, *right.Replay)
	}
	return nil
}

func adjudicationsEqual(left, right Adjudication) bool {
	if left.Time != right.Time || left.Rule != right.Rule || len(left.Items) != len(right.Items) {
		return false
	}
	for i := range left.Items {
		if left.Items[i] != right.Items[i] {
			return false
		}
	}
	return true
}

func topologyEqual(left, right protocol.Topology) bool {
	return left.X == right.X &&
		left.Y == right.Y &&
		left.Width == right.Width &&
		left.Height == right.Height &&
		left.Resolution == right.Resolution &&
		left.Scale.Equal(right.Scale)
}

func capabilityEqual(left, right protocol.Capability) bool {
	return left.Resolution == right.Resolution &&
		left.Scale.Equal(right.Scale) &&
		left.CropMode == right.CropMode
}

func diffError(window int, displayID, field, left, right string) *protocol.KernelError {
	err := protocol.Errorf(protocol.CodeDiff, "compare", field,
		"first difference window=%d display=%s field=%s left=%s right=%s", window, displayID, field, left, right)
	err.Window = window
	err.DisplayID = displayID
	err.Field = field
	err.Cause = fmt.Sprintf("left=%s right=%s", left, right)
	return err
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func SortedScreenResults(results []ScreenResult) []ScreenResult {
	output := append([]ScreenResult(nil), results...)
	sort.Slice(output, func(i, j int) bool { return output[i].DisplayID < output[j].DisplayID })
	return output
}
