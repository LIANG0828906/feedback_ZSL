package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"wardrobe/assembly"
	"wardrobe/domain"
	"wardrobe/planner"
	"wardrobe/scheduler"
	"wardrobe/schema"
	"wardrobe/store"
)

var (
	ErrProjectNotFound = errors.New("project not found")
	ErrCaseNotFound    = errors.New("truth case not found")
	ErrPartNotFound    = errors.New("part not found")
)

type Config struct {
	Scheduler scheduler.Config
	Store     *store.Store
}

type Engine struct {
	mu              sync.Mutex
	projects        map[string]domain.Project
	projectOrder    []string
	cases           map[string]domain.ExpectedCase
	cache           map[string]cachedCase
	schedulerConfig scheduler.Config
	store           *store.Store
}

type cachedCase struct {
	inputHash string
	result    domain.CaseResult
}

type BatchFailureError struct {
	BatchID string
	Err     error
}

func (e *BatchFailureError) Error() string {
	return fmt.Sprintf("batch %s failed: %v", e.BatchID, e.Err)
}

func (e *BatchFailureError) Unwrap() error {
	return e.Err
}

func DefaultSchedulerConfig() scheduler.Config {
	return scheduler.Config{
		MaxWorkers:               4,
		CandidateBufferPerWorker: 9,
		PendingQueueBudget:       18,
	}
}

func New(projects []domain.Project, config Config) (*Engine, error) {
	bundle := domain.Bundle{
		SchemaVersion:    domain.SchemaVersion,
		GeneratorVersion: domain.GeneratorVersion,
		AlgorithmVersion: domain.AlgorithmVersion,
		Projects:         projects,
	}
	if err := schema.Validate(&bundle); err != nil {
		return nil, err
	}
	return newEngine(projects, config)
}

func newEngine(projects []domain.Project, config Config) (*Engine, error) {
	cfg := config.Scheduler
	defaults := DefaultSchedulerConfig()
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = defaults.MaxWorkers
	}
	if cfg.CandidateBufferPerWorker <= 0 {
		cfg.CandidateBufferPerWorker = defaults.CandidateBufferPerWorker
	}
	if cfg.PendingQueueBudget <= 0 {
		cfg.PendingQueueBudget = defaults.PendingQueueBudget
	}
	if err := scheduler.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	e := &Engine{
		projects:        make(map[string]domain.Project, len(projects)),
		cases:           make(map[string]domain.ExpectedCase),
		cache:           make(map[string]cachedCase),
		schedulerConfig: cfg,
		store:           config.Store,
	}
	for _, project := range projects {
		if _, exists := e.projects[project.ProjectID]; exists {
			return nil, fmt.Errorf("duplicate project id %s", project.ProjectID)
		}
		e.projects[project.ProjectID] = cloneProject(project)
		e.projectOrder = append(e.projectOrder, project.ProjectID)
		for _, truthCase := range project.Truth.Cases {
			key := caseKey(project.ProjectID, truthCase.CaseID)
			e.cases[key] = cloneExpected(truthCase)
		}
	}
	sort.Strings(e.projectOrder)
	return e, nil
}

func (e *Engine) ProjectIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.projectOrder...)
}

func (e *Engine) Project(projectID string) (domain.Project, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	project, ok := e.projects[projectID]
	if !ok {
		return domain.Project{}, false
	}
	return cloneProject(project), true
}

func (e *Engine) Projects() []domain.Project {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]domain.Project, 0, len(e.projectOrder))
	for _, projectID := range e.projectOrder {
		out = append(out, cloneProject(e.projects[projectID]))
	}
	return out
}

func (e *Engine) CaseIDs(projectID string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	project, ok := e.projects[projectID]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(project.Truth.Cases))
	for _, truthCase := range project.Truth.Cases {
		out = append(out, truthCase.CaseID)
	}
	return out
}

func (e *Engine) TruthCase(projectID, caseID string) (domain.ExpectedCase, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	expected, ok := e.cases[caseKey(projectID, caseID)]
	if !ok {
		return domain.ExpectedCase{}, false
	}
	return cloneExpected(expected), true
}

// FullCase always computes from an empty cache.
func (e *Engine) FullCase(projectID, caseID string) (domain.CaseResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	project, expected, err := e.lookupLocked(projectID, caseID)
	if err != nil {
		return domain.CaseResult{}, err
	}
	return e.computeCase(project, expected, "full", false)
}

