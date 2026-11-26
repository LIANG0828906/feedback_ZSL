// Package assembly contains the order-independent legality kernel.
package assembly

import (
	"fmt"
	"sort"
	"strings"

	"wardrobekernel/internal/model"
	"wardrobekernel/internal/xerr"
)

// Evaluate determines the conflict-free legal subset of candidateIDs.
//
// The total adjudication order is (arbitration_priority DESC, id ASC). Parts
// are considered in that order and accepted only when they do not conflict
// with an already accepted part. Consequently the result is independent of
// the order or concurrency schedule of candidateIDs.
func Evaluate(project *model.Project, candidateIDs []string) (*model.AssemblyResult, error) {
	parts := make(map[string]model.Part, len(project.Parts))
	for _, part := range project.Parts {
		parts[part.ID] = part
	}

	unique := make(map[string]struct{}, len(candidateIDs))
	ids := make([]string, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		if id == "" {
			continue
		}
		if _, ok := unique[id]; ok {
			continue
		}
		unique[id] = struct{}{}
		if _, ok := parts[id]; !ok {
			return nil, &xerr.Error{Code: xerr.ESchema, Message: "unknown candidate part " + id}
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	ranked := append([]string(nil), ids...)
	sort.SliceStable(ranked, func(i, j int) bool {
		pi, pj := parts[ranked[i]], parts[ranked[j]]
		if pi.ArbitrationPriority != pj.ArbitrationPriority {
			return pi.ArbitrationPriority > pj.ArbitrationPriority
		}
		return pi.ID < pj.ID
	})

	accepted := make([]model.Part, 0, len(ranked))
	acceptedIDs := make(map[string]struct{}, len(ranked))
	conflicts := make([]model.Conflict, 0)

	for _, id := range ranked {
		candidate := parts[id]
		blockerFacts := findBlockers(candidate, accepted, project.MutexRules)
		conflictsForCandidate := principalConflicts(candidate.ID, blockerFacts)
		if len(conflictsForCandidate) == 0 {
			accepted = append(accepted, candidate)
			acceptedIDs[id] = struct{}{}
			continue
		}
		conflicts = append(conflicts, conflictsForCandidate...)
	}

	legal := make([]string, 0, len(acceptedIDs))
	for id := range acceptedIDs {
		legal = append(legal, id)
	}
	sort.Strings(legal)
	sortConflicts(conflicts)

	return &model.AssemblyResult{
		CandidateIDs:  ids,
		Legal:         len(conflicts) == 0,
		LegalAssembly: legal,
		Conflicts:     conflicts,
	}, nil
}

func principalConflicts(candidateID string, blockers []blocker) []model.Conflict {
	out := make([]model.Conflict, 0)
	var attach *blocker
	var skin *blocker
	mutexByRule := map[string]blocker{}
	for i := range blockers {
		b := blockers[i]
		switch b.Category {
		case model.ConflictAttachPoint:
			if attach == nil || higherBlocker(b, *attach) {
				copy := b
				attach = &copy
			}
		case model.ConflictSkin:
			if skin == nil || higherBlocker(b, *skin) {
				copy := b
				skin = &copy
			}
		case model.ConflictMutex:
			existing, ok := mutexByRule[b.RuleID]
			if !ok || higherBlocker(b, existing) {
				mutexByRule[b.RuleID] = b
			}
		}
	}
	if attach != nil {
		out = append(out, attach.toConflict(candidateID))
	}
	if skin != nil {
		out = append(out, skin.toConflict(candidateID))
	}
	ruleIDs := make([]string, 0, len(mutexByRule))
	for ruleID := range mutexByRule {
		ruleIDs = append(ruleIDs, ruleID)
	}
	sort.Strings(ruleIDs)
	for _, ruleID := range ruleIDs {
		b := mutexByRule[ruleID]
		out = append(out, b.toConflict(candidateID))
	}
	return out
}

func higherBlocker(a, b blocker) bool {
	if a.Part.ArbitrationPriority != b.Part.ArbitrationPriority {
		return a.Part.ArbitrationPriority > b.Part.ArbitrationPriority
	}
	return a.Part.ID < b.Part.ID
}

type blocker struct {
	Part          model.Part
	Category      string
	AttachPointID string
	RuleID        string
	Message       string
}

func (b blocker) toConflict(loserID string) model.Conflict {
	ids := []string{b.Part.ID, loserID}
	sort.Strings(ids)
	code := b.Category
	return model.Conflict{
		Category:      b.Category,
		Code:          code,
		PartIDs:       ids,
		WinnerID:      b.Part.ID,
		LoserID:       loserID,
		AttachPointID: b.AttachPointID,
		RuleID:        b.RuleID,
		Message:       b.Message,
	}
}

func findBlockers(candidate model.Part, accepted []model.Part, rules []model.MutexRule) []blocker {
	out := make([]blocker, 0)
	for _, incumbent := range accepted {
		if candidate.AttachPointID != "" && candidate.AttachPointID == incumbent.AttachPointID {
			out = append(out, blocker{
				Part: incumbent, Category: model.ConflictAttachPoint,
				AttachPointID: candidate.AttachPointID,
				Message: fmt.Sprintf("parts %s and %s both occupy attach point %s",
					incumbent.ID, candidate.ID, candidate.AttachPointID),
			})
		}
		for _, rule := range rules {
			if ruleContains(rule.PartIDs, candidate.ID) && ruleContains(rule.PartIDs, incumbent.ID) {
				message := rule.Reason
				if message == "" {
					message = fmt.Sprintf("mutex rule %s forbids assembling %s and %s", rule.ID, incumbent.ID, candidate.ID)
				}
				out = append(out, blocker{
					Part: incumbent, Category: model.ConflictMutex, RuleID: rule.ID, Message: message,
				})
			}
		}
		if skinIncompatible(candidate, incumbent) {
			out = append(out, blocker{
				Part: incumbent, Category: model.ConflictSkin,
				Message: fmt.Sprintf("skinned groups %q and %q are incompatible", candidate.SkinGroup, incumbent.SkinGroup),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		if out[i].RuleID != out[j].RuleID {
			return out[i].RuleID < out[j].RuleID
		}
		return out[i].Part.ID < out[j].Part.ID
	})
	return out
}

func skinIncompatible(a, b model.Part) bool {
	return a.Skinned && b.Skinned && a.SkinGroup != "" && b.SkinGroup != "" && a.SkinGroup != b.SkinGroup
}

func ruleContains(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func sortConflicts(conflicts []model.Conflict) {
	sort.SliceStable(conflicts, func(i, j int) bool {
		ki := conflictKey(conflicts[i])
		kj := conflictKey(conflicts[j])
		return ki < kj
	})
}

func conflictKey(c model.Conflict) string {
	return strings.Join([]string{
		c.Category, c.WinnerID, c.LoserID, c.AttachPointID, c.RuleID,
	}, "|")
}
