package dressbatch

import (
	"fmt"
	"sort"
)

const (
	ReasonPrioritySeed = "new_batch_priority_seed"
	ReasonFits         = "fits_material_vertex_index_and_skin"
	ReasonMaterial     = "material_slot_limit"
	ReasonVertex       = "vertex_budget_limit"
	ReasonIndex        = "index_budget_limit"
	ReasonSkin         = "cross_skin_not_allowed"
)

type batchWork struct {
	ID         string
	Index      int
	Parts      []Part
	Decisions  []Decision
	Slots      []string
	SkinGroups []string
	Skin       bool
	MixedSkin  bool
	VertexUsed int
	IndexUsed  int
}

// Pack computes a deterministic partition for a legal assembly set.
// A legal set is required; use EvaluateAssembly first when conflicts are
// possible. The result is a pure function of the project and candidate set.
func Pack(project *Project, candidatePartIDs []string) (*PackResult, error) {
	assembly, err := EvaluateAssembly(project, candidatePartIDs)
	if err != nil {
		return nil, err
	}
	if !assembly.Legal {
		domainErr := withDetails(
			newError(CodeAssembly, "assembly", "candidate_part_ids", "cannot pack an assembly containing conflicts"),
			map[string]any{"conflicts": assembly.Conflicts},
		)
		return nil, domainErr
	}
	for _, partID := range assembly.AcceptedPartIDs {
		if err := packSinglePartCheck(project, partID); err != nil {
			return nil, err
		}
	}

	works, err := composeBatches(project, assembly.AcceptedPartIDs)
	if err != nil {
		return nil, err
	}
	batches := make([]Batch, 0, len(works))
	upstreamKey := ""
	for _, work := range works {
		batch := materializeBatch(project, work, upstreamKey)
		upstreamKey = batch.CacheKey
		batches = append(batches, batch)
	}
	commitSequence := make([]string, 0, len(batches))
	for _, batch := range batches {
		commitSequence = append(commitSequence, batch.ID)
	}
	return &PackResult{
		ProjectID:         project.ID,
		CandidatePartIDs:  assembly.CandidatePartIDs,
		AcceptedPartIDs:   assembly.AcceptedPartIDs,
		Batches:           batches,
		CommitSequence:    commitSequence,
		Conflicts:         []Conflict{},
		ConstraintVersion: ConstraintVersion(project.Constraints),
		MaterialVersion:   AllMaterialVersion(project),
		AlgorithmVersion:  AlgorithmVersion,
	}, nil
}

func packSinglePartCheck(project *Project, partID string) error {
	byID := partMap(project)
	part, exists := byID[partID]
	if !exists {
		return newError(CodeSchema, fmt.Sprintf("part:%s", partID), "id", "referenced part does not exist")
	}
	constraints := project.Constraints
	if part.VertexCount > constraints.MaxVerticesPerBatch ||
		part.EffectiveIndexCount() > constraints.MaxIndicesPerBatch ||
		len(part.MaterialSlots) > constraints.MaxMaterialSlotsPerBatch {
		return withDetails(
			newError(CodePack, part.ID, "constraints", "part cannot fit into an empty batch"),
			map[string]any{
				"batch_id":                     "b0001",
				"remaining_material_slots":     constraints.MaxMaterialSlotsPerBatch,
				"remaining_vertex_budget":      constraints.MaxVerticesPerBatch,
				"remaining_index_budget":       constraints.MaxIndicesPerBatch,
				"part_material_slots":          sortedCopy(part.MaterialSlots),
				"part_vertex_count":            part.VertexCount,
				"part_index_count":             part.EffectiveIndexCount(),
				"max_material_slots_per_batch": constraints.MaxMaterialSlotsPerBatch,
				"max_vertices_per_batch":       constraints.MaxVerticesPerBatch,
				"max_indices_per_batch":        constraints.MaxIndicesPerBatch,
			},
		)
	}
	return nil
}

func composeBatches(project *Project, acceptedPartIDs []string) ([]batchWork, error) {
	return composeBatchesFrom(project, acceptedPartIDs, nil, 1, 0)
}

