package dressbatch

import (
	"sort"
)

type ProjectChangeSet struct {
	ConstraintsChanged     bool     `json:"constraints_changed"`
	AlgorithmChanged       bool     `json:"algorithm_changed"`
	AddedParts             []string `json:"added_part_ids"`
	RemovedParts           []string `json:"removed_part_ids"`
	FitAffectedParts       []string `json:"fit_affected_part_ids"`
	LayoutAffectedParts    []string `json:"layout_affected_part_ids"`
	ReorderedParts         []string `json:"reordered_part_ids"`
	ChangedMaterials       []string `json:"changed_material_ids"`
	ChangedExclusions      []string `json:"changed_exclusion_ids"`
	ExclusionAffectedParts []string `json:"exclusion_affected_part_ids"`
}

func DiffProjects(oldProject, newProject *Project) (*ProjectChangeSet, error) {
	if oldProject == nil || newProject == nil {
		return nil, newError(CodeSchema, "project", "document", "old and new projects are required")
	}
	if oldProject.ID != newProject.ID {
		return nil, newError(CodeSchema, "project", "id", "cannot diff projects with different ids")
	}
	changes := &ProjectChangeSet{}
	if ConstraintVersion(oldProject.Constraints) != ConstraintVersion(newProject.Constraints) {
		changes.ConstraintsChanged = true
	}
	oldParts := partMap(oldProject)
	newParts := partMap(newProject)
	for partID, oldPart := range oldParts {
		newPart, exists := newParts[partID]
		if !exists {
			changes.RemovedParts = append(changes.RemovedParts, partID)
			continue
		}
		if PartHash(oldPart) == PartHash(newPart) {
			continue
		}
		reordered := oldPart.Priority != newPart.Priority ||
			oldPart.MountID != newPart.MountID ||
			oldPart.Skin != newPart.Skin ||
			oldPart.SkinGroup != newPart.SkinGroup
		if reordered {
			changes.ReorderedParts = append(changes.ReorderedParts, partID)
		}
		fitChanged := reordered ||
			oldPart.VertexCount != newPart.VertexCount ||
			oldPart.EffectiveIndexCount() != newPart.EffectiveIndexCount() ||
			!equalStrings(sortedCopy(oldPart.MaterialSlots), sortedCopy(newPart.MaterialSlots))
		if fitChanged {
			changes.FitAffectedParts = append(changes.FitAffectedParts, partID)
		} else {
			changes.LayoutAffectedParts = append(changes.LayoutAffectedParts, partID)
		}
	}
	for partID := range newParts {
		if _, exists := oldParts[partID]; !exists {
			changes.AddedParts = append(changes.AddedParts, partID)
		}
	}

	oldMaterials := materialVersionMap(oldProject)
	newMaterials := materialVersionMap(newProject)
	for slot, oldVersion := range oldMaterials {
		if newVersion, exists := newMaterials[slot]; !exists || newVersion != oldVersion {
			changes.ChangedMaterials = append(changes.ChangedMaterials, slot)
		}
	}
	for slot := range newMaterials {
		if _, exists := oldMaterials[slot]; !exists {
			changes.ChangedMaterials = append(changes.ChangedMaterials, slot)
		}
	}

	oldRules := ruleMap(oldProject.MutualExclusions)
	newRules := ruleMap(newProject.MutualExclusions)
	affected := make(map[string]struct{})
	for ruleID, oldRule := range oldRules {
		newRule, exists := newRules[ruleID]
		if !exists {
			changes.ChangedExclusions = append(changes.ChangedExclusions, ruleID)
			affected[oldRule.A] = struct{}{}
			affected[oldRule.B] = struct{}{}
			continue
		}
		if oldRule.A != newRule.A || oldRule.B != newRule.B || oldRule.Reason != newRule.Reason {
			changes.ChangedExclusions = append(changes.ChangedExclusions, ruleID)
			affected[oldRule.A] = struct{}{}
			affected[oldRule.B] = struct{}{}
			affected[newRule.A] = struct{}{}
			affected[newRule.B] = struct{}{}
		}
	}
	for ruleID, newRule := range newRules {
		if _, exists := oldRules[ruleID]; !exists {
			changes.ChangedExclusions = append(changes.ChangedExclusions, ruleID)
			affected[newRule.A] = struct{}{}
			affected[newRule.B] = struct{}{}
		}
	}
	for partID := range affected {
		changes.ExclusionAffectedParts = append(changes.ExclusionAffectedParts, partID)
	}

	changes.AddedParts = sortedUnique(changes.AddedParts)
	changes.RemovedParts = sortedUnique(changes.RemovedParts)
	changes.FitAffectedParts = sortedUnique(changes.FitAffectedParts)
	changes.LayoutAffectedParts = sortedUnique(changes.LayoutAffectedParts)
	changes.ReorderedParts = sortedUnique(changes.ReorderedParts)
	changes.ChangedMaterials = sortedUnique(changes.ChangedMaterials)
	changes.ChangedExclusions = sortedUnique(changes.ChangedExclusions)
	changes.ExclusionAffectedParts = sortedUnique(changes.ExclusionAffectedParts)
	return changes, nil
}

