package domain

import "fmt"

const MaxLogicalMS int64 = 10_000_000

// Validate performs input-integrity checks without mutating p. Loaders and
// injection tests rely on the stable error codes and contextual paths.
func (p *Project) Validate() error {
	if p.ProjectID == "" {
		return NewError(EInputInvalidRef, "project_id is required").At("project_id")
	}
	if p.BatchID == "" {
		return NewError(EInputInvalidRef, "batch_id is required").At("batch_id")
	}

	seenItems := map[string]int{}
	for i, item := range p.Items {
		path := fmt.Sprintf("items[%d]", i)
		if item.ID == "" {
			return NewError(EInputInvalidRef, "data item id is empty").At(path + ".id")
		}
		if first, dup := seenItems[item.ID]; dup {
			return NewError(EInputDuplicateItem, "duplicate data item").
				At(path+".id").
				With("item_id", item.ID).
				With("first_index", first).
				With("duplicate_index", i)
		}
		seenItems[item.ID] = i
	}

	seenTxns := map[uint64]int{}
	for i, txn := range p.Txns {
		path := fmt.Sprintf("transactions[%d]", i)
		if txn.ID == NoTxn {
			return NewError(EInputInvalidRef, "transaction id must be non-zero").At(path + ".id")
		}
		if first, dup := seenTxns[txn.ID]; dup {
			return NewError(EInputDuplicateTxn, "duplicate transaction").
				At(path+".id").With("txn_id", txn.ID).
				With("first_index", first).With("duplicate_index", i)
		}
		seenTxns[txn.ID] = i
		if txn.StartMS < 0 || txn.StartMS > MaxLogicalMS {
			return NewError(EInputTimeOutOfRange, "transaction start logical time is outside declared range").
				At(path+".start_ms").With("txn_id", txn.ID).With("value", txn.StartMS).
				With("min", 0).With("max", MaxLogicalMS)
		}
		if txn.CommitMS != nil && (*txn.CommitMS < txn.StartMS || *txn.CommitMS > MaxLogicalMS) {
			return NewError(EInputTimeOutOfRange, "transaction commit logical time is outside declared range").
				At(path+".commit_ms").With("txn_id", txn.ID).With("value", *txn.CommitMS)
		}
		switch txn.Isolation {
		case ReadCommitted, RepeatableRead, Serializable:
		default:
			return NewError(EInputInvalidIsolation, "unsupported isolation level").
				At(path+".isolation").With("txn_id", txn.ID).With("value", txn.Isolation)
		}
	}

	seenVersions := map[string]int{}
	for i, version := range p.Versions {
		path := fmt.Sprintf("versions[%d]", i)
		if version.ID == "" {
			return NewError(EInputInvalidRef, "version id is empty").At(path + ".id")
		}
		if first, dup := seenVersions[version.ID]; dup {
			return NewError(EInputDuplicateVersion, "duplicate version").
				At(path+".id").With("version_id", version.ID).
				With("first_index", first).With("duplicate_index", i)
		}
		seenVersions[version.ID] = i
		if _, ok := seenItems[version.ItemID]; !ok {
			return NewError(EInputInvalidRef, "version references unknown data item").
				At(path+".item_id").With("version_id", version.ID).With("item_id", version.ItemID)
		}
		if _, ok := seenTxns[version.CreateTxn]; !ok {
			return NewError(EInputBadCreateRef, "version create transaction does not exist").
				At(path+".create_txn").With("version_id", version.ID).
				With("create_txn", version.CreateTxn)
		}
		if version.DeleteTxn != nil {
			if _, ok := seenTxns[*version.DeleteTxn]; !ok {
				return NewError(EInputBadDeleteRef, "version delete transaction does not exist").
					At(path+".delete_txn").With("version_id", version.ID).
					With("delete_txn", *version.DeleteTxn).With("item_id", version.ItemID)
			}
		}
	}

	seenOps := map[int]bool{}
	for i, op := range p.Ops {
		path := fmt.Sprintf("operations[%d]", i)
		if op.Index < 0 || seenOps[op.Index] {
			return NewError(EInputInvalidOperation, "operation index is invalid or duplicated").
				At(path+".index").With("op_id", op.ID).With("index", op.Index)
		}
		seenOps[op.Index] = true
		if op.AtMS < 0 || op.AtMS > MaxLogicalMS {
			return NewError(EInputTimeOutOfRange, "operation logical time is outside declared range").
				At(path+".at_ms").With("op_id", op.ID).With("value", op.AtMS)
		}
		switch op.Kind {
		case OpRead, OpWrite, OpCommit, OpRollback:
			if op.TxnID == NoTxn {
				return NewError(EInputInvalidOperation, "operation requires transaction").
					At(path+".txn_id").With("op_id", op.ID).With("kind", op.Kind)
			}
			if _, ok := seenTxns[op.TxnID]; !ok {
				return NewError(EInputInvalidRef, "operation references unknown transaction").
					At(path+".txn_id").With("op_id", op.ID).With("txn_id", op.TxnID)
			}
		case OpGC:
		default:
			return NewError(EInputInvalidOperation, "unknown operation kind").
				At(path+".kind").With("op_id", op.ID).With("kind", op.Kind)
		}
		if op.Kind == OpRead || op.Kind == OpWrite {
			if _, ok := seenItems[op.ItemID]; !ok {
				return NewError(EInputInvalidRef, "operation references unknown data item").
					At(path+".item_id").With("op_id", op.ID).With("item_id", op.ItemID)
			}
		}
	}
	return nil
}
