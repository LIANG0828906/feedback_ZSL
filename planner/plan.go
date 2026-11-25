package planner

import (
	"fmt"
	"sort"
	"strings"

	"wardrobe/domain"
)

const CodePrefixMismatch = "E_INCREMENTAL_PREFIX"
const CodeUnplaceable = "E_BATCH_UNPLACEABLE"

type Prefix struct {
	Batches   []domain.BatchResult
	Decisions []domain.Decision
}

type Result struct {
	Batches        []domain.BatchResult
	Decisions      []domain.Decision
	CommitSequence []string
}

type UnplaceableError struct {
	PartID     string
	Vertices   int
	Indices    int
	Slots      int
	Constraint domain.BatchConstraints
}

func (e *UnplaceableError) Error() string {
	return fmt.Sprintf("%s: part %s (%d vertices, %d indices, %d material slots) cannot fit empty batch constraints (v=%d, i=%d, slots=%d)",
		CodeUnplaceable, e.PartID, e.Vertices, e.Indices, e.Slots,
		e.Constraint.MaxVertices, e.Constraint.MaxIndices, e.Constraint.MaxMaterialSlots)
}

type PrefixMismatchError struct {
	Message string
}

func (e *PrefixMismatchError) Error() string {
	return CodePrefixMismatch + ": " + e.Message
}

type workingBatch struct {
	result      domain.BatchResult
	parts       []domain.Part
	materialSet map[string]struct{}
	vertexUsed  int
	indexUsed   int
	allSkinned  bool
	hasParts    bool
}

type blockedBatch struct {
	index       int
	blockers    []string
	skinOK      bool
	materialHit bool
}

func Plan(project domain.Project, acceptedPartIDs []string) (Result, error) {
	return PlanWithPrefix(project, acceptedPartIDs, Prefix{})
}

// PlanWithPrefix replays only the suffix after an immutable, already committed
// prefix. The prefix must exactly match the beginning of the deterministic part
// order. This is the incremental core used by the engine.
func PlanWithPrefix(project domain.Project, acceptedPartIDs []string, prefix Prefix) (Result, error) {
	partMap := project.PartMap()
	orderedIDs := make([]string, 0, len(acceptedPartIDs))
	for _, id := range domain.NormalizeIDs(acceptedPartIDs) {
		part, ok := partMap[id]
		if !ok {
			return Result{}, fmt.Errorf("unknown part %s", id)
		}
		orderedIDs = append(orderedIDs, part.ID)
	}
	sort.Slice(orderedIDs, func(i, j int) bool {
		return domain.PartOrderLess(partMap[orderedIDs[i]], partMap[orderedIDs[j]])
	})
	orderedParts := make([]domain.Part, 0, len(orderedIDs))
	for _, id := range orderedIDs {
		orderedParts = append(orderedParts, partMap[id])
	}

	batches := make([]workingBatch, 0, len(orderedParts))
	startIndex := 0
	prefixPartCount := 0
	for _, batch := range prefix.Batches {
		prefixPartCount += len(batch.PartIDs)
	}
	if len(prefix.Batches) > 0 {
		if prefixPartCount > len(orderedParts) {
			return Result{}, &PrefixMismatchError{Message: "prefix contains more parts than the new selection"}
		}
		flat := make([]string, 0, prefixPartCount)
		for _, batch := range prefix.Batches {
			flat = append(flat, batch.PartIDs...)
		}
		for index, id := range flat {
			if orderedIDs[index] != id {
				return Result{}, &PrefixMismatchError{Message: fmt.Sprintf("part order differs at index %d: prefix=%s ordered=%s", index, id, orderedIDs[index])}
			}
		}
		startIndex = prefixPartCount
		for _, batchResult := range prefix.Batches {
			parts := make([]domain.Part, 0, len(batchResult.PartIDs))
			materialSet := make(map[string]struct{})
			vertexUsed := 0
			indexUsed := 0
			for _, partID := range batchResult.PartIDs {
				part := partMap[partID]
				parts = append(parts, part)
				for _, slot := range materialSlots(part) {
					materialSet[slot] = struct{}{}
				}
				vertexUsed += part.Mesh.VertexCount
				indexUsed += part.Mesh.TriangleCount * 3
			}
			batches = append(batches, workingBatch{
				result:      cloneBatch(batchResult),
				parts:       parts,
				materialSet: materialSet,
				vertexUsed:  vertexUsed,
				indexUsed:   indexUsed,
				allSkinned:  allPartsSkinned(parts),
				hasParts:    len(parts) > 0,
			})
		}
	}
	decisions := make([]domain.Decision, 0, len(orderedParts))
	if len(prefix.Batches) > 0 || len(prefix.Decisions) > 0 {
		expectedPrefixDecisions := 0
		for _, batch := range prefix.Batches {
			expectedPrefixDecisions += len(batch.PartIDs)
		}
		if len(prefix.Decisions) != expectedPrefixDecisions {
			return Result{}, &PrefixMismatchError{Message: fmt.Sprintf("prefix decision count=%d, expected %d", len(prefix.Decisions), expectedPrefixDecisions)}
		}
		decisions = append(decisions, cloneDecisions(prefix.Decisions)...)
	}

	for partIndex := startIndex; partIndex < len(orderedParts); partIndex++ {
		part := orderedParts[partIndex]
		partSlots := materialSlots(part)
		if part.Mesh.VertexCount > project.Constraints.MaxVertices ||
			part.Mesh.TriangleCount*3 > project.Constraints.MaxIndices ||
			len(partSlots) > project.Constraints.MaxMaterialSlots {
			return Result{}, &UnplaceableError{
				PartID:     part.ID,
				Vertices:   part.Mesh.VertexCount,
				Indices:    part.Mesh.TriangleCount * 3,
				Slots:      len(partSlots),
				Constraint: project.Constraints,
			}
		}
		decision := placePart(project, part, &batches)
		decision.Sequence = len(decisions) + 1
		decisions = append(decisions, decision)
	}

	for index := range batches {
		batches[index].result.ID = batchID(index + 1)
		batches[index].result.Ordinal = index + 1
		batches[index].result.CommitSequence = index + 1
		batches[index].result.Dependencies = dependenciesFor(project, batches[index], index)
		upstreamKey := ""
		if index > 0 {
			upstreamKey = batches[index-1].result.CacheKey
		}
		batches[index].result.CacheKey = cacheKeyFor(project, batches[index], upstreamKey)
	}

	result := Result{
		Batches:        make([]domain.BatchResult, 0, len(batches)),
		Decisions:      decisions,
		CommitSequence: make([]string, 0, len(batches)),
	}
	for _, batch := range batches {
		result.Batches = append(result.Batches, cloneBatch(batch.result))
		result.CommitSequence = append(result.CommitSequence, batch.result.ID)
	}
	return result, nil
}

