package diagnostics

import (
	"context"

	"mvccworkbench/internal/checkpoint"
	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/incremental"
)

const (
	InjectDuplicateItem  = "duplicate_item"
	InjectBadDeleteRef   = "bad_delete_ref"
	InjectTimeOutOfRange = "time_out_of_range"
	InjectUnknownSchema  = "unknown_schema"
	InjectTruncatedCkpt  = "truncated_checkpoint"
	InjectBadSnapshot    = "version_mismatched_snapshot"
)

// InjectInput creates an invalid deep copy and returns its stable validation
// error. The source project is never mutated.
func InjectInput(project *domain.Project, kind string) (*domain.Project, error) {
	clone, err := incremental.CloneProject(project)
	if err != nil {
		return nil, err
	}
	switch kind {
	case InjectDuplicateItem:
		if len(clone.Items) == 0 {
			clone.Items = append(clone.Items, domain.Item{ID: "injected-item"})
		}
		original := clone.Items[0]
		clone.Items = append(clone.Items, domain.Item{ID: original.ID, Description: "injected duplicate"})
	case InjectBadDeleteRef:
		if len(clone.Versions) == 0 {
			return nil, domain.NewError(domain.EInputInvalidOperation, "project has no version for delete-ref injection")
		}
		txnID := uint64(999_999)
		clone.Versions[0].DeleteTxn = &txnID
	case InjectTimeOutOfRange:
		if len(clone.Txns) > 0 {
			clone.Txns[0].StartMS = domain.MaxLogicalMS + 100
		} else if len(clone.Ops) > 0 {
			clone.Ops[0].AtMS = -1
		} else {
			return nil, domain.NewError(domain.EInputInvalidOperation, "project has no time field for injection")
		}
	default:
		return nil, domain.NewError(domain.EInputInjectionRejected, "unsupported input injection").
			With("injection", kind).
			With("supported", []string{InjectDuplicateItem, InjectBadDeleteRef, InjectTimeOutOfRange})
	}
	return clone, clone.Validate()
}

func InjectCheckpointFault(ctx context.Context, store *checkpoint.Store, kind, itemID, snapshotID string) error {
	switch kind {
	case InjectTruncatedCkpt:
		if itemID == "" {
			itemID = "alpha"
		}
		if err := store.InjectTruncatedChain(ctx, itemID); err != nil {
			return err
		}
		_, err := store.Load(ctx)
		return err
	case InjectBadSnapshot:
		if snapshotID == "" {
			snapshotID = "SNAP-INJECTED-BAD"
		}
		if err := store.InjectSnapshotVersion(ctx, snapshotID, "snapshot/v9-injected"); err != nil {
			return err
		}
		_, err := store.Load(ctx)
		return err
	default:
		return domain.NewError(domain.EInputInjectionRejected, "unsupported checkpoint injection").With("injection", kind)
	}
}
