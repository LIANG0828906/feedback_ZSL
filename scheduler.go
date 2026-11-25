package dressbatch

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

type PoolConfig struct {
	MaxWorkers               int `json:"max_workers"`
	CandidateBufferPerWorker int `json:"candidate_buffer_per_worker"`
	QueueBudget              int `json:"queue_budget_parts"`
}

type QueueEvent struct {
	BatchID     string `json:"batch_id"`
	Reason      string `json:"reason"`
	WaitMillis  int64  `json:"wait_millis"`
	QueuedParts int    `json:"queued_parts"`
}

type BatchTaskResult struct {
	BatchID  string       `json:"batch_id"`
	Valid    bool         `json:"valid"`
	Attempts int          `json:"attempts"`
	Error    *DomainError `json:"error,omitempty"`
}

type SchedulerReport struct {
	Results                []BatchTaskResult `json:"results"`
	QueueEvents            []QueueEvent      `json:"queue_events"`
	MaxParallelism         int               `json:"max_parallelism"`
	CandidateBufferPerTask int               `json:"candidate_buffer_per_worker"`
	QueueBudgetParts       int               `json:"queue_budget_parts"`
}

type BatchValidator func(project *Project, batch Batch) error

type batchTask struct {
	Batch Batch
	Order int
}

type taskResult struct {
	Index  int
	Result BatchTaskResult
}

func ValidateBatchesWithPool(ctx context.Context, project *Project, batches []Batch, config PoolConfig) (*SchedulerReport, error) {
	return RunBatchValidationPool(ctx, project, batches, config, nil)
}

func RunBatchValidationPool(ctx context.Context, project *Project, batches []Batch, config PoolConfig, validator BatchValidator) (*SchedulerReport, error) {
	if err := validatePoolConfig(config); err != nil {
		return nil, err
	}
	if validator == nil {
		validator = ValidatePlannedBatch
	}
	tasks := make([]batchTask, 0, len(batches))
	for i, batch := range batches {
		if len(batch.PartIDs) > config.QueueBudget {
			return nil, withDetails(
				newError(CodeScheduler, "batch:"+batch.ID, "queue_budget_parts", "batch candidate set exceeds queue budget"),
				map[string]any{
					"queue_budget_parts": config.QueueBudget,
					"candidate_parts":    len(batch.PartIDs),
				},
			)
		}
		tasks = append(tasks, batchTask{Batch: batch, Order: i})
	}

	report := &SchedulerReport{
		Results:                make([]BatchTaskResult, 0, len(tasks)),
		QueueEvents:            make([]QueueEvent, 0),
		MaxParallelism:         config.MaxWorkers,
		CandidateBufferPerTask: config.CandidateBufferPerWorker,
		QueueBudgetParts:       config.QueueBudget,
	}
	if len(tasks) == 0 {
		return report, nil
	}

	workerChannels := make([]chan batchTask, config.MaxWorkers)
	for i := range workerChannels {
		workerChannels[i] = make(chan batchTask, config.CandidateBufferPerWorker)
	}
	results := make(chan taskResult, len(tasks))
	tokens := make(chan struct{}, config.QueueBudget)
	for i := 0; i < config.QueueBudget; i++ {
		tokens <- struct{}{}
	}
	var workers sync.WaitGroup
	workers.Add(config.MaxWorkers)
	for _, jobs := range workerChannels {
		go func(jobs <-chan batchTask) {
			defer workers.Done()
			for task := range jobs {
				result := runBatchTaskSafely(project, task, validator)
				for range task.Batch.PartIDs {
					tokens <- struct{}{}
				}
				results <- taskResult{Index: task.Order, Result: result}
			}
		}(jobs)
	}

	for i, task := range tasks {
		enqueued, event, err := dispatchTask(ctx, task, workerChannels, tokens)
		if event != nil {
			report.QueueEvents = append(report.QueueEvents, *event)
		}
		if err != nil {
			for _, jobs := range workerChannels {
				close(jobs)
			}
			workers.Wait()
			return nil, err
		}
		if !enqueued {
			return nil, newError(CodeScheduler, task.Batch.ID, "queue", "task was not enqueued (index %d)", i)
		}
	}
	for _, jobs := range workerChannels {
		close(jobs)
	}
	workers.Wait()
	close(results)

	indexed := make([]BatchTaskResult, len(tasks))
	for result := range results {
		result.Result.Attempts = 1
		indexed[result.Index] = result.Result
	}
	report.Results = indexed
	sort.Slice(report.QueueEvents, func(i, j int) bool {
		if report.QueueEvents[i].BatchID != report.QueueEvents[j].BatchID {
			return report.QueueEvents[i].BatchID < report.QueueEvents[j].BatchID
		}
		return report.QueueEvents[i].Reason < report.QueueEvents[j].Reason
	})
	return report, nil
}