// BuildCase computes from empty cache, stores the full result in the in-memory
// cache, and writes a SQLite checkpoint when configured.
func (e *Engine) BuildCase(projectID, caseID string) (domain.CaseResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	project, expected, err := e.lookupLocked(projectID, caseID)
	if err != nil {
		return domain.CaseResult{}, err
	}
	result, err := e.computeCase(project, expected, "full", true)
	if err != nil {
		return domain.CaseResult{}, err
	}
	e.cache[caseKey(projectID, caseID)] = cachedCase{inputHash: result.InputHash, result: result}
	return result, nil
}

// EnsureCase reuses an in-memory result only when the semantic input hash is
// unchanged. Otherwise it falls back to a full recompute.
func (e *Engine) EnsureCase(projectID, caseID string) (domain.CaseResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	project, expected, err := e.lookupLocked(projectID, caseID)
	if err != nil {
		return domain.CaseResult{}, err
	}
	inputHash := domain.CaseInputHash(project, expected.SelectedPartIDs)
	if cached, ok := e.cache[caseKey(projectID, caseID)]; ok && cached.inputHash == inputHash {
		result := cloneCaseResult(cached.result)
		result.Source = "memory_cache_reuse"
		return result, nil
	}
	if e.store != nil {
		loaded, err := e.store.LoadCase(context.Background(), projectID, caseID, inputHash,
			project.Constraints.Version, domain.AlgorithmVersion)
		if err != nil {
			return domain.CaseResult{}, err
		}
		if loaded.Recovered && loaded.Result != nil {
			result := cloneCaseResult(*loaded.Result)
			for index := range result.Batches {
				result.Batches[index].Source = "sqlite_recovery"
			}
			e.cache[caseKey(projectID, caseID)] = cachedCase{inputHash: result.InputHash, result: result}
			return result, nil
		}
	}
	result, err := e.computeCase(project, expected, "full", true)
	if err != nil {
		return domain.CaseResult{}, err
	}
	e.cache[caseKey(projectID, caseID)] = cachedCase{inputHash: result.InputHash, result: result}
	return result, nil
}

func (e *Engine) ValidateAssembly(projectID string, selectedPartIDs []string) (assembly.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	project, ok := e.projects[projectID]
	if !ok {
		return assembly.Result{}, ErrProjectNotFound
	}
	return assembly.Evaluate(project, selectedPartIDs)
}

func (e *Engine) computeCase(
	project domain.Project,
	expected domain.ExpectedCase,
	source string,
	persist bool,
) (domain.CaseResult, error) {
	assemblyResult, err := assembly.Evaluate(project, expected.SelectedPartIDs)
	if err != nil {
		return domain.CaseResult{}, err
	}
	plan, err := planner.Plan(project, assemblyResult.AcceptedPartIDs)
	if err != nil {
		return domain.CaseResult{}, err
	}
	validated, scheduleReport, err := e.validateBatches(project, plan.Batches)
	if err != nil {
		return domain.CaseResult{}, err
	}
	result := domain.CaseResult{
		CaseID:            expected.CaseID,
		Description:       expected.Description,
		SelectedPartIDs:   assemblyResult.SelectedPartIDs,
		Valid:             assemblyResult.Valid,
		AcceptedPartIDs:   assemblyResult.AcceptedPartIDs,
		Conflicts:         assemblyResult.Conflicts,
		Batches:           validated,
		Decisions:         plan.Decisions,
		CommitSequence:    append([]string(nil), plan.CommitSequence...),
		InputHash:         domain.CaseInputHash(project, assemblyResult.SelectedPartIDs),
		ConstraintVersion: project.Constraints.Version,
		AlgorithmVersion:  domain.AlgorithmVersion,
		Source:            source,
		Schedule:          scheduleReport,
	}
	for index := range result.Batches {
		result.Batches[index].Source = source
	}
	if persist && e.store != nil {
		if err := e.store.SaveCase(context.Background(), project, result); err != nil {
			return domain.CaseResult{}, err
		}
	}
	return result, nil
}

