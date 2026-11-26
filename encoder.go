package locklab

import (
	"encoding/json"
)

// EncodeProject emits one of the three schemas from the normalized model.
func EncodeProject(project *Project, version int) ([]byte, error) {
	if project == nil {
		return nil, NewError(EFieldRequired, "$", "project is nil")
	}
	copyProject := project.Clone()
	if err := ValidateProject(copyProject); err != nil {
		return nil, err
	}
	var (
		doc any
		err error
	)
	switch version {
	case 1:
		doc, err = encodeV1(copyProject)
	case 2:
		doc, err = encodeV2(copyProject)
	case 3:
		doc, err = encodeV3(copyProject)
	default:
		return nil, NewError(ESchemaUnsupported, "$", "unsupported schema version %d", version)
	}
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, WrapError(EInternal, "$", err, "cannot encode schema v%d", version)
	}
	return append(data, '\n'), nil
}

func stringPtr(s string) *string    { return &s }
func intPtr(v int) *int             { return &v }
func int64Ptr(v int64) *int64       { return &v }
func float64Ptr(v float64) *float64 { return &v }
func boolPtr(v bool) *bool          { return &v }

var v1ModeCodes = map[LockMode]string{
	ModeIS: "IS", ModeIX: "IX", ModeS: "S", ModeSIX: "SIX", ModeX: "X",
}

func encodeV1(p *Project) (v1Document, error) {
	doc := v1Document{
		SchemaVersion: intPtr(1), ProjectID: stringPtr(p.ID), Seed: int64Ptr(p.Seed),
		DeadlockDetection: boolPtr(p.DeadlockDetection),
		WorkerBudget:      intPtr(p.WorkerBudget), MemoryBudgetBytes: int64Ptr(p.MemoryBudgetBytes),
		ClockOriginMs: int64Ptr(p.ClockOriginMs),
	}
	for _, mode := range AllLockModes {
		row := v1LockMode{Code: stringPtr(v1ModeCodes[mode]), Mode: stringPtr(string(mode))}
		for _, other := range AllLockModes {
			if CanonicalLockCompatibility[mode][other] {
				row.CompatibleWith = append(row.CompatibleWith, string(other))
			}
		}
		doc.LockModes = append(doc.LockModes, row)
	}
	for _, item := range p.Items {
		doc.Items = append(doc.Items, v1Item{
			ItemID: stringPtr(item.ID), TableName: stringPtr(item.Table), InitialValue: item.InitialValue,
		})
	}
	for _, tx := range p.Transactions {
		rawTx := v1Transaction{
			TransactionID: stringPtr(tx.ID), Priority: intPtr(tx.Priority),
			Isolation: stringPtr(string(tx.Isolation)),
		}
		for _, op := range tx.Ops {
			rawOp := v1Operation{
				LocalSeq: intPtr(op.LocalSeq), AtMs: int64Ptr(op.AtMs), Kind: stringPtr(string(op.Kind)),
				ItemID: op.ItemID, Granularity: string(op.Granularity),
				Value: op.Value, NoWait: op.NoWait,
			}
			if op.Table != "" && op.Granularity == TableGranularity {
				rawOp.ItemID = ""
				rawOp.Granularity = string(TableGranularity)
				rawOp.TableName = op.Table
			}
			if op.Mode != "" {
				rawOp.LockMode = v1ModeCodes[op.Mode]
			}
			if op.NeedsLock() {
				rawOp.WaitTimeoutMs = int64Ptr(op.WaitTimeoutMs)
			}
			rawTx.Operations = append(rawTx.Operations, rawOp)
		}
		doc.Transactions = append(doc.Transactions, rawTx)
	}
	return doc, nil
}

