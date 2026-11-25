package generator

import (
	"fmt"
	"sort"
	"strings"

	"wardrobe/domain"
)

// The reference functions below intentionally do not import the production
// assembly or planner packages. They are the generator's independent,
// deterministic construction used to write the truth section of the sample.

type referenceAssemblyResult struct {
	SelectedPartIDs []string
	AcceptedPartIDs []string
	Valid           bool
	Conflicts       []domain.Conflict
}

func referenceEvaluate(project domain.Project, selectedPartIDs []string) referenceAssemblyResult {
	selected := domain.SortedUnique(selectedPartIDs)
	partMap := project.PartMap()
	conflicts := referenceConflicts(project, selected)
	ordered := append([]string(nil), selected...)
	sort.Slice(ordered, func(i, j int) bool {
		return domain.PartOrderLess(partMap[ordered[i]], partMap[ordered[j]])
	})
	accepted := make([]string, 0, len(ordered))
	for _, candidate := range ordered {
		trial := append(append([]string(nil), accepted...), candidate)
		if len(referenceConflicts(project, trial)) == 0 {
			accepted = append(accepted, candidate)
		}
	}
	sort.Strings(accepted)
	return referenceAssemblyResult{
		SelectedPartIDs: selected,
		AcceptedPartIDs: accepted,
		Valid:           len(conflicts) == 0,
		Conflicts:       conflicts,
	}
}

func referenceConflicts(project domain.Project, selected []string) []domain.Conflict {
	partMap := project.PartMap()
	mountMap := project.MountMap()
	selectedSet := make(map[string]struct{}, len(selected))
	for _, id := range selected {
		selectedSet[id] = struct{}{}
	}
	conflicts := make([]domain.Conflict, 0)
	byMount := make(map[string][]string)
	for _, id := range selected {
		part := partMap[id]
		byMount[part.MountID] = append(byMount[part.MountID], id)
	}
	mountIDs := make([]string, 0, len(byMount))
	for mountID := range byMount {
		mountIDs = append(mountIDs, mountID)
	}
	sort.Strings(mountIDs)
	for _, mountID := range mountIDs {
		ids := domain.SortedUnique(byMount[mountID])
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
		if !compatible {
			conflicts = append(conflicts, domain.Conflict{
				Category: domain.ConflictSkinIncompatible,
				MountID:  mount.ID,
				PartIDs:  []string{part.ID},
				Message:  fmt.Sprintf("part %s skinning=%t is incompatible with mount %s skinMode=%s", part.ID, part.Skinned, mount.ID, mount.SkinMode),
			})
		}
	}
	sort.Slice(conflicts, func(i, j int) bool {
		return referenceConflictKey(conflicts[i]) < referenceConflictKey(conflicts[j])
	})
	return conflicts
}

func referenceConflictKey(conflict domain.Conflict) string {
	return strings.Join([]string{
		conflict.Category,
		conflict.MountID,
		conflict.RuleID,
		strings.Join(conflict.PartIDs, ","),
		conflict.Message,
	}, "\x00")
}

type referenceBatch struct {
	result      domain.BatchResult
	parts       []domain.Part
	materialSet map[string]struct{}
	vertexUsed  int
	indexUsed   int
	allSkinned  bool
	hasParts    bool
}

type referenceBlocked struct {
	index       int
	blockers    []string
	skinOK      bool
	materialHit bool
}

type referencePlanResult struct {
	Batches        []domain.BatchResult
	Decisions      []domain.Decision
	CommitSequence []string
}