func composeBatchesFrom(project *Project, acceptedPartIDs []string, consumed map[string]struct{}, startIndex, decisionStart int) ([]batchWork, error) {
	byID := partMap(project)
	remaining := make([]Part, 0, len(acceptedPartIDs))
	for _, partID := range acceptedPartIDs {
		if _, alreadyConsumed := consumed[partID]; !alreadyConsumed {
			remaining = append(remaining, byID[partID])
		}
	}
	sort.Slice(remaining, func(i, j int) bool {
		return partLess(remaining[i], remaining[j])
	})

	works := make([]batchWork, 0)
	decisionSequence := decisionStart
	for len(remaining) > 0 {
		index := startIndex + len(works)
		batchID := fmt.Sprintf("b%04d", index)
		seed := remaining[0]
		slots := make(map[string]struct{}, len(seed.MaterialSlots))
		for _, slot := range seed.MaterialSlots {
			slots[slot] = struct{}{}
		}
		work := batchWork{
			ID:         batchID,
			Index:      index,
			Parts:      []Part{seed},
			VertexUsed: seed.VertexCount,
			IndexUsed:  seed.EffectiveIndexCount(),
		}
		decisionSequence++
		work.Decisions = append(work.Decisions, Decision{
			Sequence:               decisionSequence,
			BatchID:                batchID,
			PartID:                 seed.ID,
			Action:                 DecisionSplit,
			Reason:                 ReasonPrioritySeed,
			RemainingMaterialSlots: project.Constraints.MaxMaterialSlotsPerBatch - len(slots),
			RemainingVertexBudget:  project.Constraints.MaxVerticesPerBatch - seed.VertexCount,
			RemainingIndexBudget:   project.Constraints.MaxIndicesPerBatch - seed.EffectiveIndexCount(),
			PartVertexCount:        seed.VertexCount,
			PartIndexCount:         seed.EffectiveIndexCount(),
		})

		kept := make([]Part, 0, len(remaining)-1)
		for _, candidate := range remaining[1:] {
			unionSlots := unionSlotSet(slots, candidate.MaterialSlots)
			remainingMaterial := project.Constraints.MaxMaterialSlotsPerBatch - len(slots)
			remainingVertex := project.Constraints.MaxVerticesPerBatch - work.VertexUsed
			remainingIndex := project.Constraints.MaxIndicesPerBatch - work.IndexUsed
			materialFits := len(unionSlots) <= project.Constraints.MaxMaterialSlotsPerBatch
			vertexFits := candidate.VertexCount <= remainingVertex
			indexFits := candidate.EffectiveIndexCount() <= remainingIndex
			skinFits := project.Constraints.AllowCrossSkin || (len(work.Parts) > 0 && work.Parts[0].Skin == candidate.Skin)
			decision := Decision{
				Sequence:               decisionSequence + 1,
				BatchID:                batchID,
				PartID:                 candidate.ID,
				RemainingMaterialSlots: remainingMaterial,
				RemainingVertexBudget:  remainingVertex,
				RemainingIndexBudget:   remainingIndex,
				PartVertexCount:        candidate.VertexCount,
				PartIndexCount:         candidate.EffectiveIndexCount(),
			}
			switch {
			case !skinFits:
				decision.Action = DecisionDefer
				decision.Reason = ReasonSkin
			case !materialFits:
				decision.Action = DecisionDefer
				decision.Reason = ReasonMaterial
			case !vertexFits:
				decision.Action = DecisionDefer
				decision.Reason = ReasonVertex
			case !indexFits:
				decision.Action = DecisionDefer
				decision.Reason = ReasonIndex
			default:
				decision.Action = DecisionPlace
				decision.Reason = ReasonFits
				work.Parts = append(work.Parts, candidate)
				work.VertexUsed += candidate.VertexCount
				work.IndexUsed += candidate.EffectiveIndexCount()
				for slot := range unionSlots {
					slots[slot] = struct{}{}
				}
			}
			decisionSequence++
			work.Decisions = append(work.Decisions, decision)
			if decision.Action != DecisionPlace {
				kept = append(kept, candidate)
			}
		}
		work.Slots = make([]string, 0, len(slots))
		for slot := range slots {
			work.Slots = append(work.Slots, slot)
		}
		sort.Strings(work.Slots)
		work.Skin, work.MixedSkin, work.SkinGroups = skinSummary(work.Parts)
		works = append(works, work)
		remaining = kept
	}
	return works, nil
}

func unionSlotSet(existing map[string]struct{}, adding []string) map[string]struct{} {
	out := make(map[string]struct{}, len(existing)+len(adding))
	for slot := range existing {
		out[slot] = struct{}{}
	}
	for _, slot := range adding {
		out[slot] = struct{}{}
	}
	return out
}

func skinSummary(parts []Part) (bool, bool, []string) {
	skinned := 0
	rigid := 0
	groups := make([]string, 0)
	for _, part := range parts {
		if part.Skin {
			skinned++
			groups = append(groups, part.SkinGroup)
		} else {
			rigid++
		}
	}
	return skinned > 0 && rigid == 0, skinned > 0 && rigid > 0, sortedUnique(groups)
}