func ruleMap(rules []MutualExclusion) map[string]MutualExclusion {
	out := make(map[string]MutualExclusion, len(rules))
	for _, rule := range rules {
		out[rule.ID] = rule
	}
	return out
}

func IncrementalRepack(request IncrementalRequest) (*IncrementalResult, error) {
	if request.NewProject == nil || request.Previous == nil {
		return nil, newError(CodeSchema, "incremental", "previous", "new project and previous result are required")
	}
	full, err := Pack(request.NewProject, request.CandidatePartIDs)
	if err != nil {
		return nil, err
	}
	changes, err := DiffProjects(request.OldProject, request.NewProject)
	if err != nil {
		return nil, err
	}
	candidatesChanged := !equalStrings(sortedUnique(request.Previous.CandidatePartIDs), full.CandidatePartIDs)
	acceptedChanged := !equalStrings(sortedUnique(request.Previous.AcceptedPartIDs), full.AcceptedPartIDs)
	report, invalid := computeInvalidation(request.NewProject, request.Previous, changes, candidatesChanged, acceptedChanged)
	firstInvalid := len(request.Previous.Batches)
	if len(invalid) > 0 {
		firstInvalid = invalid[0]
	}
	prefixLen := firstInvalid
	if report.FirstInvalid == "" {
		prefixLen = len(request.Previous.Batches)
	}
	prefix := append([]Batch(nil), request.Previous.Batches[:prefixLen]...)
	if request.ReusedPrefix != nil {
		if len(request.ReusedPrefix) < prefixLen {
			return nil, diffError(
				firstInvalidBatchID(request.Previous, firstInvalid),
				"reused_prefix.length",
				prefixLen,
				len(request.ReusedPrefix),
			)
		}
		for i := range prefix {
			if err := compareBatches(prefix[i], request.ReusedPrefix[i]); err != nil {
				return nil, err
			}
		}
		prefix = append([]Batch(nil), request.ReusedPrefix[:prefixLen]...)
	}

	incrementalResult := &PackResult{
		ProjectID:         full.ProjectID,
		CandidatePartIDs:  append([]string(nil), full.CandidatePartIDs...),
		AcceptedPartIDs:   append([]string(nil), full.AcceptedPartIDs...),
		Conflicts:         append([]Conflict(nil), full.Conflicts...),
		ConstraintVersion: full.ConstraintVersion,
		MaterialVersion:   full.MaterialVersion,
		AlgorithmVersion:  full.AlgorithmVersion,
	}
	incrementalResult.Batches = append(incrementalResult.Batches, prefix...)
	if prefixLen < len(full.Batches) {
		consumed := make(map[string]struct{})
		decisionStart := 0
		for _, batch := range prefix {
			for _, partID := range batch.PartIDs {
				consumed[partID] = struct{}{}
			}
			decisionStart += len(batch.Decisions)
		}
		works, err := composeBatchesFrom(request.NewProject, full.AcceptedPartIDs, consumed, prefixLen+1, decisionStart)
		if err != nil {
			return nil, err
		}
		upstreamKey := ""
		if len(prefix) > 0 {
			upstreamKey = prefix[len(prefix)-1].CacheKey
		}
		for _, work := range works {
			incrementalResult.Batches = append(incrementalResult.Batches, materializeBatch(request.NewProject, work, upstreamKey))
			upstreamKey = incrementalResult.Batches[len(incrementalResult.Batches)-1].CacheKey
		}
	}
	incrementalResult.CommitSequence = make([]string, 0, len(incrementalResult.Batches))
	for _, batch := range incrementalResult.Batches {
		incrementalResult.CommitSequence = append(incrementalResult.CommitSequence, batch.ID)
	}
	if err := ComparePackResults(full, incrementalResult); err != nil {
		return nil, err
	}
	return &IncrementalResult{Result: incrementalResult, Invalidation: report}, nil
}

