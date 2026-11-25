package lipsync

import (
	"fmt"
	"sort"
)

func BuildFullResult(project *Project) (*FullResult, error) {
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	EnsureDialogueHashes(project)

	dialogues := sortedDialogues(project.Dialogues)
	result := &FullResult{
		ProjectID:   project.ID,
		Recipe:      project.Recipe,
		Dialogues:   make([]*DialogueResult, 0, len(dialogues)),
		Truncations: []Truncation{},
	}
	allPacks := make([]Pack, 0)

	for _, d := range dialogues {
		quantized, truncations, err := QuantizeDialogue(d, project.Recipe)
		if err != nil {
			return nil, err
		}
		packs, err := PackDialogue(d, quantized, project.Recipe)
		if err != nil {
			return nil, err
		}
		attribution := BuildAttribution(d, quantized, packs)
		result.Dialogues = append(result.Dialogues, &DialogueResult{
			Dialogue:    d,
			Quantized:   quantized,
			Packs:       packs,
			Attribution: attribution,
			Truncations: truncations,
		})
		result.Truncations = append(result.Truncations, truncations...)
		for _, pack := range packs {
			allPacks = append(allPacks, pack)
		}
	}
	result.Dispatch = DispatchPacks(allPacks, project.Recipe)
	return result, nil
}

