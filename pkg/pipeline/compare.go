package pipeline

import (
	"fmt"
	"sort"

	"lipsync.local/prototype/pkg/domain"
)

// CompareReports compares package partitions, per-frame lip classes, per-frame
// dispatch sets and the committed shard sequence. It returns E_DIFF at the first
// deterministic mismatch, including dialogue, frame and field.
func CompareReports(base, other *Report) error {
	if base == nil || other == nil {
		return &domain.Error{Code: domain.ErrDiff, Field: "report", Reason: "nil report"}
	}
	ids := dialogueIDs(base)
	for _, id := range dialogueIDs(other) {
		if !containsString(ids, id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		a, aok := base.Shard(id)
		b, bok := other.Shard(id)
		if !aok || !bok {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: id, Field: "dialogue_presence",
				Reason: fmt.Sprintf("presence base=%v other=%v", aok, bok)}
		}
		if err := compareShard(a, b); err != nil {
			return err
		}
	}
	if len(base.CommitSequence) != len(other.CommitSequence) {
		return &domain.Error{Code: domain.ErrDiff, Field: "commit_sequence.length",
			Reason: fmt.Sprintf("%d != %d", len(base.CommitSequence), len(other.CommitSequence))}
	}
	for i := range base.CommitSequence {
		if base.CommitSequence[i] != other.CommitSequence[i] {
			return &domain.Error{Code: domain.ErrDiff, Field: fmt.Sprintf("commit_sequence[%d]", i),
				Reason: base.CommitSequence[i] + " != " + other.CommitSequence[i]}
		}
	}
	return nil
}

