package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"wardrobe/assembly"
	"wardrobe/domain"
	"wardrobe/planner"
	"wardrobe/scheduler"
	"wardrobe/schema"
)

type EditRequest struct {
	ProjectID    string `json:"projectId"`
	CaseID       string `json:"caseId"`
	PartID       string `json:"partId"`
	MaterialSlot string `json:"materialSlot,omitempty"`
	ScalePercent int    `json:"scalePercent,omitempty"`
	Skinned      *bool  `json:"skinned,omitempty"`
	MountID      string `json:"mountId,omitempty"`
}

type EditReport struct {
	ProjectID           string            `json:"projectId"`
	CaseID              string            `json:"caseId"`
	PartID              string            `json:"partId"`
	ProjectRevision     int               `json:"projectRevision"`
	Applied             []string          `json:"applied"`
	Structural          bool              `json:"structural"`
	ImmediateConflicts  []domain.Conflict `json:"immediateConflicts"`
	InvalidatedBatchIDs []string          `json:"invalidatedBatchIds"`
	ReusedBatchIDs      []string          `json:"reusedBatchIds"`
	Result              domain.CaseResult `json:"result"`
}

func (e *Engine) EditPart(request EditRequest) (EditReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	baseProject, ok := e.projects[request.ProjectID]
	if !ok {
		return EditReport{}, ErrProjectNotFound
	}
	expected, ok := e.cases[caseKey(request.ProjectID, request.CaseID)]
	if !ok {
		return EditReport{}, ErrCaseNotFound
	}
	newProject, newPart, structural, applied, err := applyPartPatch(baseProject, request)
	if err != nil {
		return EditReport{}, err
	}
	newAssembly, err := assembly.Evaluate(newProject, expected.SelectedPartIDs)
	if err != nil {
		return EditReport{}, err
	}
	oldCache, hasCache := e.cache[caseKey(request.ProjectID, request.CaseID)]
	oldInputHash := domain.CaseInputHash(baseProject, expected.SelectedPartIDs)
	newInputHash := domain.CaseInputHash(newProject, newAssembly.SelectedPartIDs)
	selectedPart := contains(expected.SelectedPartIDs, request.PartID)

	report := EditReport{
		ProjectID:          request.ProjectID,
		CaseID:             request.CaseID,
		PartID:             request.PartID,
		ProjectRevision:    newProject.Revision,
		Applied:            applied,
		Structural:         structural,
		ImmediateConflicts: newAssembly.Conflicts,
	}

	if hasCache && oldCache.inputHash == oldInputHash && !structural && oldInputHash == newInputHash {
		result := cloneCaseResult(oldCache.result)
		result.Source = "memory_cache_reuse"
		result.Conflicts = newAssembly.Conflicts
		report.ReusedBatchIDs = batchIDs(result.Batches)
		report.Result = result
		e.projects[request.ProjectID] = newProject
		return report, nil
	}

	canIncremental := hasCache && oldCache.inputHash == oldInputHash && !structural && selectedPart &&
		contains(newAssembly.AcceptedPartIDs, request.PartID) &&
		reflect.DeepEqual(oldCache.result.AcceptedPartIDs, newAssembly.AcceptedPartIDs) &&
		reflect.DeepEqual(oldCache.result.Conflicts, newAssembly.Conflicts)
	if !canIncremental {
		return e.fullEditFallback(baseProject, newProject, expected, oldCache, hasCache, report, "incremental precondition not met")
	}

	directOrdinal := 0
	for _, batch := range oldCache.result.Batches {
		if contains(batch.PartIDs, request.PartID) {
			directOrdinal = batch.Ordinal
			break
		}
	}
	if directOrdinal == 0 {
		return e.fullEditFallback(baseProject, newProject, expected, oldCache, hasCache, report, "edited part has no cached batch")
	}
	frontier := directOrdinal
	earlierPrefix := oldCache.result.Batches[:directOrdinal-1]
	if earliest := planner.EarliestBatchThatCouldFit(newProject, earlierPrefix, newPart); earliest > 0 {
		frontier = earliest
	}
	if request.MaterialSlot != "" {
		changedSlot := request.MaterialSlot
		for index, batch := range oldCache.result.Batches[:frontier-1] {
			if contains(batch.MaterialSlots, changedSlot) {
				frontier = index + 1
				break
			}
		}
	}
	prefixBatchCount := frontier - 1
	prefixPartCount := 0
	for index := 0; index < prefixBatchCount; index++ {
		prefixPartCount += len(oldCache.result.Batches[index].PartIDs)
	}
	prefix := planner.Prefix{}
	if prefixBatchCount > 0 {
		prefix.Batches = copyBatches(oldCache.result.Batches[:prefixBatchCount])
		prefix.Decisions = append([]domain.Decision(nil), oldCache.result.Decisions[:prefixPartCount]...)
	}
	plan, err := planner.PlanWithPrefix(newProject, newAssembly.AcceptedPartIDs, prefix)
	if err != nil {
		return EditReport{}, err
	}
	if prefixBatchCount > 0 {
		for index := 0; index < prefixBatchCount; index++ {
			if !batchCoreEqual(prefix.Batches[index], plan.Batches[index]) {
				return e.fullEditFallback(baseProject, newProject, expected, oldCache, hasCache, report,
					fmt.Sprintf("reused prefix batch %s changed", prefix.Batches[index].ID))
			}
		}
		if !reflect.DeepEqual(prefix.Decisions, plan.Decisions[:prefixPartCount]) {
			return e.fullEditFallback(baseProject, newProject, expected, oldCache, hasCache, report, "reused prefix decisions changed")
		}
	}

	suffix := plan.Batches[prefixBatchCount:]
	validatedSuffix, scheduleReport, err := e.validateBatches(newProject, suffix)
	if err != nil {
		return EditReport{}, err
	}
	batches := make([]domain.BatchResult, 0, len(plan.Batches))
	for index, batch := range plan.Batches {
		if index < prefixBatchCount {
			batch = cloneBatch(prefix.Batches[index])
			batch.Source = "reused"
		} else {
			batch = validatedSuffix[index-prefixBatchCount]
			batch.Source = "incremental"
		}
		batches = append(batches, batch)
	}
	result := domain.CaseResult{
		CaseID:            expected.CaseID,
		Description:       expected.Description,
		SelectedPartIDs:   newAssembly.SelectedPartIDs,
		Valid:             newAssembly.Valid,
		AcceptedPartIDs:   newAssembly.AcceptedPartIDs,
		Conflicts:         newAssembly.Conflicts,
		Batches:           batches,
		Decisions:         plan.Decisions,
		CommitSequence:    append([]string(nil), plan.CommitSequence...),
		InputHash:         newInputHash,
		ConstraintVersion: newProject.Constraints.Version,
		AlgorithmVersion:  domain.AlgorithmVersion,
		Source:            "incremental",
		ReusedBatchIDs:    batchIDs(batches[:prefixBatchCount]),
		Schedule:          scheduleReport,
	}
	for index := frontier - 1; index < len(oldCache.result.Batches); index++ {
		result.InvalidatedBatchIDs = append(result.InvalidatedBatchIDs, oldCache.result.Batches[index].ID)
	}
	if e.store != nil {
		if err := e.store.SaveCase(context.Background(), newProject, result); err != nil {
			return EditReport{}, err
		}
	}
	e.projects[request.ProjectID] = newProject
	e.cache[caseKey(request.ProjectID, request.CaseID)] = cachedCase{inputHash: newInputHash, result: result}
	report.InvalidatedBatchIDs = append([]string(nil), result.InvalidatedBatchIDs...)
	report.ReusedBatchIDs = append([]string(nil), result.ReusedBatchIDs...)
	report.Result = result
	return report, nil
}