func computeInvalidation(project *Project, previous *PackResult, changes *ProjectChangeSet, candidatesChanged, acceptedChanged bool) (InvalidationReport, []int) {
	report := InvalidationReport{
		Entries:        []InvalidationEntry{},
		ReusedBatchIDs: []string{},
	}
	if len(previous.Batches) == 0 {
		return report, nil
	}
	reasons := make([][]string, len(previous.Batches))
	affected := make([]map[string]struct{}, len(previous.Batches))
	for i := range affected {
		affected[i] = make(map[string]struct{})
	}
	mark := func(index int, reason string, partIDs ...string) {
		reasons[index] = append(reasons[index], reason)
		for _, partID := range partIDs {
			affected[index][partID] = struct{}{}
		}
	}

	all := candidatesChanged || acceptedChanged || changes.ConstraintsChanged || changes.AlgorithmChanged || len(changes.ReorderedParts) > 0
	if all {
		reason := "constraints_changed"
		switch {
		case candidatesChanged:
			reason = "candidate_set_changed"
		case acceptedChanged:
			reason = "accepted_set_changed"
		case changes.AlgorithmChanged:
			reason = "algorithm_version_changed"
		case len(changes.ReorderedParts) > 0:
			reason = "part_order_changed"
		}
		for i, batch := range previous.Batches {
			mark(i, reason, batch.PartIDs...)
		}
	} else {
		fitParts := stringSet(changes.FitAffectedParts)
		layoutParts := stringSet(changes.LayoutAffectedParts)
		exclusionParts := stringSet(changes.ExclusionAffectedParts)
		for i, batch := range previous.Batches {
			decisionParts := decisionPartSet(batch)
			for partID := range fitParts {
				if _, exists := decisionParts[partID]; exists {
					mark(i, "part_fit_changed:"+partID, partID)
				}
			}
			for partID := range layoutParts {
				if containsString(batch.PartIDs, partID) {
					mark(i, "part_layout_changed:"+partID, partID)
				}
			}
			for _, slot := range changes.ChangedMaterials {
				if containsString(decisionMaterialSlots(project, batch), slot) {
					mark(i, "material_version_changed:"+slot, batchDecisionPartIDs(batch)...)
				}
			}
			for ruleID := range stringSet(changes.ChangedExclusions) {
				for partID := range exclusionParts {
					if _, exists := decisionParts[partID]; exists {
						mark(i, "exclusion_changed:"+ruleID, partID)
					}
				}
			}
		}
	}

	first := -1
	for i := range previous.Batches {
		if len(reasons[i]) > 0 && first == -1 {
			first = i
		}
	}
	if first == -1 {
		for _, batch := range previous.Batches {
			report.ReusedBatchIDs = append(report.ReusedBatchIDs, batch.ID)
		}
		return report, nil
	}
	report.FirstInvalid = previous.Batches[first].ID
	for i := first; i < len(previous.Batches); i++ {
		if i > first && len(reasons[i]) == 0 {
			mark(i, "upstream_cache_key_changed", previous.Batches[i].PartIDs...)
		} else if i > first {
			reasons[i] = append(reasons[i], "upstream_cache_key_changed")
		}
		partIDs := make([]string, 0, len(affected[i]))
		for partID := range affected[i] {
			partIDs = append(partIDs, partID)
		}
		report.Entries = append(report.Entries, InvalidationEntry{
			BatchID:       previous.Batches[i].ID,
			Reasons:       sortedUnique(reasons[i]),
			AffectedParts: sortedUnique(partIDs),
		})
	}
	for i := 0; i < first; i++ {
		report.ReusedBatchIDs = append(report.ReusedBatchIDs, previous.Batches[i].ID)
	}
	invalid := make([]int, 0, len(previous.Batches)-first)
	for i := first; i < len(previous.Batches); i++ {
		invalid = append(invalid, i)
	}
	return report, invalid
}

func decisionPartSet(batch Batch) map[string]struct{} {
	out := make(map[string]struct{}, len(batch.Decisions))
	for _, decision := range batch.Decisions {
		out[decision.PartID] = struct{}{}
	}
	for _, partID := range batch.PartIDs {
		out[partID] = struct{}{}
	}
	return out
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func firstInvalidBatchID(previous *PackResult, index int) string {
	if previous != nil && index >= 0 && index < len(previous.Batches) {
		return previous.Batches[index].ID
	}
	return ""
}

// BuildDependencyGraph exposes the graph used for diagnostics and invalidation.
func BuildDependencyGraph(project *Project, result *PackResult) map[string][]string {
	graph := make(map[string][]string)
	for _, part := range project.Parts {
		graph["part:"+part.ID] = sortedUnique([]string{"mount:" + part.MountID, "part_hash:" + PartHash(part)})
		for _, material := range materialVersions(project, part.MaterialSlots) {
			graph["part:"+part.ID] = append(graph["part:"+part.ID], "material:"+material.ID+"@"+material.Version)
		}
		graph["part:"+part.ID] = sortedUnique(graph["part:"+part.ID])
	}
	for _, batch := range result.Batches {
		node := "batch:" + batch.ID
		graph[node] = append([]string(nil), batch.Dependencies...)
	}
	return graph
}

func sortStrings(values []string) []string {
	sort.Strings(values)
	return values
}