func RetryFailedBatches(ctx context.Context, project *Project, batches []Batch, previous *SchedulerReport, config PoolConfig, validator BatchValidator) (*SchedulerReport, error) {
	if previous == nil {
		return nil, newError(CodeScheduler, "report", "previous", "previous scheduler report is required")
	}
	failed := make(map[string]struct{})
	for _, result := range previous.Results {
		if !result.Valid {
			failed[result.BatchID] = struct{}{}
		}
	}
	if len(failed) == 0 {
		copyReport := *previous
		copyReport.Results = append([]BatchTaskResult(nil), previous.Results...)
		copyReport.QueueEvents = append([]QueueEvent(nil), previous.QueueEvents...)
		return &copyReport, nil
	}
	retryBatches := make([]Batch, 0, len(failed))
	for _, batch := range batches {
		if _, retry := failed[batch.ID]; retry {
			retryBatches = append(retryBatches, batch)
		}
	}
	retryReport, err := RunBatchValidationPool(ctx, project, retryBatches, config, validator)
	if err != nil {
		return nil, err
	}
	retryByID := make(map[string]BatchTaskResult, len(retryReport.Results))
	for _, result := range retryReport.Results {
		retryByID[result.BatchID] = result
	}
	merged := *previous
	merged.Results = append([]BatchTaskResult(nil), previous.Results...)
	merged.QueueEvents = append(append([]QueueEvent(nil), previous.QueueEvents...), retryReport.QueueEvents...)
	for i := range merged.Results {
		if result, retried := retryByID[merged.Results[i].BatchID]; retried {
			result.Attempts += merged.Results[i].Attempts
			merged.Results[i] = result
		}
	}
	sort.Slice(merged.QueueEvents, func(i, j int) bool {
		if merged.QueueEvents[i].BatchID != merged.QueueEvents[j].BatchID {
			return merged.QueueEvents[i].BatchID < merged.QueueEvents[j].BatchID
		}
		return merged.QueueEvents[i].Reason < merged.QueueEvents[j].Reason
	})
	return &merged, nil
}

func validatePoolConfig(config PoolConfig) error {
	if config.MaxWorkers <= 0 {
		return newError(CodeScheduler, "pool", "max_workers", "must be greater than zero")
	}
	if config.CandidateBufferPerWorker <= 0 {
		return newError(CodeScheduler, "pool", "candidate_buffer_per_worker", "must be greater than zero")
	}
	if config.QueueBudget <= 0 {
		return newError(CodeScheduler, "pool", "queue_budget_parts", "must be greater than zero")
	}
	return nil
}

