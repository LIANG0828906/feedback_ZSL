package engine

import (
	"fmt"
	"sort"
	"sync"

	"example.com/columnar/domain"
)

// PatchRequest is one independent per-batch patch solve. Logical commits still
// happen in operation order; this API parallelizes the pure solve stage across
// distinct batches under the shared budget.
type PatchRequest struct {
	Batch     domain.Batch
	Operation domain.Operation
}

type PatchSolution struct {
	BatchID         int
	Rows            int64
	CompressedBytes int64
	Digest          uint64
	Encoding        string
	RequiredBuffer  int64
	Budget          domain.BudgetObservation
}

// SolvePatches solves multiple independent batch patches. Duplicate targets are
// rejected rather than silently merged, and budget exhaustion is observable via
// deterministic waves/backpressure.
func SolvePatches(requests []PatchRequest, digestBits uint, budget domain.Budget) ([]PatchSolution, domain.BudgetObservation, error) {
	obs := domain.BudgetObservation{Workers: budget.Workers}
	if budget.Workers <= 0 {
		return nil, obs, domain.Errorf(domain.CodeBudgetWorkers, "budget.workers", "workers must be positive")
	}
	if budget.TotalDecodeBufferBytes <= 0 || budget.PerBatchDecodeBufferBytes <= 0 {
		return nil, obs, domain.Errorf(domain.CodeBudgetBuffer, "budget", "buffer limits must be positive")
	}
	sorted := append([]PatchRequest(nil), requests...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Batch.ID < sorted[j].Batch.ID })
	seen := map[int]bool{}
	type prepared struct {
		req      PatchRequest
		required int64
		index    int
	}
	jobs := make([]prepared, 0, len(sorted))
	for _, req := range sorted {
		if seen[req.Batch.ID] {
			return nil, obs, &domain.Error{Code: domain.CodeOperationOrder, Context: "parallel_patch", BatchID: req.Batch.ID, Message: "duplicate patch target"}
		}
		seen[req.Batch.ID] = true
		op := req.Operation
		if op.Kind != domain.OpPatch {
			return nil, obs, &domain.Error{Code: domain.CodeOperationKind, Op: op.Index, BatchID: req.Batch.ID, Context: "parallel_patch"}
		}
		if op.Range == nil || op.Range.From < 0 || op.Range.To < op.Range.From || op.Range.To > req.Batch.Rows {
			return nil, obs, &domain.Error{Code: domain.CodePatchRange, Op: op.Index, BatchID: req.Batch.ID, Context: "parallel_patch.range"}
		}
		rowBytes, overflow := domain.SatMulInt64(req.Batch.Rows, 8)
		if overflow {
			return nil, obs, &domain.Error{Code: domain.CodeOverflow, Op: op.Index, BatchID: req.Batch.ID}
		}
		required := req.Batch.DecodeBufferBytes
		if rowBytes > required {
			required = rowBytes
		}
		if required < 1 {
			required = 1
		}
		if required > budget.PerBatchDecodeBufferBytes {
			return nil, obs, &domain.Error{
				Code: domain.CodeBudgetBuffer, Context: "budget.per_batch_decode_buffer_bytes", BatchID: req.Batch.ID,
				Message: fmt.Sprintf("requires %d bytes, limit %d", required, budget.PerBatchDecodeBufferBytes),
			}
		}
		if required > budget.TotalDecodeBufferBytes {
			return nil, obs, &domain.Error{
				Code: domain.CodeBudgetBuffer, Context: "budget.total_decode_buffer_bytes", BatchID: req.Batch.ID,
				Message: fmt.Sprintf("requires %d bytes, total budget %d", required, budget.TotalDecodeBufferBytes),
			}
		}
		jobs = append(jobs, prepared{req: req, required: required, index: len(jobs)})
	}

	var waves [][]prepared
	var current []prepared
	var used int64
	for _, job := range jobs {
		if len(current) > 0 && (len(current) >= budget.Workers || used+job.required > budget.TotalDecodeBufferBytes) {
			obs.BackpressureEvents++
			obs.BackpressuredBatches = append(obs.BackpressuredBatches, job.req.Batch.ID)
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
	solutions := make([]PatchSolution, len(jobs))
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
				b := job.req.Batch
				op := job.req.Operation
				rows, under, overflow := domain.SatAddDelta(b.Rows, op.RowDelta)
				if under || overflow {
					return
				}
				bytes, under, overflow := domain.SatAddDelta(b.CompressedBytes, op.BytesDelta)
				if under || overflow {
					return
				}
				encoding := b.Encoding
				if op.Encoding != "" {
					encoding = op.Encoding
				}
				solutions[job.index] = PatchSolution{
					BatchID: b.ID, Rows: rows, CompressedBytes: bytes,
					Digest:   patchDigest(b.Digest, *op.Range, op.RowDelta, op.Digest, op.Encoding, digestBits),
					Encoding: encoding, RequiredBuffer: job.required, Budget: obs,
				}
			}()
		}
		wg.Wait()
	}
	obs.Waves = len(waves)
	// Re-run preconditions in deterministic order so overflow produces a stable
	// business error instead of a zero solution.
	for i, job := range jobs {
		b, op := job.req.Batch, job.req.Operation
		if _, under, overflow := domain.SatAddDelta(b.Rows, op.RowDelta); under {
			return nil, obs, &domain.Error{Code: domain.CodePatchResult, Op: op.Index, BatchID: b.ID, Message: "row delta underflow"}
		} else if overflow {
			return nil, obs, &domain.Error{Code: domain.CodeOverflow, Op: op.Index, BatchID: b.ID, Message: "row count overflow"}
		}
		if _, under, overflow := domain.SatAddDelta(b.CompressedBytes, op.BytesDelta); under {
			return nil, obs, &domain.Error{Code: domain.CodePatchResult, Op: op.Index, BatchID: b.ID, Message: "byte delta underflow"}
		} else if overflow {
			return nil, obs, &domain.Error{Code: domain.CodeOverflow, Op: op.Index, BatchID: b.ID, Message: "byte count overflow"}
		}
		solutions[i].Budget = domain.BudgetObservation{
			Workers: obs.Workers, Waves: obs.Waves, MaxInFlight: obs.MaxInFlight,
			BackpressureEvents: obs.BackpressureEvents, BackpressuredBatches: append([]int(nil), obs.BackpressuredBatches...),
		}
	}
	return solutions, obs, nil
}