func sortedDialogues(dialogues []*Dialogue) []*Dialogue {
	out := append([]*Dialogue(nil), dialogues...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ShardIndex != out[j].ShardIndex {
			return out[i].ShardIndex < out[j].ShardIndex
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func BuildAttribution(d *Dialogue, qps []QuantizedPhoneme, packs []Pack) []FrameAttribution {
	frameCount := 0
	for _, qp := range qps {
		if qp.EndFrame > frameCount {
			frameCount = qp.EndFrame
		}
	}
	if len(packs) > 0 && packs[len(packs)-1].EndFrame > frameCount {
		frameCount = packs[len(packs)-1].EndFrame
	}
	out := make([]FrameAttribution, frameCount)
	for frame := 0; frame < frameCount; frame++ {
		out[frame] = FrameAttribution{
			DialogueID: d.ID,
			ShardIndex: d.ShardIndex,
			Frame:      frame,
			PackIndex:  -1,
		}
	}
	for _, pack := range packs {
		dominant := dominantClass(qps, pack.PhonemeIDs)
		for frame := pack.StartFrame; frame < pack.EndFrame; frame++ {
			if frame < 0 || frame >= frameCount {
				continue
			}
			out[frame].PackIndex = pack.Index
			out[frame].PackID = pack.PackID
			out[frame].DominantClass = dominant
		}
	}
	return out
}

func dominantClass(qps []QuantizedPhoneme, ids []string) PhonemeClass {
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	bestWeight := -1
	bestClass := ClassSilence
	for _, qp := range qps {
		if wanted[qp.ID] && qp.WeightPermyriad > bestWeight {
			bestWeight = qp.WeightPermyriad
			bestClass = qp.Class
		}
	}
	return bestClass
}

func DiffResults(expected, actual *FullResult) *Error {
	if expected == nil || actual == nil {
		return NewError(ECDiff, "", 0, false, "result", "one result is nil")
	}
	if expected.ProjectID != actual.ProjectID {
		return NewError(ECDiff, "", 0, false, "projectId", "project id mismatch %q != %q", expected.ProjectID, actual.ProjectID)
	}
	if len(expected.Dialogues) != len(actual.Dialogues) {
		return NewError(ECDiff, "", 0, false, "dialogues",
			"dialogue count mismatch %d != %d", len(expected.Dialogues), len(actual.Dialogues))
	}
	for i := range expected.Dialogues {
		ed := expected.Dialogues[i]
		ad := actual.Dialogues[i]
		if ed.Dialogue.ID != ad.Dialogue.ID {
			return NewError(ECDiff, "", 0, false,
				fmt.Sprintf("dialogues[%d].id", i), "dialogue id mismatch %q != %q", ed.Dialogue.ID, ad.Dialogue.ID)
		}
		dialogueID := ed.Dialogue.ID
		if len(ed.Packs) != len(ad.Packs) {
			err := NewError(ECDiff, dialogueID, ed.Dialogue.ShardIndex, true,
				fmt.Sprintf("dialogues.%s.packs.length", dialogueID),
				"pack count mismatch %d != %d", len(ed.Packs), len(ad.Packs))
			return err
		}
		for j := range ed.Packs {
			if diff := diffPack(dialogueID, ed.Dialogue.ShardIndex, &ed.Packs[j], &ad.Packs[j]); diff != nil {
				return diff
			}
		}
		if len(ed.Attribution) != len(ad.Attribution) {
			return diffFrame(dialogueID, ed.Dialogue.ShardIndex, 0, "attribution.length",
				"attribution frame count %d != %d", len(ed.Attribution), len(ad.Attribution))
		}
		for f := range ed.Attribution {
			if ed.Attribution[f].PackID != ad.Attribution[f].PackID {
				return diffFrame(dialogueID, ed.Dialogue.ShardIndex, f, "attribution.packId",
					"pack attribution %q != %q", ed.Attribution[f].PackID, ad.Attribution[f].PackID)
			}
			if ed.Attribution[f].DominantClass != ad.Attribution[f].DominantClass {
				return diffFrame(dialogueID, ed.Dialogue.ShardIndex, f, "attribution.dominantClass",
					"dominant class %q != %q", ed.Attribution[f].DominantClass, ad.Attribution[f].DominantClass)
			}
		}
	}
	return DiffDispatch(expected.Dispatch, actual.Dispatch)
}

func diffPack(dialogueID string, shard int, expected, actual *Pack) *Error {
	frame := expected.StartFrame
	if expected.Index != actual.Index {
		return diffFrame(dialogueID, shard, frame, "packs.index", "pack index %d != %d", expected.Index, actual.Index)
	}
	if stringsJoin(expected.PhonemeIDs) != stringsJoin(actual.PhonemeIDs) {
		return diffFrame(dialogueID, shard, frame, fmt.Sprintf("packs[%d].phonemeIds", expected.Index),
			"phoneme ids %v != %v", expected.PhonemeIDs, actual.PhonemeIDs)
	}
	if expected.StartFrame != actual.StartFrame {
		return diffFrame(dialogueID, shard, expected.StartFrame, fmt.Sprintf("packs[%d].startFrame", expected.Index),
			"start frame %d != %d", expected.StartFrame, actual.StartFrame)
	}
	if expected.EndFrame != actual.EndFrame {
		return diffFrame(dialogueID, shard, expected.EndFrame, fmt.Sprintf("packs[%d].endFrame", expected.Index),
			"end frame %d != %d", expected.EndFrame, actual.EndFrame)
	}
	if expected.VisibleFrames != actual.VisibleFrames {
		return diffFrame(dialogueID, shard, frame, fmt.Sprintf("packs[%d].visibleFrames", expected.Index),
			"visible frames %d != %d", expected.VisibleFrames, actual.VisibleFrames)
	}
	if expected.SummaryHash != actual.SummaryHash {
		return diffFrame(dialogueID, shard, frame, fmt.Sprintf("packs[%d].summaryHash", expected.Index),
			"summary hash %s != %s", expected.SummaryHash, actual.SummaryHash)
	}
	return nil
}

func DiffDispatch(expected, actual *DispatchResult) *Error {
	if expected == nil || actual == nil {
		return NewError(ECDiff, "", 0, false, "dispatch", "dispatch result nil")
	}
	if expected.FrameCount != actual.FrameCount || len(expected.Frames) != len(actual.Frames) {
		frame := minInt(len(expected.Frames), len(actual.Frames))
		return diffFrame("", 0, frame, "dispatch.frameCount",
			"frame count %d/%d != %d/%d", expected.FrameCount, len(expected.Frames), actual.FrameCount, len(actual.Frames))
	}
	for i := range expected.Frames {
		ef := expected.Frames[i]
		af := actual.Frames[i]
		if ef.Frame != af.Frame {
			return diffFrame("", 0, i, "dispatch.frames.frame", "frame index %d != %d", ef.Frame, af.Frame)
		}
		if ef.BudgetUsed != af.BudgetUsed {
			return diffFrame(firstDialogueInFrame(ef), 0, ef.Frame, "dispatch.budgetUsed",
				"budget used %d != %d", ef.BudgetUsed, af.BudgetUsed)
		}
		if ef.ActivePackCount != af.ActivePackCount {
			return diffFrame(firstDialogueInFrame(ef), 0, ef.Frame, "dispatch.activePackCount",
				"active pack count %d != %d", ef.ActivePackCount, af.ActivePackCount)
		}
		es := frameAssignmentSet(ef)
		as := frameAssignmentSet(af)
		if es != as {
			return diffFrame(firstDialogueInFrame(ef), 0, ef.Frame, "dispatch.dispatched",
				"dispatched set %s != %s", es, as)
		}
		ech := channelSet(ef)
		ach := channelSet(af)
		if ech != ach {
			return diffFrame(firstDialogueInFrame(ef), 0, ef.Frame, "dispatch.channels",
				"channel set %s != %s", ech, ach)
		}
		ed := deferralSet(ef.DeferredThisFrame)
		ad := deferralSet(af.DeferredThisFrame)
		if ed != ad {
			return diffFrame(firstDialogueInFrame(ef), 0, ef.Frame, "dispatch.deferred",
				"deferral set %s != %s", ed, ad)
		}
	}
	if len(expected.Rejections) != len(actual.Rejections) {
		return diffFrame("", 0, 0, "dispatch.rejections",
			"rejection count %d != %d", len(expected.Rejections), len(actual.Rejections))
	}
	return nil
}

func diffFrame(dialogueID string, shard, frame int, path, format string, args ...any) *Error {
	err := NewError(ECDiff, dialogueID, shard, true, path, format, args...)
	err.Frame = frame
	err.HasFrame = true
	return err
}

func stringsJoin(values []string) string {
	out := ""
	for i, value := range values {
		if i > 0 {
			out += "|"
		}
		out += value
	}
	return out
}

func frameAssignmentSet(frame FrameDispatch) string {
	values := make([]string, 0, len(frame.Dispatched))
	for _, a := range frame.Dispatched {
		values = append(values, fmt.Sprintf("ch%d:%s@%d", a.Channel, a.PackID, a.ActiveFrom))
	}
	sort.Strings(values)
	return stringsJoin(values)
}

func channelSet(frame FrameDispatch) string {
	values := make([]string, 0, len(frame.Channels))
	for _, ch := range frame.Channels {
		for _, packID := range ch.PackIDs {
			values = append(values, fmt.Sprintf("ch%d:active:%s", ch.Channel, packID))
		}
		for _, packID := range ch.DeliveredPackIDs {
			values = append(values, fmt.Sprintf("ch%d:delivered:%s", ch.Channel, packID))
		}
	}
	sort.Strings(values)
	return stringsJoin(values)
}

func deferralSet(items []DeferralRecord) string {
	values := make([]string, 0, len(items))
	for _, d := range items {
		values = append(values, fmt.Sprintf("%s:%s@%d:%s", d.DialogueID, d.PackID, d.Frame, d.Reason))
	}
	sort.Strings(values)
	return stringsJoin(values)
}

func firstDialogueInFrame(frame FrameDispatch) string {
	if len(frame.Dispatched) > 0 {
		return frame.Dispatched[0].DialogueID
	}
	if len(frame.DeferredThisFrame) > 0 {
		return frame.DeferredThisFrame[0].DialogueID
	}
	if len(frame.RejectedThisFrame) > 0 {
		return frame.RejectedThisFrame[0].DialogueID
	}
	return ""
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