func compareShard(a, b ShardResult) error {
	if (a.Err == nil) != (b.Err == nil) {
		return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Field: "shard_error",
			Reason: "error presence differs"}
	}
	if a.Err != nil && domain.CodeOf(a.Err) != domain.CodeOf(b.Err) {
		return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Field: "shard_error.code",
			Reason: string(domain.CodeOf(a.Err)) + " != " + string(domain.CodeOf(b.Err))}
	}
	if len(a.Lip.Packages) != len(b.Lip.Packages) {
		return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Field: "packages.length",
			Reason: fmt.Sprintf("%d != %d", len(a.Lip.Packages), len(b.Lip.Packages))}
	}
	for i := range a.Lip.Packages {
		pa, pb := a.Lip.Packages[i], b.Lip.Packages[i]
		if pa.ID != pb.ID {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, PackageID: pa.ID, Field: "package.id",
				Reason: pa.ID + " != " + pb.ID}
		}
		if pa.StartFrame != pb.StartFrame || pa.EndFrame != pb.EndFrame {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, PackageID: pa.ID,
				Frame: pa.StartFrame, HaveFrame: true, Field: "package.frame_range",
				Reason: fmt.Sprintf("[%d,%d) != [%d,%d)", pa.StartFrame, pa.EndFrame, pb.StartFrame, pb.EndFrame)}
		}
		if !equalStrings(pa.PhonemeIDs, pb.PhonemeIDs) {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, PackageID: pa.ID,
				Frame: pa.StartFrame, HaveFrame: true, Field: "package.phoneme_ids",
				Reason: fmt.Sprintf("%v != %v", pa.PhonemeIDs, pb.PhonemeIDs)}
		}
	}
	if len(a.Lip.Frames) != len(b.Lip.Frames) {
		return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Field: "frames.length",
			Reason: fmt.Sprintf("%d != %d", len(a.Lip.Frames), len(b.Lip.Frames))}
	}
	for i := range a.Lip.Frames {
		fa, fb := a.Lip.Frames[i], b.Lip.Frames[i]
		if fa.Frame != fb.Frame {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Frame: fa.Frame, HaveFrame: true,
				Field: "frame.number", Reason: fmt.Sprintf("%d != %d", fa.Frame, fb.Frame)}
		}
		if fa.Lip != fb.Lip {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Frame: fa.Frame, HaveFrame: true,
				Field: "frame.lip", LipA: fa.Lip, LipB: fb.Lip, Reason: fa.Lip + " != " + fb.Lip}
		}
		if fa.PackageID != fb.PackageID {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Frame: fa.Frame, HaveFrame: true,
				Field: "frame.package_id", PackageID: fa.PackageID, Reason: fa.PackageID + " != " + fb.PackageID}
		}
		if fa.Transition != fb.Transition {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Frame: fa.Frame, HaveFrame: true,
				Field: "frame.transition", Reason: "transition marker differs"}
		}
		if fa.TransitionFrom != fb.TransitionFrom || fa.TransitionTo != fb.TransitionTo {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Frame: fa.Frame, HaveFrame: true,
				Field:  "frame.transition_family",
				Reason: fmt.Sprintf("%s->%s != %s->%s", fa.TransitionFrom, fa.TransitionTo, fb.TransitionFrom, fb.TransitionTo)}
		}
	}
	if len(a.Lip.Transitions) != len(b.Lip.Transitions) {
		return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Field: "transitions.length",
			Reason: fmt.Sprintf("%d != %d", len(a.Lip.Transitions), len(b.Lip.Transitions))}
	}
	for i := range a.Lip.Transitions {
		ta, tb := a.Lip.Transitions[i], b.Lip.Transitions[i]
		if ta.Frame != tb.Frame || ta.PackageID != tb.PackageID || ta.Bridge != tb.Bridge {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Frame: ta.Frame, HaveFrame: true,
				PackageID: ta.PackageID, Field: "transition",
				Reason: fmt.Sprintf("%s@%d/%s != %s@%d/%s", ta.PackageID, ta.Frame, ta.Bridge, tb.PackageID, tb.Frame, tb.Bridge)}
		}
	}
	if !equalStrings(a.Dispatch.Accepted, b.Dispatch.Accepted) {
		return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Field: "dispatch.accepted",
			Reason: fmt.Sprintf("%v != %v", a.Dispatch.Accepted, b.Dispatch.Accepted)}
	}
	if len(a.Dispatch.DispatchFrame) != len(b.Dispatch.DispatchFrame) {
		return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Field: "dispatch.frame_map.length",
			Reason: fmt.Sprintf("%d != %d", len(a.Dispatch.DispatchFrame), len(b.Dispatch.DispatchFrame))}
	}
	frameKeys := make([]string, 0, len(a.Dispatch.DispatchFrame)+len(b.Dispatch.DispatchFrame))
	for id := range a.Dispatch.DispatchFrame {
		frameKeys = append(frameKeys, id)
	}
	for id := range b.Dispatch.DispatchFrame {
		if _, ok := a.Dispatch.DispatchFrame[id]; !ok {
			frameKeys = append(frameKeys, id)
		}
	}
	sort.Strings(frameKeys)
	for _, id := range frameKeys {
		fa, oka := a.Dispatch.DispatchFrame[id]
		fb, okb := b.Dispatch.DispatchFrame[id]
		if !oka || !okb || fa != fb {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, PackageID: id,
				Frame: fa, HaveFrame: true, Field: "dispatch.frame",
				Reason: fmt.Sprintf("%d != %d", fa, fb)}
		}
	}
	if len(a.Dispatch.Frames) != len(b.Dispatch.Frames) {
		return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Field: "dispatch.frames.length",
			Reason: fmt.Sprintf("%d != %d", len(a.Dispatch.Frames), len(b.Dispatch.Frames))}
	}
	for i := range a.Dispatch.Frames {
		fa, fb := a.Dispatch.Frames[i], b.Dispatch.Frames[i]
		if fa.Frame != fb.Frame {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Frame: fa.Frame, HaveFrame: true,
				Field: "dispatch.frame_number", Reason: fmt.Sprintf("%d != %d", fa.Frame, fb.Frame)}
		}
		if !equalStrings(fa.Dispatched, fb.Dispatched) {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Frame: fa.Frame, HaveFrame: true,
				Field: "dispatch.dispatched", Reason: fmt.Sprintf("%v != %v", fa.Dispatched, fb.Dispatched)}
		}
		if fa.Used != fb.Used {
			return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Frame: fa.Frame, HaveFrame: true,
				Field: "dispatch.used", Reason: fmt.Sprintf("%d != %d", fa.Used, fb.Used)}
		}
		channels := make([]string, 0, len(fa.Channels)+len(fb.Channels))
		for channel := range fa.Channels {
			channels = append(channels, channel)
		}
		for channel := range fb.Channels {
			if _, ok := fa.Channels[channel]; !ok {
				channels = append(channels, channel)
			}
		}
		sort.Strings(channels)
		for _, channel := range channels {
			if !equalStrings(fa.Channels[channel], fb.Channels[channel]) {
				return &domain.Error{Code: domain.ErrDiff, DialogueID: a.DialogueID, Frame: fa.Frame, HaveFrame: true,
					Field:  "dispatch.channel." + channel,
					Reason: fmt.Sprintf("%v != %v", fa.Channels[channel], fb.Channels[channel])}
			}
		}
	}
	return nil
}

func dialogueIDs(report *Report) []string {
	ids := make([]string, 0, len(report.Shards))
	for _, shard := range report.Shards {
		ids = append(ids, shard.DialogueID)
	}
	sort.Strings(ids)
	return ids
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
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