func referencePlan(project domain.Project, acceptedPartIDs []string) referencePlanResult {
	partMap := project.PartMap()
	orderedIDs := domain.NormalizeIDs(acceptedPartIDs)
	sort.Slice(orderedIDs, func(i, j int) bool {
		return domain.PartOrderLess(partMap[orderedIDs[i]], partMap[orderedIDs[j]])
	})
	batches := make([]referenceBatch, 0, len(orderedIDs))
	decisions := make([]domain.Decision, 0, len(orderedIDs))
	for _, partID := range orderedIDs {
		part := partMap[partID]
		decision := referencePlace(project, part, &batches)
		decision.Sequence = len(decisions) + 1
		decisions = append(decisions, decision)
	}
	for index := range batches {
		batches[index].result.Ordinal = index + 1
		batches[index].result.CommitSequence = index + 1
	}
	out := referencePlanResult{Decisions: decisions}
	for _, batch := range batches {
		out.Batches = append(out.Batches, batch.result)
		out.CommitSequence = append(out.CommitSequence, batch.result.ID)
	}
	return out
}

func referencePlace(project domain.Project, part domain.Part, batches *[]referenceBatch) domain.Decision {
	partSlots := referenceMaterialSlots(part)
	blockedBy := make([]string, 0)
	blocked := make([]referenceBlocked, 0, len(*batches))
	for index := range *batches {
		batch := &(*batches)[index]
		blockers := referenceBlockers(project, *batch, part, partSlots)
		if len(blockers) == 0 {
			decision := domain.Decision{
				PartID:                 part.ID,
				BatchID:                batch.result.ID,
				BatchOrdinal:           batch.result.Ordinal,
				ConsideredBatchID:      batch.result.ID,
				ConsideredBatchOrdinal: batch.result.Ordinal,
				Result:                 domain.DecisionReuse,
				Reason:                 domain.ReasonBudgetFit,
			}
			if referenceSharesMaterial(*batch, partSlots) {
				decision.Reason = domain.ReasonMaterialAffinity
				decision.Detail = fmt.Sprintf("part %s joined %s because it shares material slot(s) %s and fits all budgets",
					part.ID, batch.result.ID, strings.Join(partSlots, ","))
			} else {
				decision.Detail = fmt.Sprintf("part %s joined %s because all budgets and skin policy fit", part.ID, batch.result.ID)
			}
			referenceAppend(batch, part)
			return decision
		}
		skinOK := project.Constraints.AllowCrossSkin || !batch.hasParts || batch.allSkinned == part.Skinned
		blocked = append(blocked, referenceBlocked{
			index:       index,
			blockers:    blockers,
			skinOK:      skinOK,
			materialHit: referenceSharesMaterial(*batch, partSlots),
		})
		for _, blocker := range blockers {
			blockedBy = append(blockedBy, batch.result.ID+":"+blocker)
		}
	}
	newOrdinal := len(*batches) + 1
	newBatch := referenceNewBatch(newOrdinal)
	chosen := referenceChoose(blocked)
	primaryReason := domain.ReasonInitialBatch
	consideredID := ""
	consideredOrdinal := 0
	if chosen != nil {
		consideredID = (*batches)[chosen.index].result.ID
		consideredOrdinal = (*batches)[chosen.index].result.Ordinal
		primaryReason = chosen.blockers[0]
	}
	referenceAppend(&newBatch, part)
	*batches = append(*batches, newBatch)
	detail := fmt.Sprintf("part %s opened %s; no existing batch accepted it", part.ID, newBatch.result.ID)
	if len(blockedBy) > 0 {
		detail += "; blockers: " + strings.Join(blockedBy, ", ")
	}
	return domain.Decision{
		PartID:                 part.ID,
		BatchID:                newBatch.result.ID,
		BatchOrdinal:           newOrdinal,
		ConsideredBatchID:      consideredID,
		ConsideredBatchOrdinal: consideredOrdinal,
		Result:                 domain.DecisionSplit,
		Reason:                 primaryReason,
		BlockedBy:              blockedBy,
		Detail:                 detail,
	}
}

func referenceChoose(blocked []referenceBlocked) *referenceBlocked {
	if len(blocked) == 0 {
		return nil
	}
	for index := range blocked {
		if blocked[index].skinOK && blocked[index].materialHit {
			return &blocked[index]
		}
	}
	for index := range blocked {
		if blocked[index].skinOK {
			return &blocked[index]
		}
	}
	return &blocked[0]
}