func dispatchTask(ctx context.Context, task batchTask, workerChannels []chan batchTask, tokens chan struct{}) (bool, *QueueEvent, error) {
	parts := len(task.Batch.PartIDs)
	waitStart := time.Time{}
	for acquired := 0; acquired < parts; {
		select {
		case <-tokens:
			acquired++
		default:
			if waitStart.IsZero() {
				waitStart = time.Now()
			}
			select {
			case <-tokens:
				acquired++
			case <-ctx.Done():
				return false, nil, newError(CodeScheduler, task.Batch.ID, "queue", "context canceled while waiting for queue budget: %v", ctx.Err())
			}
		}
	}
	enqueueStart := time.Time{}
	for {
		for _, jobs := range workerChannels {
			select {
			case jobs <- task:
				var event *QueueEvent
				if !waitStart.IsZero() {
					wait := time.Since(waitStart)
					event = &QueueEvent{
						BatchID:     task.Batch.ID,
						Reason:      "queue_part_budget",
						WaitMillis:  wait.Milliseconds(),
						QueuedParts: parts,
					}
				} else if !enqueueStart.IsZero() {
					event = &QueueEvent{
						BatchID:     task.Batch.ID,
						Reason:      "candidate_buffer_full",
						WaitMillis:  time.Since(enqueueStart).Milliseconds(),
						QueuedParts: parts,
					}
				}
				return true, event, nil
			default:
			}
		}
		if enqueueStart.IsZero() {
			enqueueStart = time.Now()
		}
		select {
		case <-ctx.Done():
			for i := 0; i < parts; i++ {
				select {
				case tokens <- struct{}{}:
				default:
				}
			}
			return false, nil, newError(CodeScheduler, task.Batch.ID, "queue", "context canceled while waiting for a worker buffer: %v", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func runBatchTaskSafely(project *Project, task batchTask, validator BatchValidator) (result BatchTaskResult) {
	result.BatchID = task.Batch.ID
	defer func() {
		if recovered := recover(); recovered != nil {
			result.Valid = false
			result.Error = withDetails(
				newError(CodeScheduler, "batch:"+task.Batch.ID, "panic", "batch worker panic: %v", recovered),
				map[string]any{"stack": string(debug.Stack())},
			)
		}
	}()
	if err := validator(project, task.Batch); err != nil {
		result.Valid = false
		if domainErr, ok := AsDomainError(err); ok {
			result.Error = domainErr
		} else {
			result.Error = newError(CodeScheduler, "batch:"+task.Batch.ID, "validation", "%v", err)
		}
		return result
	}
	result.Valid = true
	return result
}

func ValidatePlannedBatch(project *Project, batch Batch) error {
	byID := partMap(project)
	slots := make(map[string]struct{})
	vertexUsed := 0
	indexUsed := 0
	parts := make([]Part, 0, len(batch.PartIDs))
	for _, partID := range batch.PartIDs {
		part, exists := byID[partID]
		if !exists {
			return schedulerBatchError(batch.ID, "part_ids", "referenced part %q does not exist", partID)
		}
		parts = append(parts, part)
		vertexUsed += part.VertexCount
		indexUsed += part.EffectiveIndexCount()
		for _, slot := range part.MaterialSlots {
			slots[slot] = struct{}{}
		}
	}
	slotList := make([]string, 0, len(slots))
	for slot := range slots {
		slotList = append(slotList, slot)
	}
	sort.Strings(slotList)
	if !equalStrings(slotList, batch.MaterialSlots) {
		return schedulerBatchError(batch.ID, "material_slots", "material slot union does not match candidate parts")
	}
	if vertexUsed != batch.VertexUsed {
		return schedulerBatchError(batch.ID, "vertex_used", "computed vertex usage %d does not match batch value %d", vertexUsed, batch.VertexUsed)
	}
	if indexUsed != batch.IndexUsed {
		return schedulerBatchError(batch.ID, "index_used", "computed index usage %d does not match batch value %d", indexUsed, batch.IndexUsed)
	}
	if vertexUsed > project.Constraints.MaxVerticesPerBatch {
		return withDetails(
			schedulerBatchError(batch.ID, "vertex_used", "batch exceeds vertex budget"),
			map[string]any{
				"remaining_vertex_budget": project.Constraints.MaxVerticesPerBatch - vertexUsed + parts[len(parts)-1].VertexCount,
				"part_vertex_count":       parts[len(parts)-1].VertexCount,
			},
		)
	}
	if indexUsed > project.Constraints.MaxIndicesPerBatch {
		return withDetails(
			schedulerBatchError(batch.ID, "index_used", "batch exceeds index budget"),
			map[string]any{
				"remaining_index_budget": project.Constraints.MaxIndicesPerBatch - indexUsed + parts[len(parts)-1].EffectiveIndexCount(),
				"part_index_count":       parts[len(parts)-1].EffectiveIndexCount(),
			},
		)
	}
	if len(slotList) > project.Constraints.MaxMaterialSlotsPerBatch {
		return schedulerBatchError(batch.ID, "material_slots", "batch exceeds material slot budget")
	}
	if !project.Constraints.AllowCrossSkin {
		firstSkin := false
		for i, part := range parts {
			if i == 0 {
				firstSkin = part.Skin
			} else if part.Skin != firstSkin {
				return schedulerBatchError(batch.ID, "skin", "batch mixes skinned and rigid parts while cross-skin is disabled")
			}
		}
	}
	if len(batch.Placements) != len(parts) {
		return schedulerBatchError(batch.ID, "placements", "placement count does not match part count")
	}
	vertexCursor := 0
	indexCursor := 0
	for i, part := range parts {
		placement := batch.Placements[i]
		if placement.PartID != part.ID {
			return schedulerBatchError(batch.ID, fmt.Sprintf("placements[%d].part_id", i), "expected %q, got %q", part.ID, placement.PartID)
		}
		if placement.VertexOffset != vertexCursor || placement.VertexStart != vertexCursor || placement.VertexEnd != vertexCursor+part.VertexCount {
			return schedulerBatchError(batch.ID, fmt.Sprintf("placements[%d].vertex_offset", i), "invalid vertex interval")
		}
		if placement.IndexOffset != indexCursor || placement.IndexStart != indexCursor || placement.IndexEnd != indexCursor+part.EffectiveIndexCount() {
			return schedulerBatchError(batch.ID, fmt.Sprintf("placements[%d].index_offset", i), "invalid index interval")
		}
		submeshes := append([]Submesh(nil), part.Submeshes...)
		sort.Slice(submeshes, func(a, b int) bool {
			if submeshes[a].VertexStart != submeshes[b].VertexStart {
				return submeshes[a].VertexStart < submeshes[b].VertexStart
			}
			return submeshes[a].ID < submeshes[b].ID
		})
		if len(placement.Submeshes) != len(submeshes) {
			return schedulerBatchError(batch.ID, fmt.Sprintf("placements[%d].submeshes", i), "submesh placement count mismatch")
		}
		for j, submesh := range submeshes {
			expectedVertex := vertexCursor + submesh.VertexStart
			expectedIndex := indexCursor + submesh.IndexStart
			got := placement.Submeshes[j]
			if got.SubmeshID != submesh.ID ||
				got.VertexOffset != expectedVertex ||
				got.IndexOffset != expectedIndex ||
				got.VertexEnd != expectedVertex+submesh.VertexCount ||
				got.IndexEnd != expectedIndex+submesh.IndexCount {
				return schedulerBatchError(batch.ID, fmt.Sprintf("placements[%d].submeshes[%d]", i, j), "submesh offsets do not match source geometry")
			}
		}
		vertexCursor += part.VertexCount
		indexCursor += part.EffectiveIndexCount()
	}
	if batch.ContentHash != "" && BatchContentHash(batch) != batch.ContentHash {
		return schedulerBatchError(batch.ID, "content_hash", "batch content hash mismatch")
	}
	return nil
}

func schedulerBatchError(batchID, field, format string, args ...any) *DomainError {
	return newError(CodeScheduler, "batch:"+batchID, field, format, args...)
}
