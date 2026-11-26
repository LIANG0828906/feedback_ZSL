package budget

import (
	"fmt"
	"sort"
	"strings"
)

// OrderedIntent is an intent after deterministic arbitration sorting.
type OrderedIntent struct {
	Intent Intent
	Skill  Skill
}

// OrderTickIntents establishes the explainable total order:
//  1. lower Skill.Priority wins;
//  2. earlier source sequence ordinal wins;
//  3. lexicographic skill ID and intent ID are final map-iteration guards.
//
// In StrictPriorityTies mode, equal explicit priorities are not guessed and
// produce E_ARB with the complete contested set.
func OrderTickIntents(intents []Intent, project *Project, params Params) ([]OrderedIntent, *Error) {
	items := make([]OrderedIntent, 0, len(intents))
	for _, intent := range intents {
		skill, ok := project.skillByID(intent.SkillID)
		if !ok {
			return nil, &Error{
				Code: ECodeSchema, Message: "intent references unknown skill",
				Tick: &intent.Tick, SkillID: intent.SkillID, Field: "intents.skill_id",
			}
		}
		items = append(items, OrderedIntent{Intent: intent, Skill: skill})
	}

	if params.StrictPriorityTies {
		byPriority := map[int][]string{}
		for _, item := range items {
			byPriority[item.Skill.Priority] = append(byPriority[item.Skill.Priority], item.Skill.ID)
		}
		var tied []string
		for _, ids := range byPriority {
			if len(ids) > 1 {
				tied = append(tied, ids...)
			}
		}
		if len(tied) > 0 {
			sort.Strings(tied)
			tick := intents[0].Tick
			return nil, &Error{
				Code:    ECodeARB,
				Message: "ambiguous equal arbitration priorities under strict policy",
				Tick:    &tick, Contested: uniqueSorted(tied),
				Basis: "policy=strict_priority; tie_break=disabled",
			}
		}
	}

	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Skill.Priority != b.Skill.Priority {
			return a.Skill.Priority < b.Skill.Priority
		}
		if a.Intent.Ordinal != b.Intent.Ordinal {
			return a.Intent.Ordinal < b.Intent.Ordinal
		}
		if a.Skill.ID != b.Skill.ID {
			return a.Skill.ID < b.Skill.ID
		}
		return a.Intent.IntentID < b.Intent.IntentID
	})
	return items, nil
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func contentionFor(item OrderedIntent, project *Project, tickIntents []Intent) string {
	groupCount := 0
	resourceUsers := 0
	for _, intent := range tickIntents {
		skill, ok := project.skillByID(intent.SkillID)
		if !ok {
			continue
		}
		if skill.GroupID != "" && skill.GroupID == item.Skill.GroupID {
			groupCount++
		}
		if skill.Cost > 0 {
			resourceUsers++
		}
	}
	if item.Skill.GroupID != "" && groupCount > 1 {
		return "shared_cooldown_group"
	}
	if resourceUsers > 1 {
		return "shared_resource_pool"
	}
	return "none"
}

func basisFor(item OrderedIntent, extra ...string) string {
	parts := []string{
		fmt.Sprintf("priority=%d", item.Skill.Priority),
		fmt.Sprintf("source_ordinal=%d", item.Intent.Ordinal),
		fmt.Sprintf("skill_id=%s", item.Skill.ID),
	}
	parts = append(parts, extra...)
	return strings.Join(parts, "; ")
}