func (e *Engine) validateBatches(project domain.Project, batches []domain.BatchResult) ([]domain.BatchResult, *domain.ScheduleReport, error) {
	if len(batches) == 0 {
		return []domain.BatchResult{}, &domain.ScheduleReport{
			PendingQueueBudget: e.schedulerConfig.PendingQueueBudget,
			MaxWorkers:         e.schedulerConfig.MaxWorkers,
			PerWorkerBuffer:    e.schedulerConfig.CandidateBufferPerWorker,
		}, nil
	}
	tasks := make([]scheduler.Task[domain.BatchResult], 0, len(batches))
	for _, batch := range batches {
		batchCopy := batch
		tasks = append(tasks, scheduler.Task[domain.BatchResult]{
			ID:     batch.ID,
			Weight: max(1, len(batch.PartIDs)),
			Run: func(ctx context.Context) (domain.BatchResult, error) {
				return layoutAndValidate(project, batchCopy)
			},
		})
	}
	results, stats, err := scheduler.Run(context.Background(), e.schedulerConfig, tasks)
	report := &domain.ScheduleReport{
		MaxQueueDepth:      stats.MaxQueueDepth,
		PendingQueueBudget: stats.PendingQueueBudget,
		MaxWorkers:         stats.MaxWorkers,
		PerWorkerBuffer:    stats.PerWorkerBuffer,
		TotalTaskWeight:    stats.TotalTaskWeight,
		WaitEvents:         make([]domain.ScheduleWaitEvent, 0, len(stats.WaitEvents)),
	}
	for _, event := range stats.WaitEvents {
		report.WaitEvents = append(report.WaitEvents, domain.ScheduleWaitEvent{
			TaskID:           event.TaskID,
			Stage:            event.Stage,
			Reason:           event.Reason,
			WaitMilliseconds: event.WaitMilliseconds,
		})
	}
	if err != nil {
		return nil, report, err
	}
	validated := make([]domain.BatchResult, len(results))
	for index, result := range results {
		if result.Err != nil {
			return nil, report, &BatchFailureError{BatchID: result.ID, Err: result.Err}
		}
		validated[index] = result.Value
	}
	return validated, report, nil
}

func layoutAndValidate(project domain.Project, batch domain.BatchResult) (domain.BatchResult, error) {
	if batch.ID == "" || batch.Ordinal <= 0 || batch.CommitSequence <= 0 {
		return domain.BatchResult{}, errors.New("invalid batch identity")
	}
	partMap := project.PartMap()
	out := domain.BatchResult{
		ID:             batch.ID,
		Ordinal:        batch.Ordinal,
		CommitSequence: batch.CommitSequence,
		PartIDs:        append([]string(nil), batch.PartIDs...),
		Parts:          make([]domain.PartPlacement, 0, len(batch.PartIDs)),
		MaterialSlots:  []string{},
		Dependencies:   append([]string(nil), batch.Dependencies...),
		CacheKey:       batch.CacheKey,
		Source:         batch.Source,
	}
	materialSet := make(map[string]struct{})
	allSkinned := false
	for partIndex, partID := range out.PartIDs {
		part, ok := partMap[partID]
		if !ok {
			return domain.BatchResult{}, fmt.Errorf("unknown part %s", partID)
		}
		if partIndex == 0 {
			allSkinned = part.Skinned
		} else if !project.Constraints.AllowCrossSkin && part.Skinned != allSkinned {
			return domain.BatchResult{}, fmt.Errorf("cross-skin mix at part %s", partID)
		}
		placement := domain.PartPlacement{
			PartID:       part.ID,
			VertexOffset: out.VertexUsed,
			IndexOffset:  out.IndexUsed,
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
				VertexOffset: out.VertexUsed + submesh.VertexStart,
				IndexOffset:  out.IndexUsed + submesh.IndexStart,
			})
			materialSet[submesh.MaterialSlot] = struct{}{}
		}
		out.Parts = append(out.Parts, placement)
		out.VertexUsed += part.Mesh.VertexCount
		out.IndexUsed += part.Mesh.TriangleCount * 3
	}
	if out.VertexUsed > project.Constraints.MaxVertices {
		return domain.BatchResult{}, fmt.Errorf("vertex budget exceeded: %d > %d", out.VertexUsed, project.Constraints.MaxVertices)
	}
	if out.IndexUsed > project.Constraints.MaxIndices {
		return domain.BatchResult{}, fmt.Errorf("index budget exceeded: %d > %d", out.IndexUsed, project.Constraints.MaxIndices)
	}
	for slot := range materialSet {
		out.MaterialSlots = append(out.MaterialSlots, slot)
	}
	sort.Strings(out.MaterialSlots)
	if len(out.MaterialSlots) > project.Constraints.MaxMaterialSlots {
		return domain.BatchResult{}, fmt.Errorf("material slot budget exceeded: %d > %d", len(out.MaterialSlots), project.Constraints.MaxMaterialSlots)
	}
	if !reflect.DeepEqual(batch.PartIDs, out.PartIDs) ||
		!reflect.DeepEqual(batch.Parts, out.Parts) ||
		!reflect.DeepEqual(batch.MaterialSlots, out.MaterialSlots) ||
		batch.VertexUsed != out.VertexUsed ||
		batch.IndexUsed != out.IndexUsed {
		return domain.BatchResult{}, errors.New("planner layout does not match recomputed offsets")
	}
	return out, nil
}

