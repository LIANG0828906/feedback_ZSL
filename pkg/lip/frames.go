package lip

import (
	"fmt"
	"sort"

	"lipsync.local/prototype/pkg/domain"
)

// RenderFrames derives the frame lip track from quantized phonemes and package
// partitions. Non-contiguous family changes are represented by deterministic
// bridge frames inside the declared tolerance; the timeline length is not
// extended.
func RenderFrames(dialogueID string, phonemes []domain.QuantizedPhoneme, packages []domain.LipPackage, tolerance int) (domain.LipResult, error) {
	if len(phonemes) == 0 || len(packages) == 0 {
		return domain.LipResult{}, &domain.Error{
			Code: domain.ErrSchema, DialogueID: dialogueID, Field: "phonemes/packages", Reason: "empty input",
		}
	}
	if tolerance < 0 {
		return domain.LipResult{}, &domain.Error{
			Code: domain.ErrPara, DialogueID: dialogueID, Field: "transition_tolerance", Reason: "must not be negative",
		}
	}
	sorted := append([]domain.QuantizedPhoneme(nil), phonemes...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].StartFrame != sorted[j].StartFrame {
			return sorted[i].StartFrame < sorted[j].StartFrame
		}
		return sorted[i].PhonemeID < sorted[j].PhonemeID
	})
	owner := make(map[string]string, len(sorted))
	for _, pkg := range packages {
		for _, id := range pkg.PhonemeIDs {
			if existing, ok := owner[id]; ok {
				return domain.LipResult{}, &domain.Error{
					Code: domain.ErrLip, DialogueID: dialogueID, PackageID: pkg.ID,
					PhonemeID: id, Reason: "phoneme owned by multiple packages: " + existing,
				}
			}
			owner[id] = pkg.ID
		}
	}
	minFrame := sorted[0].StartFrame
	maxFrame := sorted[0].EndFrame
	for i, ph := range sorted {
		if ph.EndFrame <= ph.StartFrame {
			return domain.LipResult{}, &domain.Error{
				Code: domain.ErrOrder, DialogueID: dialogueID, PhonemeID: ph.PhonemeID,
				Field: "frame_interval", Reason: "invalid quantized interval",
			}
		}
		if i > 0 && ph.StartFrame < sorted[i-1].EndFrame {
			return domain.LipResult{}, &domain.Error{
				Code: domain.ErrOrder, DialogueID: dialogueID,
				PhonemeA: sorted[i-1].PhonemeID, PhonemeB: ph.PhonemeID,
				Field: "frame_interval", Reason: "overlap",
			}
		}
		if ph.StartFrame < minFrame {
			minFrame = ph.StartFrame
		}
		if ph.EndFrame > maxFrame {
			maxFrame = ph.EndFrame
		}
		if _, ok := owner[ph.PhonemeID]; !ok {
			return domain.LipResult{}, &domain.Error{
				Code: domain.ErrLip, DialogueID: dialogueID, PhonemeID: ph.PhonemeID,
				Reason: "phoneme has no package",
			}
		}
	}
	frames := make([]domain.FrameLip, maxFrame-minFrame)
	for i := range frames {
		frames[i] = domain.FrameLip{Frame: minFrame + i, Lip: domain.LipRest}
	}
	for _, ph := range sorted {
		for f := ph.StartFrame; f < ph.EndFrame; f++ {
			idx := f - minFrame
			frames[idx].Lip = domain.LipForCategory(ph.Category)
			frames[idx].PhonemeID = ph.PhonemeID
			frames[idx].PackageID = owner[ph.PhonemeID]
		}
	}

	transitions := make([]domain.TransitionInsertion, 0)
	usedTransitionFrame := make(map[int]domain.TransitionInsertion)
	for i := 1; i < len(sorted); i++ {
		prev, next := sorted[i-1], sorted[i]
		from := domain.LipForCategory(prev.Category)
		to := domain.LipForCategory(next.Category)
		distance := domain.FamilyDistance(from, to)
		if distance <= 1 {
			continue
		}
		bridges := bridgeSequence(from, to)
		if len(bridges) == 0 {
			continue
		}
		if len(bridges) > tolerance {
			return domain.LipResult{}, &domain.Error{
				Code: domain.ErrLip, DialogueID: dialogueID, PackageID: owner[next.PhonemeID],
				PhonemeID: next.PhonemeID, Frame: next.StartFrame, HaveFrame: true,
				LipA: from, LipB: to,
				Reason: fmt.Sprintf("required bridge frames %d exceed tolerance %d", len(bridges), tolerance),
			}
		}
		leftCount := len(bridges) / 2
		rightCount := len(bridges) - leftCount
		if next.StartFrame-prev.StartFrame < leftCount || next.EndFrame-next.StartFrame < rightCount {
			return domain.LipResult{}, &domain.Error{
				Code: domain.ErrLip, DialogueID: dialogueID, PackageID: owner[next.PhonemeID],
				PhonemeID: next.PhonemeID, Frame: next.StartFrame, HaveFrame: true,
				LipA: from, LipB: to, Reason: "insufficient frames for transition bridge",
			}
		}
		for j, bridge := range bridges {
			frame := next.StartFrame - leftCount + j
			if prev, exists := usedTransitionFrame[frame]; exists {
				return domain.LipResult{}, &domain.Error{
					Code: domain.ErrLip, DialogueID: dialogueID, PackageID: prev.PackageID,
					PhonemeID: next.PhonemeID, Frame: frame, HaveFrame: true,
					LipA: prev.Bridge, LipB: bridge, Reason: "overlapping transition insertions",
				}
			}
			idx := frame - minFrame
			if idx < 0 || idx >= len(frames) {
				return domain.LipResult{}, &domain.Error{
					Code: domain.ErrLip, DialogueID: dialogueID, PackageID: owner[next.PhonemeID],
					PhonemeID: next.PhonemeID, Frame: frame, HaveFrame: true,
					LipA: from, LipB: to, Reason: "transition outside rendered frame range",
				}
			}
			insertion := domain.TransitionInsertion{
				DialogueID: dialogueID, PackageID: frames[idx].PackageID, Frame: frame,
				From: from, To: to, Bridge: bridge,
				Basis: fmt.Sprintf("family_distance=%d tolerance=%d bridge=%d/%d", distance, tolerance, j+1, len(bridges)),
			}
			frames[idx].Lip = bridge
			frames[idx].Transition = true
			frames[idx].TransitionFrom = from
			frames[idx].TransitionTo = to
			usedTransitionFrame[frame] = insertion
			transitions = append(transitions, insertion)
		}
	}
	sort.Slice(transitions, func(i, j int) bool {
		if transitions[i].Frame != transitions[j].Frame {
			return transitions[i].Frame < transitions[j].Frame
		}
		return transitions[i].PackageID < transitions[j].PackageID
	})
	return domain.LipResult{
		DialogueID: dialogueID, Packages: packages,
		Frames: frames, Transitions: transitions,
	}, nil
}

func bridgeSequence(from, to string) []string {
	a, b := domain.LipFamilyRank(from), domain.LipFamilyRank(to)
	if a < 0 || b < 0 || a == b {
		return nil
	}
	var out []string
	if a < b {
		for rank := a + 1; rank < b; rank++ {
			out = append(out, lipForRank(rank))
		}
	} else {
		for rank := a - 1; rank > b; rank-- {
			out = append(out, lipForRank(rank))
		}
	}
	return out
}

func lipForRank(rank int) string {
	switch rank {
	case 0:
		return domain.LipRest
	case 1:
		return domain.LipClosed
	case 2:
		return domain.LipOpen
	default:
		return ""
	}
}