func referenceBlockers(project domain.Project, batch referenceBatch, part domain.Part, partSlots []string) []string {
	blockers := make([]string, 0, 4)
	if !project.Constraints.AllowCrossSkin && batch.hasParts && batch.allSkinned != part.Skinned {
		blockers = append(blockers, domain.ReasonSkinSeparation)
	}
	nextSlots := len(batch.materialSet)
	for _, slot := range partSlots {
		if _, exists := batch.materialSet[slot]; !exists {
			nextSlots++
		}
	}
	if nextSlots > project.Constraints.MaxMaterialSlots {
		blockers = append(blockers, domain.ReasonMaterialPressure)
	}
	if batch.vertexUsed+part.Mesh.VertexCount > project.Constraints.MaxVertices {
		blockers = append(blockers, domain.ReasonVertexPressure)
	}
	if batch.indexUsed+part.Mesh.TriangleCount*3 > project.Constraints.MaxIndices {
		blockers = append(blockers, domain.ReasonIndexPressure)
	}
	return blockers
}

func referenceNewBatch(ordinal int) referenceBatch {
	return referenceBatch{
		result: domain.BatchResult{
			ID:             fmt.Sprintf("B%03d", ordinal),
			Ordinal:        ordinal,
			CommitSequence: ordinal,
			PartIDs:        []string{},
			Parts:          []domain.PartPlacement{},
			MaterialSlots:  []string{},
		},
		materialSet: make(map[string]struct{}),
	}
}

func referenceAppend(batch *referenceBatch, part domain.Part) {
	vertexOffset := batch.vertexUsed
	indexOffset := batch.indexUsed
	placement := domain.PartPlacement{
		PartID:       part.ID,
		VertexOffset: vertexOffset,
		IndexOffset:  indexOffset,
		Submeshes:    make([]domain.SubmeshPlacement, 0, len(part.Mesh.Submeshes)),
	}
	for _, submesh := range part.Mesh.Submeshes {
		placement.Submeshes = append(placement.Submeshes, domain.SubmeshPlacement{
			SubmeshID:    submesh.ID,
			MaterialSlot: submesh.MaterialSlot,
			VertexStart:  submesh.VertexStart,
			VertexCount:  submesh.VertexCount,
			IndexStart:   submesh.IndexStart,
			IndexCount:   submesh.IndexCount,
			VertexOffset: vertexOffset + submesh.VertexStart,
			IndexOffset:  indexOffset + submesh.IndexStart,
		})
		batch.materialSet[submesh.MaterialSlot] = struct{}{}
	}
	batch.parts = append(batch.parts, part)
	batch.result.PartIDs = append(batch.result.PartIDs, part.ID)
	batch.result.Parts = append(batch.result.Parts, placement)
	batch.vertexUsed += part.Mesh.VertexCount
	batch.indexUsed += part.Mesh.TriangleCount * 3
	if !batch.hasParts {
		batch.allSkinned = part.Skinned
		batch.hasParts = true
	}
	batch.result.VertexUsed = batch.vertexUsed
	batch.result.IndexUsed = batch.indexUsed
	slots := make([]string, 0, len(batch.materialSet))
	for slot := range batch.materialSet {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	batch.result.MaterialSlots = slots
}

func referenceMaterialSlots(part domain.Part) []string {
	slots := make([]string, 0, len(part.Mesh.Submeshes))
	for _, submesh := range part.Mesh.Submeshes {
		slots = append(slots, submesh.MaterialSlot)
	}
	return domain.SortedUnique(slots)
}

func referenceSharesMaterial(batch referenceBatch, slots []string) bool {
	for _, slot := range slots {
		if _, ok := batch.materialSet[slot]; ok {
			return true
		}
	}
	return false
}