func (e *Engine) fullEditFallback(
	baseProject domain.Project,
	newProject domain.Project,
	expected domain.ExpectedCase,
	oldCache cachedCase,
	hasCache bool,
	report EditReport,
	reason string,
) (EditReport, error) {
	result, err := e.computeCase(newProject, expected, "full_after_edit", true)
	if err != nil {
		return EditReport{}, err
	}
	if hasCache {
		report.InvalidatedBatchIDs = batchIDs(oldCache.result.Batches)
	}
	report.ReusedBatchIDs = []string{}
	report.Applied = append(report.Applied, "fallback="+reason)
	report.Result = result
	e.projects[newProject.ProjectID] = newProject
	e.cache[caseKey(newProject.ProjectID, expected.CaseID)] = cachedCase{inputHash: result.InputHash, result: result}
	return report, nil
}

func (e *Engine) RetryBatch(projectID, caseID, batchID string) (domain.BatchResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	project, ok := e.projects[projectID]
	if !ok {
		return domain.BatchResult{}, ErrProjectNotFound
	}
	cached, ok := e.cache[caseKey(projectID, caseID)]
	if !ok {
		return domain.BatchResult{}, errors.New("case has no cached result; build it before retrying a batch")
	}
	batchIndex := -1
	for index, batch := range cached.result.Batches {
		if batch.ID == batchID {
			batchIndex = index
			break
		}
	}
	if batchIndex < 0 {
		return domain.BatchResult{}, fmt.Errorf("batch %s not found", batchID)
	}
	task := scheduler.Task[domain.BatchResult]{
		ID:     cached.result.Batches[batchIndex].ID,
		Weight: max(1, len(cached.result.Batches[batchIndex].PartIDs)),
		Run: func(ctx context.Context) (domain.BatchResult, error) {
			return layoutAndValidate(project, cached.result.Batches[batchIndex])
		},
	}
	results, _, err := scheduler.Run(context.Background(), e.schedulerConfig, []scheduler.Task[domain.BatchResult]{task})
	if err != nil {
		return domain.BatchResult{}, err
	}
	if len(results) != 1 || results[0].Err != nil {
		if len(results) == 1 {
			return domain.BatchResult{}, &BatchFailureError{BatchID: batchID, Err: results[0].Err}
		}
		return domain.BatchResult{}, errors.New("retry produced no result")
	}
	updated := cloneCaseResult(cached.result)
	updated.Batches[batchIndex] = results[0].Value
	updated.Batches[batchIndex].Source = "retried"
	updated.Source = "batch_retry"
	if e.store != nil {
		if err := e.store.SaveCase(context.Background(), project, updated); err != nil {
			return domain.BatchResult{}, err
		}
	}
	e.cache[caseKey(projectID, caseID)] = cachedCase{inputHash: updated.InputHash, result: updated}
	return cloneBatch(results[0].Value), nil
}