func (e *Engine) lookupLocked(projectID, caseID string) (domain.Project, domain.ExpectedCase, error) {
	project, ok := e.projects[projectID]
	if !ok {
		return domain.Project{}, domain.ExpectedCase{}, ErrProjectNotFound
	}
	expected, ok := e.cases[caseKey(projectID, caseID)]
	if !ok {
		return domain.Project{}, domain.ExpectedCase{}, ErrCaseNotFound
	}
	return cloneProject(project), cloneExpected(expected), nil
}

func caseKey(projectID, caseID string) string {
	return projectID + "\x00" + caseID
}

func cloneProject(project domain.Project) domain.Project {
	out := project
	out.Mounts = append([]domain.Mount(nil), project.Mounts...)
	out.Materials = append([]domain.MaterialSlot(nil), project.Materials...)
	out.Parts = make([]domain.Part, len(project.Parts))
	for index, part := range project.Parts {
		out.Parts[index] = part
		out.Parts[index].Mesh.Submeshes = append([]domain.Submesh(nil), part.Mesh.Submeshes...)
	}
	out.ExclusionRules = append([]domain.ExclusionRule(nil), project.ExclusionRules...)
	out.Truth.Cases = make([]domain.ExpectedCase, len(project.Truth.Cases))
	for index, truthCase := range project.Truth.Cases {
		out.Truth.Cases[index] = cloneExpected(truthCase)
	}
	return out
}

func cloneExpected(expected domain.ExpectedCase) domain.ExpectedCase {
	out := expected
	out.SelectedPartIDs = append([]string(nil), expected.SelectedPartIDs...)
	out.AcceptedPartIDs = append([]string(nil), expected.AcceptedPartIDs...)
	out.Conflicts = append([]domain.Conflict(nil), expected.Conflicts...)
	out.CommitSequence = append([]string(nil), expected.CommitSequence...)
	out.Decisions = append([]domain.Decision(nil), expected.Decisions...)
	out.Batches = make([]domain.BatchResult, len(expected.Batches))
	for index, batch := range expected.Batches {
		out.Batches[index] = cloneBatch(batch)
	}
	return out
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

func cloneCaseResult(result domain.CaseResult) domain.CaseResult {
	out := result
	out.SelectedPartIDs = append([]string(nil), result.SelectedPartIDs...)
	out.AcceptedPartIDs = append([]string(nil), result.AcceptedPartIDs...)
	out.Conflicts = append([]domain.Conflict(nil), result.Conflicts...)
	out.Decisions = append([]domain.Decision(nil), result.Decisions...)
	out.CommitSequence = append([]string(nil), result.CommitSequence...)
	out.ReusedBatchIDs = append([]string(nil), result.ReusedBatchIDs...)
	out.InvalidatedBatchIDs = append([]string(nil), result.InvalidatedBatchIDs...)
	out.Batches = make([]domain.BatchResult, len(result.Batches))
	for index, batch := range result.Batches {
		out.Batches[index] = cloneBatch(batch)
	}
	return out
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func NormalizeResultForHash(result domain.CaseResult) domain.CaseResult {
	out := cloneCaseResult(result)
	out.Source = ""
	out.ReusedBatchIDs = nil
	out.InvalidatedBatchIDs = nil
	out.Schedule = nil
	for index := range out.Batches {
		out.Batches[index].Source = ""
	}
	return out
}

func ResultHash(result domain.CaseResult) string {
	return domain.HashJSON(NormalizeResultForHash(result))
}
