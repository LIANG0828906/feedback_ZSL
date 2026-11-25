package dressbatch

import (
	"fmt"
	"reflect"
)

func VerifyTruth(project *Project) (*TruthReport, error) {
	report := &TruthReport{
		ProjectID: project.ID,
		Passed:    true,
		Cases:     make([]TruthCaseReport, 0, len(project.Truth.Assemblies)),
	}
	for _, expected := range project.Truth.Assemblies {
		assembly, err := EvaluateAssembly(project, expected.CandidatePartIDs)
		if err != nil {
			return nil, err
		}
		caseReport := TruthCaseReport{
			CaseID:         expected.ID,
			Pose:           expected.Pose,
			Legal:          assembly.Legal,
			ExpectedLegal:  expected.ExpectedLegal,
			ConflictsMatch: conflictsEqual(assembly.Conflicts, expected.ExpectedConflicts),
		}
		if !equalStrings(assembly.AcceptedPartIDs, sortedCopy(expected.ExpectedAcceptedPartIDs)) {
			return nil, truthError(project, expected.ID, "expected_accepted_part_ids", assembly.AcceptedPartIDs, expected.ExpectedAcceptedPartIDs)
		}
		if caseReport.Legal != expected.ExpectedLegal {
			return nil, truthError(project, expected.ID, "expected_legal", caseReport.Legal, expected.ExpectedLegal)
		}
		if !caseReport.ConflictsMatch {
			return nil, truthError(project, expected.ID, "expected_conflicts", assembly.Conflicts, expected.ExpectedConflicts)
		}
		if expected.ExpectedLegal {
			result, err := Pack(project, expected.CandidatePartIDs)
			if err != nil {
				return nil, err
			}
			if !equalStrings(result.CommitSequence, expected.ExpectedCommitSequence) {
				return nil, truthError(project, expected.ID, "expected_commit_sequence", result.CommitSequence, expected.ExpectedCommitSequence)
			}
			if len(result.Batches) != len(expected.ExpectedBatches) {
				return nil, truthError(project, expected.ID, "expected_batches.length", len(result.Batches), len(expected.ExpectedBatches))
			}
			batchesMatch := true
			placementsMatch := true
			for i := range result.Batches {
				if err := compareBatchToTruth(result.Batches[i], expected.ExpectedBatches[i]); err != nil {
					if domainErr, ok := AsDomainError(err); ok {
						if domainErr.Field == "placements" {
							placementsMatch = false
						}
						batchesMatch = false
						domainErr.ObjectID = fmt.Sprintf("truth:%s.%s", expected.ID, domainErr.ObjectID)
					}
					return nil, err
				}
			}
			caseReport.BatchesMatch = batchesMatch
			caseReport.PlacementsMatch = placementsMatch
		} else {
			caseReport.BatchesMatch = len(expected.ExpectedBatches) == 0
			caseReport.PlacementsMatch = caseReport.BatchesMatch
		}
		if !caseReport.BatchesMatch || !caseReport.PlacementsMatch {
			report.Passed = false
		}
		report.Cases = append(report.Cases, caseReport)
	}
	return report, nil
}

func truthError(project *Project, caseID, field string, actual, expected any) error {
	return withDetails(
		newError(CodeTruth, "truth:"+caseID, field, "sample truth mismatch"),
		map[string]any{
			"project_id": project.ID,
			"case_id":    caseID,
			"field":      field,
			"expected":   expected,
			"actual":     actual,
		},
	)
}

func compareBatchToTruth(batch Batch, truth BatchTruth) error {
	objectID := "batch:" + truth.BatchID
	if batch.ID != truth.BatchID {
		return truthError(&Project{ID: batch.ID}, truth.BatchID, "batch_id", batch.ID, truth.BatchID)
	}
	if !equalStrings(batch.PartIDs, truth.PartIDs) {
		return truthError(&Project{ID: batch.ID}, truth.BatchID, "batch.part_ids", batch.PartIDs, truth.PartIDs)
	}
	if !equalStrings(batch.MaterialSlots, truth.MaterialSlots) {
		return truthError(&Project{ID: batch.ID}, truth.BatchID, "batch.material_slots", batch.MaterialSlots, truth.MaterialSlots)
	}
	if batch.Skin != truth.Skin || batch.MixedSkin != truth.MixedSkin || !equalStrings(batch.SkinGroups, truth.SkinGroups) {
		return truthError(&Project{ID: batch.ID}, truth.BatchID, "batch.skin", []any{batch.Skin, batch.MixedSkin, batch.SkinGroups}, []any{truth.Skin, truth.MixedSkin, truth.SkinGroups})
	}
	if batch.VertexUsed != truth.VertexUsed {
		return truthError(&Project{ID: batch.ID}, truth.BatchID, "batch.vertex_used", batch.VertexUsed, truth.VertexUsed)
	}
	if batch.IndexUsed != truth.IndexUsed {
		return truthError(&Project{ID: batch.ID}, truth.BatchID, "batch.index_used", batch.IndexUsed, truth.IndexUsed)
	}
	if len(batch.Placements) != len(truth.Placements) {
		return truthError(&Project{ID: batch.ID}, truth.BatchID, "batch.placements.length", len(batch.Placements), len(truth.Placements))
	}
	for i := range batch.Placements {
		if !reflect.DeepEqual(batch.Placements[i], truth.Placements[i]) {
			return withDetails(
				newError(CodeTruth, objectID, "placements", "placement at index %d differs", i),
				map[string]any{
					"index":    i,
					"expected": truth.Placements[i],
					"actual":   batch.Placements[i],
				},
			)
		}
	}
	return nil
}

