package verify

import (
	"fmt"
	"sort"

	"lipsync.local/prototype/pkg/domain"
	"lipsync.local/prototype/pkg/pipeline"
	"lipsync.local/prototype/pkg/sample"
)

// Truth compares a pipeline report against the deterministic generator's
// analytic package/frame/transition truth and returns E_DIFF at the first
// mismatch.
func Truth(project *domain.Project, report *pipeline.Report, truth *sample.Truth) error {
	if project == nil || report == nil || truth == nil {
		return &domain.Error{Code: domain.ErrDiff, Field: "truth_input", Reason: "nil input"}
	}
	for _, expected := range truth.Dialogues {
		shard, ok := report.Shard(expected.DialogueID)
		if !ok {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, Field: "dialogue", Reason: "missing shard"}
		}
		if shard.Err != nil {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, Field: "shard_error",
				Reason: shard.Err.Error()}
		}
		if len(shard.Lip.Packages) != len(expected.Packages) {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, Field: "packages.length",
				Reason: fmt.Sprintf("%d != %d", len(shard.Lip.Packages), len(expected.Packages))}
		}
		for i := range expected.Packages {
			got, want := shard.Lip.Packages[i], expected.Packages[i]
			if got.ID != want.ID {
				return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, PackageID: got.ID,
					Field: "package.id", Reason: got.ID + " != " + want.ID}
			}
			if got.StartFrame != want.StartFrame || got.EndFrame != want.EndFrame {
				return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, PackageID: got.ID,
					Frame: got.StartFrame, HaveFrame: true, Field: "package.frame_range",
					Reason: fmt.Sprintf("[%d,%d) != [%d,%d)", got.StartFrame, got.EndFrame, want.StartFrame, want.EndFrame)}
			}
			if !sameStrings(got.PhonemeIDs, want.PhonemeIDs) {
				return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, PackageID: got.ID,
					Field: "package.phoneme_ids", Reason: fmt.Sprintf("%v != %v", got.PhonemeIDs, want.PhonemeIDs)}
			}
		}
		if len(shard.Lip.Frames) != len(expected.Frames) {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, Field: "frames.length",
				Reason: fmt.Sprintf("%d != %d", len(shard.Lip.Frames), len(expected.Frames))}
		}
		for i := range expected.Frames {
			got, want := shard.Lip.Frames[i], expected.Frames[i]
			if got.Frame != want.Frame {
				return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, Frame: want.Frame, HaveFrame: true,
					Field: "frame.number", Reason: fmt.Sprintf("%d != %d", got.Frame, want.Frame)}
			}
			if got.Lip != want.Lip {
				return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, Frame: want.Frame, HaveFrame: true,
					Field: "frame.lip", LipA: got.Lip, LipB: want.Lip, Reason: got.Lip + " != " + want.Lip}
			}
			if got.PackageID != want.PackageID {
				return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, Frame: want.Frame, HaveFrame: true,
					Field: "frame.package_id", PackageID: got.PackageID,
					Reason: got.PackageID + " != " + want.PackageID}
			}
			if got.Transition != want.Transition {
				return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, Frame: want.Frame, HaveFrame: true,
					Field: "frame.transition", Reason: fmt.Sprintf("%v != %v", got.Transition, want.Transition)}
			}
		}
		if len(shard.Lip.Transitions) != len(expected.Transitions) {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, Field: "transitions.length",
				Reason: fmt.Sprintf("%d != %d", len(shard.Lip.Transitions), len(expected.Transitions))}
		}
		for i := range expected.Transitions {
			got, want := shard.Lip.Transitions[i], expected.Transitions[i]
			if got.Frame != want.Frame || got.Bridge != want.Bridge || got.PackageID != want.PackageID {
				return &domain.Error{Code: domain.ErrDiff, DialogueID: expected.DialogueID, Frame: want.Frame, HaveFrame: true,
					PackageID: got.PackageID, Field: "transition",
					Reason: fmt.Sprintf("frame=%d bridge=%s package=%s", got.Frame, got.Bridge, got.PackageID)}
			}
		}
	}
	return ValidateDispatch(project, report)
}

// ValidateDispatch checks budget and acceptance invariants independent of the
// generator truth.
func ValidateDispatch(project *domain.Project, report *pipeline.Report) error {
	limit := project.Recipe.MaxPackagesPerFrame
	channelLimit := make(map[string]int, len(project.Recipe.Channels))
	for _, ch := range project.Recipe.Channels {
		channelLimit[ch.ID] = ch.Slots
		if project.Recipe.MaxPackagesPerChannelFrame < ch.Slots {
			channelLimit[ch.ID] = project.Recipe.MaxPackagesPerChannelFrame
		}
	}
	accepted := make(map[string]struct{}, len(report.GlobalDispatch.Accepted))
	for _, id := range report.GlobalDispatch.Accepted {
		accepted[id] = struct{}{}
	}
	for _, id := range report.GlobalDispatch.Rejected {
		if _, ok := accepted[id]; ok {
			return &domain.Error{Code: domain.ErrConflict, PackageID: id, Field: "dispatch",
				Reason: "rejected package also accepted"}
		}
	}
	for _, frame := range report.GlobalDispatch.Frames {
		if frame.Used > limit {
			return &domain.Error{Code: domain.ErrConflict, Frame: frame.Frame, HaveFrame: true,
				Field: "frame_budget", Reason: fmt.Sprintf("%d > %d", frame.Used, limit)}
		}
		if len(frame.Dispatched) > limit {
			return &domain.Error{Code: domain.ErrConflict, Frame: frame.Frame, HaveFrame: true,
				Field: "frame_dispatch_set", Reason: fmt.Sprintf("%d > %d", len(frame.Dispatched), limit)}
		}
		for channel, ids := range frame.Channels {
			if len(ids) > channelLimit[channel] {
				return &domain.Error{Code: domain.ErrConflict, Frame: frame.Frame, HaveFrame: true,
					Field: "channel_slots", Reason: fmt.Sprintf("%s: %d > %d", channel, len(ids), channelLimit[channel])}
			}
		}
	}
	packageIDs := make(map[string]struct{})
	for _, shard := range report.Shards {
		for _, pkg := range shard.Lip.Packages {
			packageIDs[pkg.ID] = struct{}{}
		}
	}
	for id := range accepted {
		if _, ok := packageIDs[id]; !ok {
			return &domain.Error{Code: domain.ErrConflict, PackageID: id, Field: "dispatch",
				Reason: "accepted package is not part of a package partition"}
		}
	}
	return nil
}

// ReportIdentifiers returns sorted dialogue IDs for diagnostics.
func ReportIdentifiers(report *pipeline.Report) []string {
	ids := make([]string, 0, len(report.Shards))
	for _, shard := range report.Shards {
		ids = append(ids, shard.DialogueID)
	}
	sort.Strings(ids)
	return ids
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