func applyPartPatch(project domain.Project, request EditRequest) (domain.Project, domain.Part, bool, []string, error) {
	if request.PartID == "" {
		return domain.Project{}, domain.Part{}, false, nil, errors.New("partId is required")
	}
	if request.MaterialSlot == "" && request.ScalePercent == 0 && request.Skinned == nil && request.MountID == "" {
		return domain.Project{}, domain.Part{}, false, nil, errors.New("edit request has no changes")
	}
	out := cloneProject(project)
	partIndex := -1
	for index := range out.Parts {
		if out.Parts[index].ID == request.PartID {
			partIndex = index
			break
		}
	}
	if partIndex < 0 {
		return domain.Project{}, domain.Part{}, false, nil, ErrPartNotFound
	}
	applied := make([]string, 0)
	structural := false
	if request.MaterialSlot != "" {
		materialIndex := -1
		for index := range out.Materials {
			if out.Materials[index].ID == request.MaterialSlot {
				materialIndex = index
				break
			}
		}
		if materialIndex < 0 {
			return domain.Project{}, domain.Part{}, false, nil, fmt.Errorf("material slot %s does not exist", request.MaterialSlot)
		}
		for index := range out.Parts[partIndex].Mesh.Submeshes {
			out.Parts[partIndex].Mesh.Submeshes[index].MaterialSlot = request.MaterialSlot
		}
		out.Materials[materialIndex].Version++
		applied = append(applied, "materialSlot="+request.MaterialSlot+"@v"+fmt.Sprint(out.Materials[materialIndex].Version))
	}
	if request.ScalePercent != 0 {
		if request.ScalePercent < 10 || request.ScalePercent > 500 {
			return domain.Project{}, domain.Part{}, false, nil, errors.New("scalePercent must be between 10 and 500")
		}
		scaleMesh(&out.Parts[partIndex].Mesh, request.ScalePercent)
		applied = append(applied, fmt.Sprintf("geometryScale=%d%%", request.ScalePercent))
	}
	if request.MountID != "" && request.MountID != out.Parts[partIndex].MountID {
		out.Parts[partIndex].MountID = request.MountID
		structural = true
		applied = append(applied, "mountId="+request.MountID)
	}
	if request.Skinned != nil && *request.Skinned != out.Parts[partIndex].Skinned {
		out.Parts[partIndex].Skinned = *request.Skinned
		structural = true
		applied = append(applied, fmt.Sprintf("skinned=%t", *request.Skinned))
	}
	out.Revision++
	if err := schema.ValidateProjectStructure(&out); err != nil {
		return domain.Project{}, domain.Part{}, false, nil, err
	}
	return out, out.Parts[partIndex], structural, applied, nil
}

func scaleMesh(mesh *domain.Mesh, percent int) {
	vertexStart := 0
	indexStart := 0
	triangles := 0
	for index := range mesh.Submeshes {
		newVertices := max(1, mesh.Submeshes[index].VertexCount*percent/100)
		newIndices := max(3, (mesh.Submeshes[index].IndexCount*percent/100)/3*3)
		mesh.Submeshes[index].VertexStart = vertexStart
		mesh.Submeshes[index].VertexCount = newVertices
		mesh.Submeshes[index].IndexStart = indexStart
		mesh.Submeshes[index].IndexCount = newIndices
		vertexStart += newVertices
		indexStart += newIndices
		triangles += newIndices / 3
	}
	mesh.VertexCount = vertexStart
	mesh.TriangleCount = triangles
}

func batchIDs(batches []domain.BatchResult) []string {
	out := make([]string, 0, len(batches))
	for _, batch := range batches {
		out = append(out, batch.ID)
	}
	return out
}

func copyBatches(batches []domain.BatchResult) []domain.BatchResult {
	out := make([]domain.BatchResult, len(batches))
	for index, batch := range batches {
		out[index] = cloneBatch(batch)
	}
	return out
}

func batchCoreEqual(left, right domain.BatchResult) bool {
	left.Source = ""
	right.Source = ""
	return reflect.DeepEqual(left, right)
}