func materializeBatch(project *Project, work batchWork, upstreamKey string) Batch {
	batch := Batch{
		ID:            work.ID,
		Index:         work.Index,
		PartIDs:       make([]string, 0, len(work.Parts)),
		MaterialSlots: append([]string{}, work.Slots...),
		Skin:          work.Skin,
		MixedSkin:     work.MixedSkin,
		SkinGroups:    append([]string{}, work.SkinGroups...),
		VertexUsed:    work.VertexUsed,
		IndexUsed:     work.IndexUsed,
		Decisions:     append([]Decision(nil), work.Decisions...),
	}
	vertexCursor := 0
	indexCursor := 0
	for _, part := range work.Parts {
		batch.PartIDs = append(batch.PartIDs, part.ID)
		partPlacement := PartPlacement{
			PartID:       part.ID,
			BatchID:      work.ID,
			VertexOffset: vertexCursor,
			VertexStart:  vertexCursor,
			VertexEnd:    vertexCursor + part.VertexCount,
			IndexOffset:  indexCursor,
			IndexStart:   indexCursor,
			IndexEnd:     indexCursor + part.EffectiveIndexCount(),
			Submeshes:    make([]SubmeshPlacement, 0, len(part.Submeshes)),
		}
		submeshes := append([]Submesh(nil), part.Submeshes...)
		sort.Slice(submeshes, func(i, j int) bool {
			if submeshes[i].VertexStart != submeshes[j].VertexStart {
				return submeshes[i].VertexStart < submeshes[j].VertexStart
			}
			return submeshes[i].ID < submeshes[j].ID
		})
		for _, submesh := range submeshes {
			partPlacement.Submeshes = append(partPlacement.Submeshes, SubmeshPlacement{
				PartID:            part.ID,
				SubmeshID:         submesh.ID,
				MaterialSlot:      submesh.MaterialSlot,
				VertexOffset:      vertexCursor + submesh.VertexStart,
				VertexStart:       vertexCursor + submesh.VertexStart,
				VertexEnd:         vertexCursor + submesh.VertexStart + submesh.VertexCount,
				IndexOffset:       indexCursor + submesh.IndexStart,
				IndexStart:        indexCursor + submesh.IndexStart,
				IndexEnd:          indexCursor + submesh.IndexStart + submesh.IndexCount,
				SourceVertexStart: submesh.VertexStart,
				SourceVertexEnd:   submesh.VertexStart + submesh.VertexCount,
				SourceIndexStart:  submesh.IndexStart,
				SourceIndexEnd:    submesh.IndexStart + submesh.IndexCount,
			})
		}
		batch.Placements = append(batch.Placements, partPlacement)
		vertexCursor += part.VertexCount
		indexCursor += part.EffectiveIndexCount()
	}
	batch.Dependencies = batchDependencies(project, batch, upstreamKey)
	batch.ContentHash = BatchContentHash(batch)
	batch.CacheKey = BatchCacheKey(project, batch, upstreamKey)
	return batch
}

func batchDependencies(project *Project, batch Batch, upstreamKey string) []string {
	partIDs := batchDecisionPartIDs(batch)
	dependencies := make([]string, 0, len(partIDs)*3+12)
	byID := partMap(project)
	parts := make(map[string]struct{}, len(partIDs))
	members := stringSet(batch.PartIDs)
	for _, partID := range partIDs {
		part := byID[partID]
		parts[partID] = struct{}{}
		dependencies = append(dependencies, "part:"+partID, "mount:"+part.MountID)
		if _, member := members[partID]; member {
			dependencies = append(dependencies, "part_hash:"+PartHash(part))
		} else {
			dependencies = append(dependencies, "part_fit_hash:"+PartFitHash(part))
		}
	}
	materialSlots := append([]string(nil), batch.MaterialSlots...)
	for _, partID := range partIDs {
		materialSlots = append(materialSlots, byID[partID].MaterialSlots...)
	}
	for _, material := range materialVersions(project, materialSlots) {
		dependencies = append(dependencies, fmt.Sprintf("material:%s@%s", material.ID, material.Version))
	}
	dependencies = append(dependencies, "constraints:"+ConstraintVersion(project.Constraints))
	dependencies = append(dependencies,
		fmt.Sprintf("budget:vertices:%d/%d", batch.VertexUsed, project.Constraints.MaxVerticesPerBatch),
		fmt.Sprintf("budget:indices:%d/%d", batch.IndexUsed, project.Constraints.MaxIndicesPerBatch),
		fmt.Sprintf("budget:material_slots:%d/%d", len(batch.MaterialSlots), project.Constraints.MaxMaterialSlotsPerBatch),
	)
	for _, placement := range batch.Placements {
		for _, submesh := range placement.Submeshes {
			dependencies = append(dependencies, fmt.Sprintf(
				"offset:%s/%s@v%d:i%d",
				submesh.PartID,
				submesh.SubmeshID,
				submesh.VertexOffset,
				submesh.IndexOffset,
			))
		}
	}
	for _, rule := range project.MutualExclusions {
		_, a := parts[rule.A]
		_, b := parts[rule.B]
		if a || b {
			dependencies = append(dependencies, "exclusion:"+rule.ID)
		}
	}
	if upstreamKey != "" {
		dependencies = append(dependencies, "upstream:"+upstreamKey)
	}
	return sortedUnique(dependencies)
}
