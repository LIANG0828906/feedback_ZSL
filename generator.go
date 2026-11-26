package locklab

import (
	"fmt"
)

type GenerateOptions struct {
	DeadlockDetection bool
	WorkerBudget      int
	MemoryBudgetBytes int64
}

func DefaultGenerateOptions() GenerateOptions {
	return GenerateOptions{
		DeadlockDetection: true,
		WorkerBudget:      DefaultWorkerBudget,
		MemoryBudgetBytes: DefaultMemoryBudgetByte,
	}
}

// GenerateProject builds a deterministic project from a fixed seed. Seed-
// dependent values are intentionally limited to data values and project names
// so the concurrency scenarios remain reproducible and diagnosable.
func GenerateProject(seed int64, options GenerateOptions) *Project {
	if options.WorkerBudget == 0 {
		options.WorkerBudget = DefaultWorkerBudget
	}
	if options.MemoryBudgetBytes == 0 {
		options.MemoryBudgetBytes = DefaultMemoryBudgetByte
	}
	suffix := fmt.Sprintf("%08x", uint32(seed))
	p := &Project{
		ID: fmt.Sprintf("locklab-%d", seed), Seed: seed,
		DeadlockDetection: options.DeadlockDetection,
		WorkerBudget:      options.WorkerBudget, MemoryBudgetBytes: options.MemoryBudgetBytes,
		ClockOriginMs: 1_700_000_000_000,
		Items: []Item{
			{ID: "accounts/A", Table: "accounts", InitialValue: "A0-" + suffix},
			{ID: "accounts/B", Table: "accounts", InitialValue: "B0-" + suffix},
			{ID: "accounts/C", Table: "accounts", InitialValue: "C0-" + suffix},
			{ID: "orders/O1", Table: "orders", InitialValue: "O10-" + suffix},
			{ID: "inventory/I1", Table: "inventory", InitialValue: "I10-" + suffix},
			{ID: "inventory/I2", Table: "inventory", InitialValue: "I20-" + suffix},
		},
	}

	p.Transactions = []Transaction{
		{
			ID: "T1", Priority: 10, Isolation: Serializable,
			Ops: []Operation{
				lockOp(1, 0, TableGranularity, "accounts", "", ModeIX, 1000, false),
				lockOp(2, 35, RowGranularity, "accounts", "accounts/A", ModeX, 1000, false),
				writeOp(3, 36, "accounts/A", "A1-"+suffix, "", 0),
				lockOp(4, 50, TableGranularity, "orders", "", ModeIS, 1000, false),
				lockOp(5, 50, RowGranularity, "orders", "orders/O1", ModeS, 1000, false),
				readOp(6, 55, "orders/O1", "", 0),
				commitOp(7, 60),
			},
		},
		{
			ID: "T2", Priority: 30, Isolation: ReadCommitted,
			Ops: []Operation{
				readOp(1, 5, "accounts/A", ModeS, 1000),
				readOp(2, 80, "accounts/A", ModeS, 1000),
				commitOp(3, 90),
			},
		},
		{
			ID: "T3", Priority: 25, Isolation: RepeatableRead,
			Ops: []Operation{
				lockOp(1, 10, TableGranularity, "accounts", "", ModeIX, 1000, false),
				readOp(2, 11, "accounts/B", ModeS, 1000),
				lockOp(3, 40, RowGranularity, "accounts", "accounts/C", ModeX, 1000, false),
				writeOp(4, 41, "accounts/C", "C3-"+suffix, "", 0),
				readOp(5, 70, "accounts/B", "", 0),
				commitOp(6, 75),
			},
		},
		{
			ID: "T4", Priority: 20, Isolation: ReadCommitted,
			Ops: []Operation{
				lockOp(1, 110, TableGranularity, "accounts", "", ModeIX, 1000, false),
				lockOp(2, 111, RowGranularity, "accounts", "accounts/C", ModeX, 1000, false),
				writeOp(3, 112, "accounts/C", "C4-"+suffix, "", 0),
				lockOp(4, 151, RowGranularity, "accounts", "accounts/B", ModeX, 499, false),
				writeOp(5, 160, "accounts/B", "B4-"+suffix, "", 0),
				commitOp(6, 290),
			},
		},
		{
			ID: "T5", Priority: 40, Isolation: ReadCommitted,
			Ops: []Operation{
				lockOp(1, 100, TableGranularity, "accounts", "", ModeIX, 1000, false),
				lockOp(2, 101, RowGranularity, "accounts", "accounts/B", ModeX, 1000, false),
				writeOp(3, 102, "accounts/B", "B5-"+suffix, "", 0),
				lockOp(4, 150, RowGranularity, "accounts", "accounts/C", ModeX, 500, false),
				writeOp(5, 160, "accounts/C", "C5-"+suffix, "", 0),
				commitOp(6, 300),
			},
		},
		{
			ID: "T6", Priority: 5, Isolation: ReadUncommitted,
			Ops: []Operation{
				readOp(1, 120, "accounts/B", "", 0),
				commitOp(2, 130),
			},
		},
		{
			ID: "T7", Priority: 15, Isolation: ReadCommitted,
			Ops: []Operation{
				lockOp(1, 121, RowGranularity, "accounts", "accounts/B", ModeX, 0, true),
				rollbackOp(2, 122),
			},
		},
		{
			ID: "T8", Priority: 50, Isolation: ReadCommitted,
			Ops: []Operation{
				lockOp(1, 205, TableGranularity, "inventory", "", ModeIX, 1000, false),
				readOp(2, 210, "inventory/I1", ModeS, 1000),
				readOp(3, 212, "inventory/I2", ModeS, 1000),
				lockOp(4, 220, RowGranularity, "inventory", "inventory/I1", ModeX, 1000, false),
				writeOp(5, 221, "inventory/I1", "I1-8-"+suffix, "", 0),
				commitOp(6, 240),
			},
		},
		{
			ID: "T9", Priority: 55, Isolation: ReadCommitted,
			Ops: []Operation{
				lockOp(1, 206, TableGranularity, "inventory", "", ModeIX, 1000, false),
				readOp(2, 214, "inventory/I1", ModeS, 1000),
				readOp(3, 215, "inventory/I2", ModeS, 1000),
				lockOp(4, 221, RowGranularity, "inventory", "inventory/I2", ModeX, 1000, false),
				writeOp(5, 222, "inventory/I2", "I2-9-"+suffix, "", 0),
				commitOp(6, 245),
			},
		},
	}
	if err := ValidateProject(p); err != nil {
		panic(fmt.Sprintf("generator produced invalid canonical project: %v", err))
	}
	return p
}

func lockOp(seq int, at int64, granularity Granularity, table, item string, mode LockMode, timeoutMs int64, noWait bool) Operation {
	return Operation{
		LocalSeq: seq, AtMs: at, Kind: OpLock, ItemID: item, Table: table,
		Granularity: granularity, Mode: mode, WaitTimeoutMs: timeoutMs, NoWait: noWait,
	}
}

func readOp(seq int, at int64, itemID string, mode LockMode, timeoutMs int64) Operation {
	return Operation{
		LocalSeq: seq, AtMs: at, Kind: OpRead, ItemID: itemID,
		Mode: mode, WaitTimeoutMs: timeoutMs,
	}
}

func writeOp(seq int, at int64, itemID, value string, mode LockMode, timeoutMs int64) Operation {
	return Operation{
		LocalSeq: seq, AtMs: at, Kind: OpWrite, ItemID: itemID,
		Mode: mode, Value: value, WaitTimeoutMs: timeoutMs,
	}
}

func commitOp(seq int, at int64) Operation {
	return Operation{LocalSeq: seq, AtMs: at, Kind: OpCommit}
}

func rollbackOp(seq int, at int64) Operation {
	return Operation{LocalSeq: seq, AtMs: at, Kind: OpRollback}
}
