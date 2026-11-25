package dressbatch

import (
	"fmt"
	"sort"
)

// EvaluateAssembly is a pure function over the project definition and the
// candidate set. It does not use caches and canonicalizes all ordering.
func EvaluateAssembly(project *Project, candidatePartIDs []string) (*AssemblyResult, error) {
	byID := partMap(project)
	unique := sortedUnique(candidatePartIDs)
	for _, partID := range unique {
		if _, exists := byID[partID]; !exists {
			return nil, newError(CodeSchema, "assembly", "candidate_part_ids", "referenced part %q does not exist", partID)
		}
	}
	parts := make([]Part, 0, len(unique))
	for _, partID := range unique {
		parts = append(parts, byID[partID])
	}

	rulesByPair := make(map[string][]MutualExclusion, len(project.MutualExclusions))
	for _, rule := range project.MutualExclusions {
		key := orderedPair(rule.A, rule.B)
		rulesByPair[key] = append(rulesByPair[key], rule)
	}
	for key := range rulesByPair {
		sort.Slice(rulesByPair[key], func(i, j int) bool {
			return rulesByPair[key][i].ID < rulesByPair[key][j].ID
		})
	}

	conflicts := make([]Conflict, 0)
	for i := 0; i < len(unique); i++ {
		for j := i + 1; j < len(unique); j++ {
			a, b := byID[unique[i]], byID[unique[j]]
			pair := orderedPair(a.ID, b.ID)
			if a.MountID == b.MountID {
				conflicts = append(conflicts, Conflict{
					Category: ConflictMount,
					PartIDs:  []string{a.ID, b.ID},
					MountID:  a.MountID,
					Detail:   fmt.Sprintf("parts %s and %s both occupy mount %s", a.ID, b.ID, a.MountID),
				})
			}
			for _, rule := range rulesByPair[pair] {
				conflicts = append(conflicts, Conflict{
					Category: ConflictRule,
					PartIDs:  []string{a.ID, b.ID},
					RuleID:   rule.ID,
					Detail:   nonEmpty(rule.Reason, fmt.Sprintf("parts %s and %s are explicitly mutually exclusive", a.ID, b.ID)),
				})
			}
			if a.Skin && b.Skin && a.SkinGroup != b.SkinGroup {
				conflicts = append(conflicts, Conflict{
					Category: ConflictSkin,
					PartIDs:  []string{a.ID, b.ID},
					Detail:   fmt.Sprintf("skinned parts use incompatible skin groups %q and %q", a.SkinGroup, b.SkinGroup),
				})
			}
		}
	}
	sortConflicts(conflicts)

	packOrder := append([]Part(nil), parts...)
	sort.Slice(packOrder, func(i, j int) bool {
		return partLess(packOrder[i], packOrder[j])
	})
	accepted := make([]string, 0, len(parts))
	for _, candidate := range packOrder {
		compatible := true
		for _, acceptedID := range accepted {
			if pairHasConflict(byID[candidate.ID], byID[acceptedID], rulesByPair) {
				compatible = false
				break
			}
		}
		if compatible {
			accepted = append(accepted, candidate.ID)
		}
	}
	sort.Strings(accepted)
	acceptedSet := make(map[string]struct{}, len(accepted))
	for _, partID := range accepted {
		acceptedSet[partID] = struct{}{}
	}
	rejected := make([]string, 0, len(unique)-len(accepted))
	for _, partID := range unique {
		if _, exists := acceptedSet[partID]; !exists {
			rejected = append(rejected, partID)
		}
	}
	return &AssemblyResult{
		CandidatePartIDs: unique,
		AcceptedPartIDs:  accepted,
		RejectedPartIDs:  rejected,
		Legal:            len(conflicts) == 0,
		Conflicts:        conflicts,
	}, nil
}

func pairHasConflict(a, b Part, rulesByPair map[string][]MutualExclusion) bool {
	if a.MountID == b.MountID {
		return true
	}
	if len(rulesByPair[orderedPair(a.ID, b.ID)]) > 0 {
		return true
	}
	if a.Skin && b.Skin && a.SkinGroup != b.SkinGroup {
		return true
	}
	return false
}

func sortConflicts(conflicts []Conflict) {
	rank := map[string]int{
		ConflictMount: 0,
		ConflictRule:  1,
		ConflictSkin:  2,
	}
	sort.Slice(conflicts, func(i, j int) bool {
		if rank[conflicts[i].Category] != rank[conflicts[j].Category] {
			return rank[conflicts[i].Category] < rank[conflicts[j].Category]
		}
		if conflicts[i].PartIDs[0] != conflicts[j].PartIDs[0] {
			return conflicts[i].PartIDs[0] < conflicts[j].PartIDs[0]
		}
		if conflicts[i].PartIDs[1] != conflicts[j].PartIDs[1] {
			return conflicts[i].PartIDs[1] < conflicts[j].PartIDs[1]
		}
		if conflicts[i].RuleID != conflicts[j].RuleID {
			return conflicts[i].RuleID < conflicts[j].RuleID
		}
		return conflicts[i].MountID < conflicts[j].MountID
	})
}

func partLess(a, b Part) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.ID < b.ID
}

func nonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