func conflictsEqual(actual, expected []Conflict) bool {
	if len(actual) != len(expected) {
		return false
	}
	a := append([]Conflict(nil), actual...)
	e := append([]Conflict(nil), expected...)
	sortConflicts(a)
	sortConflicts(e)
	return reflect.DeepEqual(a, e)
}

// ComparePackResults returns E_DIFF with the first mismatching batch and field.
func ComparePackResults(expected, actual *PackResult) error {
	if expected == nil || actual == nil {
		return diffError("", "result", expected, actual)
	}
	if expected.ProjectID != actual.ProjectID {
		return diffError("", "project_id", expected.ProjectID, actual.ProjectID)
	}
	if !equalStrings(expected.CandidatePartIDs, actual.CandidatePartIDs) {
		return diffError("", "candidate_part_ids", expected.CandidatePartIDs, actual.CandidatePartIDs)
	}
	if !equalStrings(expected.AcceptedPartIDs, actual.AcceptedPartIDs) {
		return diffError("", "accepted_part_ids", expected.AcceptedPartIDs, actual.AcceptedPartIDs)
	}
	if !conflictsEqual(expected.Conflicts, actual.Conflicts) {
		return diffError("", "conflicts", expected.Conflicts, actual.Conflicts)
	}
	if !equalStrings(expected.CommitSequence, actual.CommitSequence) {
		return diffError("", "commit_sequence", expected.CommitSequence, actual.CommitSequence)
	}
	if expected.ConstraintVersion != actual.ConstraintVersion {
		return diffError("", "constraint_version", expected.ConstraintVersion, actual.ConstraintVersion)
	}
	if expected.MaterialVersion != actual.MaterialVersion {
		return diffError("", "material_version", expected.MaterialVersion, actual.MaterialVersion)
	}
	if expected.AlgorithmVersion != actual.AlgorithmVersion {
		return diffError("", "algorithm_version", expected.AlgorithmVersion, actual.AlgorithmVersion)
	}
	max := len(expected.Batches)
	if len(actual.Batches) > max {
		max = len(actual.Batches)
	}
	for i := 0; i < max; i++ {
		if i >= len(expected.Batches) {
			return diffError(actual.Batches[i].ID, "batch.count", len(expected.Batches), len(actual.Batches))
		}
		if i >= len(actual.Batches) {
			return diffError(expected.Batches[i].ID, "batch.count", len(expected.Batches), len(actual.Batches))
		}
		if err := compareBatches(expected.Batches[i], actual.Batches[i]); err != nil {
			return err
		}
	}
	return nil
}

func compareBatches(expected, actual Batch) error {
	batchID := expected.ID
	if expected.ID != actual.ID {
		return diffError(batchID, "batch_id", expected.ID, actual.ID)
	}
	if expected.Index != actual.Index {
		return diffError(batchID, "batch_index", expected.Index, actual.Index)
	}
	if !equalStrings(expected.PartIDs, actual.PartIDs) {
		return diffError(batchID, "part_ids", expected.PartIDs, actual.PartIDs)
	}
	if !equalStrings(expected.MaterialSlots, actual.MaterialSlots) {
		return diffError(batchID, "material_slots", expected.MaterialSlots, actual.MaterialSlots)
	}
	if expected.Skin != actual.Skin {
		return diffError(batchID, "skin", expected.Skin, actual.Skin)
	}
	if expected.MixedSkin != actual.MixedSkin {
		return diffError(batchID, "mixed_skin", expected.MixedSkin, actual.MixedSkin)
	}
	if !equalStrings(expected.SkinGroups, actual.SkinGroups) {
		return diffError(batchID, "skin_groups", expected.SkinGroups, actual.SkinGroups)
	}
	if expected.VertexUsed != actual.VertexUsed {
		return diffError(batchID, "vertex_used", expected.VertexUsed, actual.VertexUsed)
	}
	if expected.IndexUsed != actual.IndexUsed {
		return diffError(batchID, "index_used", expected.IndexUsed, actual.IndexUsed)
	}
	if !reflect.DeepEqual(expected.Decisions, actual.Decisions) {
		return diffError(batchID, "decisions", expected.Decisions, actual.Decisions)
	}
	if !equalStrings(expected.Dependencies, actual.Dependencies) {
		return diffError(batchID, "dependencies", expected.Dependencies, actual.Dependencies)
	}
	if expected.ContentHash != actual.ContentHash {
		return diffError(batchID, "content_hash", expected.ContentHash, actual.ContentHash)
	}
	if expected.CacheKey != actual.CacheKey {
		return diffError(batchID, "cache_key", expected.CacheKey, actual.CacheKey)
	}
	if len(expected.Placements) != len(actual.Placements) {
		return diffError(batchID, "placements.length", len(expected.Placements), len(actual.Placements))
	}
	for i := range expected.Placements {
		if !reflect.DeepEqual(expected.Placements[i], actual.Placements[i]) {
			return diffError(batchID, fmt.Sprintf("placements[%d]", i), expected.Placements[i], actual.Placements[i])
		}
	}
	return nil
}

func diffError(batchID, field string, expected, actual any) *DomainError {
	return withDetails(
		newError(CodeDiff, batchID, field, "incremental and full repack differ at %s", field),
		map[string]any{
			"batch_id": batchID,
			"field":    field,
			"expected": expected,
			"actual":   actual,
		},
	)
}