func placePart(project domain.Project, part domain.Part, batches *[]workingBatch) domain.Decision {
	partSlots := materialSlots(part)
	blockedBy := make([]string, 0)
	blocked := make([]blockedBatch, 0, len(*batches))
	for index := range *batches {
		batch := &(*batches)[index]
		blockers := fitBlockers(project, *batch, part, partSlots)
		if len(blockers) == 0 {
			result := domain.Decision{
				PartID:                 part.ID,
				BatchID:                batch.result.ID,
				BatchOrdinal:           batch.result.Ordinal,
				ConsideredBatchID:      batch.result.ID,
				ConsideredBatchOrdinal: batch.result.Ordinal,
				Result:                 domain.DecisionReuse,
				Reason:                 domain.ReasonBudgetFit,
			}
			if sharesMaterial(*batch, partSlots) {
				result.Reason = domain.ReasonMaterialAffinity
				result.Detail = fmt.Sprintf("part %s joined %s because it shares material slot(s) %s and fits all budgets",
					part.ID, batch.result.ID, strings.Join(partSlots, ","))
			} else {
				result.Detail = fmt.Sprintf("part %s joined %s because all budgets and skin policy fit", part.ID, batch.result.ID)
			}
			appendPartToBatch(batch, part)
			return result
		}
		skinOK := project.Constraints.AllowCrossSkin || !batch.hasParts || batch.allSkinned == part.Skinned
		blocked = append(blocked, blockedBatch{
			index:       index,
			blockers:    blockers,
			skinOK:      skinOK,
			materialHit: sharesMaterial(*batch, partSlots),
		})
		for _, blocker := range blockers {
			blockedBy = append(blockedBy, batch.result.ID+":"+blocker)
		}
	}

	newOrdinal := len(*batches) + 1
	newBatch := newWorkingBatch(newOrdinal)
	chosen := chooseExplanationBatch(blocked)
	primaryReason := domain.ReasonInitialBatch
	consideredID := ""
	consideredOrdinal := 0
	if chosen != nil {
		consideredID = (*batches)[chosen.index].result.ID
		consideredOrdinal = (*batches)[chosen.index].result.Ordinal
		primaryReason = chosen.blockers[0]
	}
	if len(*batches) == 0 {
		primaryReason = domain.ReasonInitialBatch
	}
	appendPartToBatch(&newBatch, part)
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

func chooseExplanationBatch(blocked []blockedBatch) *blockedBatch {
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

func fitBlockers(project domain.Project, batch workingBatch, part domain.Part, partSlots []string) []string {
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

func newWorkingBatch(ordinal int) workingBatch {
	return workingBatch{
		result: domain.BatchResult{
			ID:             batchID(ordinal),
			Ordinal:        ordinal,
			CommitSequence: ordinal,
			PartIDs:        []string{},
			Parts:          []domain.PartPlacement{},
			MaterialSlots:  []string{},
		},
		materialSet: make(map[string]struct{}),
	}
}

func appendPartToBatch(batch *workingBatch, part domain.Part) {
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

func sharesMaterial(batch workingBatch, slots []string) bool {
	for _, slot := range slots {
		if _, ok := batch.materialSet[slot]; ok {
			return true
		}
	}
	return false
}

func materialSlots(part domain.Part) []string {
	slots := make([]string, 0, len(part.Mesh.Submeshes))
	for _, submesh := range part.Mesh.Submeshes {
		slots = append(slots, submesh.MaterialSlot)
	}
	return domain.SortedUnique(slots)
}

func allPartsSkinned(parts []domain.Part) bool {
	if len(parts) == 0 {
		return false
	}
	skinned := parts[0].Skinned
	for _, part := range parts[1:] {
		if part.Skinned != skinned {
			return false
		}
	}
	return skinned
}

func dependenciesFor(project domain.Project, batch workingBatch, batchIndex int) []string {
	deps := make([]string, 0)
	mountMap := project.MountMap()
	for _, part := range batch.parts {
		deps = append(deps, "part:"+part.ID+"@"+shortHash(domain.PartContentHash(part)))
		deps = append(deps, "mount:"+part.MountID)
		if _, ok := mountMap[part.MountID]; !ok {
			deps = append(deps, "mount:missing:"+part.MountID)
		}
	}
	for _, slot := range batch.result.MaterialSlots {
		version := project.MaterialMap()[slot].Version
		deps = append(deps, fmt.Sprintf("material:%s@%d", slot, version))
	}
	for _, placement := range batch.result.Parts {
		deps = append(deps,
			fmt.Sprintf("offset:part:%s:vertex@%d", placement.PartID, placement.VertexOffset),
			fmt.Sprintf("offset:part:%s:index@%d", placement.PartID, placement.IndexOffset),
		)
	}
	deps = append(deps,
		fmt.Sprintf("budget:vertices@max=%d,used=%d,remaining=%d",
			project.Constraints.MaxVertices, batch.vertexUsed, project.Constraints.MaxVertices-batch.vertexUsed),
		fmt.Sprintf("budget:indices@max=%d,used=%d,remaining=%d",
			project.Constraints.MaxIndices, batch.indexUsed, project.Constraints.MaxIndices-batch.indexUsed),
		fmt.Sprintf("budget:materialSlots@max=%d,used=%d,remaining=%d",
			project.Constraints.MaxMaterialSlots, len(batch.result.MaterialSlots),
			project.Constraints.MaxMaterialSlots-len(batch.result.MaterialSlots)),
		"policy:crossSkin",
	)
	if batchIndex > 0 {
		deps = append(deps, fmt.Sprintf("upstream:batch:%s", batchID(batchIndex)))
	}
	sort.Strings(deps)
	return deps
}

func cacheKeyFor(project domain.Project, batch workingBatch, upstreamKey string) string {
	partHashes := make([]string, 0, len(batch.parts))
	for _, part := range batch.parts {
		partHashes = append(partHashes, domain.PartContentHash(part))
	}
	materialVersions := make(map[string]int, len(batch.result.MaterialSlots))
	materialMap := project.MaterialMap()
	for _, slot := range batch.result.MaterialSlots {
		materialVersions[slot] = materialMap[slot].Version
	}
	return domain.BatchCacheKey(partHashes, materialVersions, project.Constraints, upstreamKey)
}

func batchID(ordinal int) string {
	return fmt.Sprintf("B%03d", ordinal)
}

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func cloneBatch(batch domain.BatchResult) domain.BatchResult {
	out := batch
	out.PartIDs = append([]string(nil), batch.PartIDs...)
	out.MaterialSlots = append([]string(nil), batch.MaterialSlots...)
	out.Dependencies = append([]string(nil), batch.Dependencies...)
	out.Parts = make([]domain.PartPlacement, len(batch.Parts))
	for index, placement := range batch.Parts {
		out.Parts[index] = placement
		out.Parts[index].Submeshes = append([]domain.SubmeshPlacement(nil), placement.Submeshes...)
	}
	return out
}

func cloneDecisions(decisions []domain.Decision) []domain.Decision {
	out := make([]domain.Decision, len(decisions))
	for index, decision := range decisions {
		out[index] = decision
		if decision.BlockedBy != nil {
			out[index].BlockedBy = make([]string, len(decision.BlockedBy))
			copy(out[index].BlockedBy, decision.BlockedBy)
		}
	}
	return out
}

// EarliestBatchThatCouldFit returns the first existing batch ordinal that would
// accept the edited part by slot and budget checks. It is used to find the true
// invalidation frontier when a part shrinks or changes material slot.
func EarliestBatchThatCouldFit(project domain.Project, batches []domain.BatchResult, part domain.Part) int {
	partMap := project.PartMap()
	for index, batch := range batches {
		working := newWorkingBatch(index + 1)
		for _, partID := range batch.PartIDs {
			appendPartToBatch(&working, partMap[partID])
		}
		if len(fitBlockers(project, working, part, materialSlots(part))) == 0 {
			return index + 1
		}
	}
	return 0
}
