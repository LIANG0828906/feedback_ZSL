package engine

import (
	"context"

	"mvccworkbench/internal/domain"
)

// EvaluateRead recomputes one read from the declared logical stream. It is the
// narrow recomputation entry point used by incremental invalidation.
func EvaluateRead(project *domain.Project, opID string) (*domain.ReadDecision, error) {
	var target domain.Op
	found := false
	for _, op := range project.Ops {
		if op.ID == opID {
			target = op
			found = true
			break
		}
	}
	if !found {
		return nil, domain.NewError(domain.EInputInvalidOperation, "read operation not found").With("op_id", opID)
	}
	if target.Kind != domain.OpRead {
		return nil, domain.NewError(domain.EInputInvalidOperation, "operation is not a read").With("op_id", opID).With("kind", target.Kind)
	}

	// Replaying the prefix also reconstructs GC reclamation state. This remains
	// deterministic and is scoped to the requested result's dependency prefix.
	runner := NewStreamRunner(project, DefaultOptions())
	for {
		op, done, err := runner.Step(context.Background())
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
		if op.ID == opID {
			decision, ok := runner.k.readMap[opID]
			if !ok {
				return nil, domain.NewError(domain.EInputInvalidOperation, "read produced no decision").With("op_id", opID)
			}
			return &decision, nil
		}
	}
	return nil, domain.NewError(domain.EInputInvalidOperation, "read operation not found during replay").With("op_id", opID)
}

// EvaluateGCAt replays the declared prefix through one GC step. The replay is
// deterministic and never reads a wall clock or checkpoint.
func EvaluateGCAt(ctx context.Context, project *domain.Project, stepID string) (*domain.GCDecision, error) {
	runner := NewStreamRunner(project, DefaultOptions())
	for {
		op, done, err := runner.Step(ctx)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
		if op.Kind == domain.OpGC && op.GCStepID == stepID {
			if len(runner.k.gcList) == 0 {
				return nil, domain.NewError(domain.EInputInvalidOperation, "GC step produced no decision").With("step_id", stepID)
			}
			decision := runner.k.gcList[len(runner.k.gcList)-1]
			return &decision, nil
		}
	}
	return nil, domain.NewError(domain.EInputInvalidOperation, "GC step not found").With("step_id", stepID)
}

func chainAt(project *domain.Project, itemID string, uptoIndex int) []domain.Version {
	chain := []domain.Version{}
	for _, op := range project.Ops {
		if op.Index > uptoIndex {
			break
		}
		if op.Kind != domain.OpWrite || op.ItemID != itemID {
			continue
		}
		var predecessorDelete *uint64
		if len(chain) > 0 {
			txnID := op.TxnID
			chain[len(chain)-1].DeleteTxn = &txnID
			predecessorDelete = &txnID
		}
		_ = predecessorDelete
		chain = append(chain, domain.Version{
			ID:        op.VersionID,
			ItemID:    itemID,
			CreateTxn: op.TxnID,
			Digest:    op.Digest,
			WriteGen:  op.WriteGen,
			ChainPos:  len(chain),
		})
	}
	return chain
}
