package workbench

import (
	"context"
	"sort"

	"performanceexpressions/workspace/checkpoint"
	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/session"
)

type RecoveryReport struct {
	Invalidated       bool     `json:"invalidated"`
	Reasons           []string `json:"reasons,omitempty"`
	ReplayedBatches   int      `json:"replayed_batches"`
	LoadedBatches     int      `json:"loaded_batches"`
	LoadedCheckpoints int      `json:"loaded_checkpoints"`
	BatchIDs          []string `json:"batch_ids"`
}

// Recover restores a session from SQLite. If any content hash or version is
// invalid, the store has already invalidated every checkpoint and the committed
// batch operations are replayed from the empty working state.
func Recover(ctx context.Context, store *checkpoint.Store, base domain.Project, params domain.Parameters) (*session.Session, RecoveryReport, error) {
	report := RecoveryReport{}
	if err := store.EnsureProject(ctx, base); err != nil {
		return nil, report, err
	}
	recovery, err := store.Load(ctx, base, params)
	if err != nil {
		return nil, report, err
	}
	report.Invalidated = recovery.Invalidated
	report.Reasons = append([]string(nil), recovery.Reasons...)
	report.LoadedCheckpoints = len(recovery.Rows)
	if recovery.Invalidated {
		sess, err := session.New(base, params, session.Options{Sink: store})
		if err != nil {
			return nil, report, err
		}
		for _, batch := range recovery.ReplayBatches {
			if len(batch.Operations) == 0 {
				return nil, report, domain.NewError(domain.CodeCommit, "cannot replay batch %q without operations", batch.BatchID)
			}
			for _, op := range batch.Operations {
				if err := sess.ApplyEdit(op); err != nil {
					return nil, report, err
				}
			}
			ids := batchSegments(batch.Operations)
			receipt, err := sess.CommitBatch(ctx, batch.BatchID, ids)
			if err != nil {
				return nil, report, err
			}
			report.BatchIDs = append(report.BatchIDs, receipt.BatchID)
			report.ReplayedBatches++
		}
		return sess, report, nil
	}
	sess, err := session.New(base, params, session.Options{Sink: store, KnownBatches: recovery.BatchIDs})
	if err != nil {
		return nil, report, err
	}
	if err := sess.RestoreCommitted(recovery.Project.Segments, recovery.BatchIDs); err != nil {
		return nil, report, err
	}
	report.BatchIDs = append([]string(nil), recovery.BatchIDs...)
	report.LoadedBatches = len(recovery.BatchIDs)
	return sess, report, nil
}

func batchSegments(ops []domain.EditOp) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, op := range ops {
		if !seen[op.SegmentID] {
			seen[op.SegmentID] = true
			ids = append(ids, op.SegmentID)
		}
	}
	sort.Strings(ids)
	return ids
}