func encodeV2(p *Project) (v2Document, error) {
	if p.MemoryBudgetBytes%1024 != 0 {
		return v2Document{}, NewError(EUnitAmbiguous, "$.memory_budget_bytes",
			"memory budget %d cannot be represented exactly in KiB", p.MemoryBudgetBytes)
	}
	doc := v2Document{
		Schema: stringPtr("lock-project/v2"), ProjectID: stringPtr(p.ID), Seed: int64Ptr(p.Seed),
		WorkerBudget: intPtr(p.WorkerBudget), MemoryBudgetKiB: int64Ptr(p.MemoryBudgetBytes / 1024),
		ClockOriginSeconds: float64Ptr(float64(p.ClockOriginMs) / 1000),
	}
	if p.DeadlockDetection {
		doc.Features = &v2FeatureFlags{DeadlockDetection: boolPtr(true)}
	}
	for _, item := range p.Items {
		doc.Items = append(doc.Items, v2Item{
			ID: stringPtr(item.ID), Table: stringPtr(item.Table), InitialValue: item.InitialValue,
		})
	}
	for _, tx := range p.Transactions {
		rawTx := v2Transaction{
			ID: stringPtr(tx.ID), Priority: intPtr(tx.Priority), Isolation: stringPtr(string(tx.Isolation)),
		}
		for _, op := range tx.Ops {
			rawOp := v2Operation{
				LocalSeq:  intPtr(op.LocalSeq),
				AtSeconds: float64Ptr(float64(op.AtMs) / 1000),
				Kind:      stringPtr(string(op.Kind)), ItemID: op.ItemID,
				Granularity: string(op.Granularity), Value: op.Value, NoWait: op.NoWait,
			}
			if op.Mode != "" {
				rawOp.LockMode = string(op.Mode)
			}
			if op.Granularity == TableGranularity {
				rawOp.ItemID = ""
				rawOp.TableName = op.Table
			}
			if op.NeedsLock() {
				rawOp.WaitDeadlineSeconds = float64Ptr(float64(op.AtMs+op.WaitTimeoutMs) / 1000)
			}
			rawTx.Operations = append(rawTx.Operations, rawOp)
		}
		doc.Transactions = append(doc.Transactions, rawTx)
	}
	return doc, nil
}

func encodeV3(p *Project) (v3Document, error) {
	doc := v3Document{
		SchemaVersion: intPtr(3), ProjectID: stringPtr(p.ID), Seed: int64Ptr(p.Seed),
		WorkerBudget: intPtr(p.WorkerBudget), MemoryBudgetBytes: int64Ptr(p.MemoryBudgetBytes),
		ClockOriginMs: int64Ptr(p.ClockOriginMs),
	}
	if p.DeadlockDetection {
		doc.DeadlockDetection = boolPtr(true)
	}
	root := &v3CompatNode{Group: "compatibility"}
	for _, mode := range AllLockModes {
		row := v3CompatNode{Mode: string(mode)}
		for _, other := range AllLockModes {
			row.Children = append(row.Children, v3CompatNode{
				Mode: string(other), Compatible: boolPtr(CanonicalLockCompatibility[mode][other]),
			})
		}
		root.Children = append(root.Children, row)
	}
	doc.LockCompatibility = root
	for _, item := range p.Items {
		doc.Items = append(doc.Items, v3Item{
			ID: stringPtr(item.ID), Table: stringPtr(item.Table), InitialValue: item.InitialValue,
		})
	}
	for _, tx := range p.Transactions {
		rawTx := v3Transaction{
			ID: stringPtr(tx.ID), Priority: intPtr(tx.Priority), Isolation: stringPtr(string(tx.Isolation)),
		}
		for _, op := range tx.Ops {
			rawOp := v3Operation{
				LocalSeq: intPtr(op.LocalSeq), AtMs: int64Ptr(op.AtMs), Kind: stringPtr(string(op.Kind)),
				ItemID: op.ItemID, Granularity: string(op.Granularity), Value: op.Value, NoWait: op.NoWait,
			}
			if op.Mode != "" {
				rawOp.LockMode = stringPtr(string(op.Mode))
			}
			if op.Granularity == TableGranularity {
				rawOp.ItemID = ""
				rawOp.Table = op.Table
			}
			if op.NeedsLock() {
				rawOp.WaitTimeoutMs = int64Ptr(op.WaitTimeoutMs)
			}
			rawTx.Operations = append(rawTx.Operations, rawOp)
		}
		doc.Transactions = append(doc.Transactions, rawTx)
	}
	return doc, nil
}
