package engine

import (
	"fmt"
	"sync"

	"example.com/columnar/domain"
)

type decodeJob struct {
	id       int
	required int64
	proof    uint64
}

// runDecodeWaves uses a deterministic bin-packing order before launching real
// goroutines. A job that cannot join the current wave is an observable
// backpressure event, never a dropped batch.
func runDecodeWaves(state map[int]domain.Batch, ids []int, budget domain.Budget) (domain.BudgetObservation, error) {
	obs := domain.BudgetObservation{Workers: budget.Workers}
	if budget.Workers <= 0 {
		return obs, domain.Errorf(domain.CodeBudgetWorkers, "budget.workers", "workers must be positive, got %d", budget.Workers)
	}
	if budget.TotalDecodeBufferBytes <= 0 || budget.PerBatchDecodeBufferBytes <= 0 {
		return obs, domain.Errorf(domain.CodeBudgetBuffer, "budget", "buffer limits must be positive")
	}
	jobs := make([]decodeJob, 0, len(ids))
	for _, id := range ids {
		b, ok := state[id]
		if !ok {
			return obs, &domain.Error{Code: domain.CodeBatchUnknown, Context: "scan.decode", BatchID: id}
		}
		rowBytes, overflow := domain.SatMulInt64(b.Rows, 8)
		if overflow {
			return obs, &domain.Error{Code: domain.CodeOverflow, Context: "scan.decode.rows", BatchID: id}
		}
		required := b.DecodeBufferBytes
		if rowBytes > required {
			required = rowBytes
		}
		if required < 1 {
			required = 1
		}
		if required > budget.PerBatchDecodeBufferBytes {
			return obs, &domain.Error{
				Code: domain.CodeBudgetBuffer, Context: "budget.per_batch_decode_buffer_bytes", BatchID: id,
				Message: fmt.Sprintf("batch requires %d bytes, per-batch limit is %d", required, budget.PerBatchDecodeBufferBytes),
			}
		}
		if required > budget.TotalDecodeBufferBytes {
			return obs, &domain.Error{
				Code: domain.CodeBudgetBuffer, Context: "budget.total_decode_buffer_bytes", BatchID: id,
				Message: fmt.Sprintf("batch requires %d bytes, total budget is %d", required, budget.TotalDecodeBufferBytes),
			}
		}
		jobs = append(jobs, decodeJob{id: id, required: required, proof: domain.Mix64(b.Digest, uint64(b.Rows), uint64(b.CompressedBytes))})
	}

	var waves [][]decodeJob
	var current []decodeJob
	var used int64
	for _, job := range jobs {
		if len(current) > 0 && (len(current) >= budget.Workers || used+job.required > budget.TotalDecodeBufferBytes) {
			obs.BackpressureEvents++
			obs.BackpressuredBatches = append(obs.BackpressuredBatches, job.id)
			waves = append(waves, current)
			current = nil
			used = 0
		}
		current = append(current, job)
		used += job.required
	}
	if len(current) > 0 {
		waves = append(waves, current)
	}

	for _, wave := range waves {
		if len(wave) > obs.MaxInFlight {
			obs.MaxInFlight = len(wave)
		}
		var wg sync.WaitGroup
		for _, job := range wave {
			job := job
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = domain.Mix64(job.proof, uint64(job.id), uint64(job.required))
			}()
		}
		wg.Wait()
	}
	obs.Waves = len(waves)
	return obs, nil
}

func checkOperationBudget(b domain.Batch, budget domain.Budget) (domain.BudgetObservation, error) {
	obs := domain.BudgetObservation{Workers: budget.Workers}
	if budget.Workers <= 0 {
		return obs, domain.Errorf(domain.CodeBudgetWorkers, "budget.workers", "workers must be positive")
	}
	rowBytes, overflow := domain.SatMulInt64(b.Rows, 8)
	if overflow {
		return obs, &domain.Error{Code: domain.CodeOverflow, Context: "operation.buffer", BatchID: b.ID}
	}
	required := b.DecodeBufferBytes
	if rowBytes > required {
		required = rowBytes
	}
	if required > budget.PerBatchDecodeBufferBytes {
		return obs, &domain.Error{
			Code: domain.CodeBudgetBuffer, Context: "budget.per_batch_decode_buffer_bytes", BatchID: b.ID,
			Message: fmt.Sprintf("batch requires %d bytes, per-batch limit is %d", required, budget.PerBatchDecodeBufferBytes),
		}
	}
	if required > budget.TotalDecodeBufferBytes {
		return obs, &domain.Error{
			Code: domain.CodeBudgetBuffer, Context: "budget.total_decode_buffer_bytes", BatchID: b.ID,
			Message: fmt.Sprintf("batch requires %d bytes, total budget is %d", required, budget.TotalDecodeBufferBytes),
		}
	}
	obs.Waves = 1
	obs.MaxInFlight = 1
	return obs, nil
}
