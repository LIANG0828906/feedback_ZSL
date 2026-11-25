package assembly

import (
	"fmt"
	"sort"
	"strings"

	"wardrobe/domain"
)

type UnknownPartError struct {
	PartIDs []string
}

func (e *UnknownPartError) Error() string {
	return "unknown parts: " + strings.Join(e.PartIDs, ", ")
}

type Result struct {
	SelectedPartIDs []string
	AcceptedPartIDs []string
	Valid           bool
	Conflicts       []domain.Conflict
}

// Evaluate is a pure, order-independent assembly legality check. The accepted
// set is a deterministic maximal legal subset selected by batch priority then
// id, while the conflict list describes every direct conflict in the request.
func Evaluate(project domain.Project, selectedPartIDs []string) (Result, error) {
	selected := domain.SortedUnique(selectedPartIDs)
	partMap := project.PartMap()
	unknown := make([]string, 0)
	for _, id := range selected {
		if _, ok := partMap[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return Result{}, &UnknownPartError{PartIDs: unknown}
	}

	conflicts := detectConflicts(project, selected)
	ordered := make([]string, 0, len(selected))
	for _, id := range selected {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return domain.PartOrderLess(partMap[ordered[i]], partMap[ordered[j]])
	})

	accepted := make([]string, 0, len(ordered))
	for _, candidate := range ordered {
		trial := append(append([]string(nil), accepted...), candidate)
		if len(detectConflicts(project, trial)) == 0 {
			accepted = append(accepted, candidate)
		}
	}
	sort.Strings(accepted)
	return Result{
		SelectedPartIDs: selected,
		AcceptedPartIDs: accepted,
		Valid:           len(conflicts) == 0,
		Conflicts:       conflicts,
	}, nil
}

func detectConflicts(project domain.Project, selected []string) []domain.Conflict {
	partMap := project.PartMap()
	mountMap := project.MountMap()
	selectedSet := make(map[string]struct{}, len(selected))
	for _, id := range selected {
		selectedSet[id] = struct{}{}
	}
	conflicts := make([]domain.Conflict, 0)

	byMount := make(map[string][]string)
	for _, id := range selected {
		part, ok := partMap[id]
		if !ok {
			continue
		}
		byMount[part.MountID] = append(byMount[part.MountID], id)
	}
	mountIDs := make([]string, 0, len(byMount))
	for mountID := range byMount {
		mountIDs = append(mountIDs, mountID)
	}
	sort.Strings(mountIDs)
	for _, mountID := range mountIDs {
		ids := domain.SortedUnique(byMount[mountID])
		if len(ids) < 2 {
			continue
		}
		for left := 0; left < len(ids); left++ {
			for right := left + 1; right < len(ids); right++ {
				pair := []string{ids[left], ids[right]}
				conflicts = append(conflicts, domain.Conflict{
					Category: domain.ConflictMountContention,
					MountID:  mountID,
					PartIDs:  pair,
					Message:  fmt.Sprintf("mount %s is occupied by both %s and %s", mountID, pair[0], pair[1]),
				})
			}
		}
	}

	rules := append([]domain.ExclusionRule(nil), project.ExclusionRules...)
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	for _, rule := range rules {
		_, hasA := selectedSet[rule.PartA]
		_, hasB := selectedSet[rule.PartB]
		if !hasA || !hasB {
			continue
		}
		pair := domain.SortedUnique([]string{rule.PartA, rule.PartB})
		conflicts = append(conflicts, domain.Conflict{
			Category: domain.ConflictExplicitExclude,
			PartIDs:  pair,
			RuleID:   rule.ID,
			Message:  fmt.Sprintf("exclusion rule %s forbids %s with %s: %s", rule.ID, pair[0], pair[1], rule.Description),
		})
	}

	sortedSelected := append([]string(nil), selected...)
	sort.Strings(sortedSelected)
	for _, id := range sortedSelected {
		part := partMap[id]
		mount := mountMap[part.MountID]
		compatible := true
		switch mount.SkinMode {
		case "skinned":
			compatible = part.Skinned
		case "rigid":
			compatible = !part.Skinned
		}
		if compatible {
			continue
		}
		conflicts = append(conflicts, domain.Conflict{
			Category: domain.ConflictSkinIncompatible,
			MountID:  mount.ID,
			PartIDs:  []string{part.ID},
			Message:  fmt.Sprintf("part %s skinning=%t is incompatible with mount %s skinMode=%s", part.ID, part.Skinned, mount.ID, mount.SkinMode),
		})
	}

	sort.Slice(conflicts, func(i, j int) bool {
		return conflictOrderKey(conflicts[i]) < conflictOrderKey(conflicts[j])
	})
	return conflicts
}

func conflictOrderKey(conflict domain.Conflict) string {
	return strings.Join([]string{
		conflict.Category,
		conflict.MountID,
		conflict.RuleID,
		strings.Join(conflict.PartIDs, ","),
		conflict.Message,
	}, "\x00")
}
