package diagnostics

import (
	"encoding/json"
	"fmt"
	"reflect"

	"wardrobe/domain"
)

type Mismatch struct {
	Path     string `json:"path"`
	Field    string `json:"field"`
	BatchID  string `json:"batchId,omitempty"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

func (m *Mismatch) Error() string {
	if m.BatchID != "" {
		return fmt.Sprintf("first mismatch at %s (field %s, batch %s): expected %s, actual %s", m.Path, m.Field, m.BatchID, m.Expected, m.Actual)
	}
	return fmt.Sprintf("first mismatch at %s (field %s): expected %s, actual %s", m.Path, m.Field, m.Expected, m.Actual)
}

// CompareExpected checks the contract recorded by the deterministic generator.
// Runtime-only batch metadata is deliberately excluded.
func CompareExpected(expected domain.ExpectedCase, actual domain.CaseResult) *Mismatch {
	expectedView := domain.CaseResult{
		CaseID:          expected.CaseID,
		SelectedPartIDs: expected.SelectedPartIDs,
		Valid:           expected.Valid,
		AcceptedPartIDs: expected.AcceptedPartIDs,
		Conflicts:       expected.Conflicts,
		Batches:         expected.Batches,
		Decisions:       expected.Decisions,
		CommitSequence:  expected.CommitSequence,
	}
	expectedView = normalizeCase(expectedView)
	actual = normalizeCase(actual)
	if !reflect.DeepEqual(expectedView.SelectedPartIDs, actual.SelectedPartIDs) {
		return mismatch("case", "selectedPartIds", "", expectedView.SelectedPartIDs, actual.SelectedPartIDs)
	}
	if expectedView.Valid != actual.Valid {
		return mismatch("case", "valid", "", expectedView.Valid, actual.Valid)
	}
	if !reflect.DeepEqual(expectedView.AcceptedPartIDs, actual.AcceptedPartIDs) {
		return mismatch("case", "acceptedPartIds", "", expectedView.AcceptedPartIDs, actual.AcceptedPartIDs)
	}
	if !reflect.DeepEqual(expectedView.Conflicts, actual.Conflicts) {
		return mismatch("case", "conflicts", "", expectedView.Conflicts, actual.Conflicts)
	}
	if !reflect.DeepEqual(expectedView.CommitSequence, actual.CommitSequence) {
		return mismatch("case", "commitSequence", "", expectedView.CommitSequence, actual.CommitSequence)
	}
	if !reflect.DeepEqual(expectedView.Decisions, actual.Decisions) {
		return mismatch("case", "decisions", "", expectedView.Decisions, actual.Decisions)
	}
	if len(expectedView.Batches) != len(actual.Batches) {
		return mismatch("case", "batches.length", "", len(expectedView.Batches), len(actual.Batches))
	}
	for index := range expectedView.Batches {
		if m := compareBatch(expectedView.Batches[index], actual.Batches[index], true); m != nil {
			return m
		}
	}
	return nil
}

// CompareResults compares two fully materialized paths. Source and reuse flags
// are observations about execution, not deterministic plan data.
func CompareResults(expected, actual domain.CaseResult) *Mismatch {
	expected = normalizeCase(expected)
	actual = normalizeCase(actual)
	if expected.CaseID != actual.CaseID {
		return mismatch("case", "caseId", "", expected.CaseID, actual.CaseID)
	}
	if expected.InputHash != actual.InputHash {
		return mismatch("case", "inputHash", "", expected.InputHash, actual.InputHash)
	}
	if expected.ConstraintVersion != actual.ConstraintVersion {
		return mismatch("case", "constraintVersion", "", expected.ConstraintVersion, actual.ConstraintVersion)
	}
	if expected.AlgorithmVersion != actual.AlgorithmVersion {
		return mismatch("case", "algorithmVersion", "", expected.AlgorithmVersion, actual.AlgorithmVersion)
	}
	if !reflect.DeepEqual(expected.SelectedPartIDs, actual.SelectedPartIDs) {
		return mismatch("case", "selectedPartIds", "", expected.SelectedPartIDs, actual.SelectedPartIDs)
	}
	if expected.Valid != actual.Valid {
		return mismatch("case", "valid", "", expected.Valid, actual.Valid)
	}
	if !reflect.DeepEqual(expected.AcceptedPartIDs, actual.AcceptedPartIDs) {
		return mismatch("case", "acceptedPartIds", "", expected.AcceptedPartIDs, actual.AcceptedPartIDs)
	}
	if !reflect.DeepEqual(expected.Conflicts, actual.Conflicts) {
		return mismatch("case", "conflicts", "", expected.Conflicts, actual.Conflicts)
	}
	if !reflect.DeepEqual(expected.CommitSequence, actual.CommitSequence) {
		return mismatch("case", "commitSequence", "", expected.CommitSequence, actual.CommitSequence)
	}
	if !reflect.DeepEqual(expected.Decisions, actual.Decisions) {
		return mismatch("case", "decisions", "", expected.Decisions, actual.Decisions)
	}
	if len(expected.Batches) != len(actual.Batches) {
		return mismatch("case", "batches.length", "", len(expected.Batches), len(actual.Batches))
	}
	for index := range expected.Batches {
		if m := compareBatch(expected.Batches[index], actual.Batches[index], false); m != nil {
			return m
		}
	}
	return nil
}

func compareBatch(expected, actual domain.BatchResult, truthOnly bool) *Mismatch {
	path := fmt.Sprintf("batches[%d]", expected.Ordinal-1)
	batchID := expected.ID
	if expected.ID != actual.ID {
		return mismatch(path, "id", batchID, expected.ID, actual.ID)
	}
	if expected.Ordinal != actual.Ordinal {
		return mismatch(path, "ordinal", batchID, expected.Ordinal, actual.Ordinal)
	}
	if expected.CommitSequence != actual.CommitSequence {
		return mismatch(path, "commitSequence", batchID, expected.CommitSequence, actual.CommitSequence)
	}
	if !reflect.DeepEqual(expected.PartIDs, actual.PartIDs) {
		return mismatch(path, "partIds", batchID, expected.PartIDs, actual.PartIDs)
	}
	if !reflect.DeepEqual(expected.MaterialSlots, actual.MaterialSlots) {
		return mismatch(path, "materialSlots", batchID, expected.MaterialSlots, actual.MaterialSlots)
	}
	if expected.VertexUsed != actual.VertexUsed {
		return mismatch(path, "vertexUsed", batchID, expected.VertexUsed, actual.VertexUsed)
	}
	if expected.IndexUsed != actual.IndexUsed {
		return mismatch(path, "indexUsed", batchID, expected.IndexUsed, actual.IndexUsed)
	}
	if len(expected.Parts) != len(actual.Parts) {
		return mismatch(path, "parts.length", batchID, len(expected.Parts), len(actual.Parts))
	}
	for partIndex := range expected.Parts {
		expectedPart := expected.Parts[partIndex]
		actualPart := actual.Parts[partIndex]
		partPath := fmt.Sprintf("%s.parts[%d]", path, partIndex)
		if expectedPart.PartID != actualPart.PartID {
			return mismatch(partPath, "partId", batchID, expectedPart.PartID, actualPart.PartID)
		}
		if expectedPart.VertexOffset != actualPart.VertexOffset {
			return mismatch(partPath, "vertexOffset", batchID, expectedPart.VertexOffset, actualPart.VertexOffset)
		}
		if expectedPart.IndexOffset != actualPart.IndexOffset {
			return mismatch(partPath, "indexOffset", batchID, expectedPart.IndexOffset, actualPart.IndexOffset)
		}
		if len(expectedPart.Submeshes) != len(actualPart.Submeshes) {
			return mismatch(partPath, "submeshes.length", batchID, len(expectedPart.Submeshes), len(actualPart.Submeshes))
		}
		for submeshIndex := range expectedPart.Submeshes {
			expectedSubmesh := expectedPart.Submeshes[submeshIndex]
			actualSubmesh := actualPart.Submeshes[submeshIndex]
			submeshPath := fmt.Sprintf("%s.submeshes[%d]", partPath, submeshIndex)
			if !reflect.DeepEqual(expectedSubmesh, actualSubmesh) {
				if expectedSubmesh.SubmeshID != actualSubmesh.SubmeshID {
					return mismatch(submeshPath, "submeshId", batchID, expectedSubmesh.SubmeshID, actualSubmesh.SubmeshID)
				}
				if expectedSubmesh.VertexOffset != actualSubmesh.VertexOffset {
					return mismatch(submeshPath, "vertexOffset", batchID, expectedSubmesh.VertexOffset, actualSubmesh.VertexOffset)
				}
				if expectedSubmesh.IndexOffset != actualSubmesh.IndexOffset {
					return mismatch(submeshPath, "indexOffset", batchID, expectedSubmesh.IndexOffset, actualSubmesh.IndexOffset)
				}
				return mismatch(submeshPath, "submesh", batchID, expectedSubmesh, actualSubmesh)
			}
		}
	}
	if !truthOnly {
		if !reflect.DeepEqual(expected.Dependencies, actual.Dependencies) {
			return mismatch(path, "dependencies", batchID, expected.Dependencies, actual.Dependencies)
		}
		if expected.CacheKey != actual.CacheKey {
			return mismatch(path, "cacheKey", batchID, expected.CacheKey, actual.CacheKey)
		}
	}
	return nil
}

func mismatch(path, field string, batchID string, expected, actual any) *Mismatch {
	return &Mismatch{
		Path:     path,
		Field:    field,
		BatchID:  batchID,
		Expected: describe(expected),
		Actual:   describe(actual),
	}
}

func describe(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(data)
}

func normalizeCase(result domain.CaseResult) domain.CaseResult {
	out := result
	out.SelectedPartIDs = nonNil(out.SelectedPartIDs)
	out.AcceptedPartIDs = nonNil(out.AcceptedPartIDs)
	out.CommitSequence = nonNil(out.CommitSequence)
	out.ReusedBatchIDs = nonNil(out.ReusedBatchIDs)
	out.InvalidatedBatchIDs = nonNil(out.InvalidatedBatchIDs)
	out.Conflicts = append([]domain.Conflict(nil), out.Conflicts...)
	for index := range out.Conflicts {
		out.Conflicts[index].PartIDs = nonNil(out.Conflicts[index].PartIDs)
	}
	out.Decisions = append([]domain.Decision(nil), out.Decisions...)
	for index := range out.Decisions {
		out.Decisions[index].BlockedBy = nonNil(out.Decisions[index].BlockedBy)
	}
	out.Batches = append([]domain.BatchResult(nil), out.Batches...)
	for batchIndex := range out.Batches {
		batch := &out.Batches[batchIndex]
		batch.PartIDs = nonNil(batch.PartIDs)
		batch.MaterialSlots = nonNil(batch.MaterialSlots)
		batch.Dependencies = nonNil(batch.Dependencies)
		batch.Parts = append([]domain.PartPlacement(nil), batch.Parts...)
		for partIndex := range batch.Parts {
			batch.Parts[partIndex].Submeshes = append([]domain.SubmeshPlacement(nil), batch.Parts[partIndex].Submeshes...)
		}
	}
	return out
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
