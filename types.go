package locklab

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type IsolationLevel string

const (
	ReadUncommitted IsolationLevel = "READ_UNCOMMITTED"
	ReadCommitted   IsolationLevel = "READ_COMMITTED"
	RepeatableRead  IsolationLevel = "REPEATABLE_READ"
	Serializable    IsolationLevel = "SERIALIZABLE"
)

var AllIsolationLevels = []IsolationLevel{
	ReadUncommitted,
	ReadCommitted,
	RepeatableRead,
	Serializable,
}

func ParseIsolation(s string) (IsolationLevel, bool) {
	n := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", "_"))
	n = strings.ReplaceAll(n, " ", "_")
	switch n {
	case "READ_UNCOMMITTED", "READUNCOMMITTED", "RU":
		return ReadUncommitted, true
	case "READ_COMMITTED", "READCOMMITTED", "RC":
		return ReadCommitted, true
	case "REPEATABLE_READ", "REPEATABLEREAD", "RR":
		return RepeatableRead, true
	case "SERIALIZABLE", "SER":
		return Serializable, true
	default:
		return "", false
	}
}

type LockMode string

const (
	ModeIS  LockMode = "IS"
	ModeIX  LockMode = "IX"
	ModeS   LockMode = "S"
	ModeSIX LockMode = "SIX"
	ModeX   LockMode = "X"
)

var AllLockModes = []LockMode{ModeIS, ModeIX, ModeS, ModeSIX, ModeX}

func ParseLockMode(s string) (LockMode, bool) {
	n := strings.ToUpper(strings.TrimSpace(s))
	switch n {
	case "IS", "INTENTION_SHARED", "INTENTIONSHARED":
		return ModeIS, true
	case "IX", "INTENTION_EXCLUSIVE", "INTENTIONEXCLUSIVE":
		return ModeIX, true
	case "S", "SHARED":
		return ModeS, true
	case "SIX", "SHARED_INTENTION_EXCLUSIVE", "SHAREDINTENTIONEXCLUSIVE":
		return ModeSIX, true
	case "X", "EXCLUSIVE":
		return ModeX, true
	default:
		return "", false
	}
}

// CanonicalLockCompatibility is the single compatibility relation used by the
// engine. Schema-specific flat tables and nested trees are validated against it.
var CanonicalLockCompatibility = map[LockMode]map[LockMode]bool{
	ModeIS:  {ModeIS: true, ModeIX: true, ModeS: true, ModeSIX: true, ModeX: false},
	ModeIX:  {ModeIS: true, ModeIX: true, ModeS: false, ModeSIX: false, ModeX: false},
	ModeS:   {ModeIS: true, ModeIX: false, ModeS: true, ModeSIX: false, ModeX: false},
	ModeSIX: {ModeIS: true, ModeIX: false, ModeS: false, ModeSIX: false, ModeX: false},
	ModeX:   {ModeIS: false, ModeIX: false, ModeS: false, ModeSIX: false, ModeX: false},
}

func ModesCompatible(a, b LockMode) bool {
	row, ok := CanonicalLockCompatibility[a]
	if !ok {
		return false
	}
	return row[b]
}

// MergeModes returns the least mode that subsumes both requested modes.
func MergeModes(a, b LockMode) LockMode {
	if a == ModeX || b == ModeX {
		return ModeX
	}
	if a == b {
		return a
	}
	switch {
	case (a == ModeS && b == ModeIX) || (a == ModeIX && b == ModeS):
		return ModeSIX
	case (a == ModeS && b == ModeSIX) || (a == ModeSIX && b == ModeS):
		return ModeSIX
	case (a == ModeIX && b == ModeSIX) || (a == ModeSIX && b == ModeIX):
		return ModeSIX
	case (a == ModeIS && b == ModeIX) || (a == ModeIX && b == ModeIS):
		return ModeIX
	case (a == ModeIS && b == ModeS) || (a == ModeS && b == ModeIS):
		return ModeS
	case (a == ModeIS && b == ModeSIX) || (a == ModeSIX && b == ModeIS):
		return ModeSIX
	default:
		return ModeX
	}
}

func ModeSubsumes(held, requested LockMode) bool {
	if held == requested || held == ModeX {
		return true
	}
	switch held {
	case ModeS:
		return requested == ModeIS
	case ModeIX:
		return requested == ModeIS
	case ModeSIX:
		return requested == ModeIS || requested == ModeS || requested == ModeIX
	}
	return false
}

type Granularity string

const (
	TableGranularity Granularity = "TABLE"
	RowGranularity   Granularity = "ROW"
)

func ParseGranularity(s string) (Granularity, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "TABLE", "T":
		return TableGranularity, true
	case "ROW", "R":
		return RowGranularity, true
	default:
		return "", false
	}
}

type OpKind string

const (
	OpRead     OpKind = "READ"
	OpWrite    OpKind = "WRITE"
	OpLock     OpKind = "LOCK"
	OpCommit   OpKind = "COMMIT"
	OpRollback OpKind = "ROLLBACK"
)

func ParseOpKind(s string) (OpKind, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "READ":
		return OpRead, true
	case "WRITE":
		return OpWrite, true
	case "LOCK":
		return OpLock, true
	case "COMMIT":
		return OpCommit, true
	case "ROLLBACK":
		return OpRollback, true
	default:
		return "", false
	}
}

type Item struct {
	ID           string `json:"id"`
	Table        string `json:"table"`
	InitialValue string `json:"initial_value"`
}

type Operation struct {
	LocalSeq      int         `json:"local_seq"`
	AtMs          int64       `json:"at_ms"`
	Kind          OpKind      `json:"kind"`
	ItemID        string      `json:"item_id,omitempty"`
	Table         string      `json:"table,omitempty"`
	Granularity   Granularity `json:"granularity,omitempty"`
	Mode          LockMode    `json:"mode,omitempty"`
	Value         string      `json:"value,omitempty"`
	WaitTimeoutMs int64       `json:"wait_timeout_ms"`
	NoWait        bool        `json:"no_wait,omitempty"`
}

func (o Operation) DeadlineMs() int64 { return o.AtMs + o.WaitTimeoutMs }

func (o Operation) IsTerminal() bool {
	return o.Kind == OpCommit || o.Kind == OpRollback
}

func (o Operation) NeedsLock() bool {
	return o.Kind == OpLock || o.Mode != ""
}

type Transaction struct {
	ID        string         `json:"id"`
	Priority  int            `json:"priority"`
	Isolation IsolationLevel `json:"isolation"`
	Ops       []Operation    `json:"ops"`
}

type Project struct {
	ID                string        `json:"id"`
	Seed              int64         `json:"seed"`
	DeadlockDetection bool          `json:"deadlock_detection"`
	WorkerBudget      int           `json:"worker_budget"`
	MemoryBudgetBytes int64         `json:"memory_budget_bytes"`
	ClockOriginMs     int64         `json:"clock_origin_ms"`
	Items             []Item        `json:"items"`
	Transactions      []Transaction `json:"transactions"`
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	out := *p
	out.Items = append([]Item(nil), p.Items...)
	out.Transactions = make([]Transaction, len(p.Transactions))
	for i, tx := range p.Transactions {
		out.Transactions[i] = tx
		out.Transactions[i].Ops = append([]Operation(nil), tx.Ops...)
	}
	return &out
}

// CanonicalDigest hashes the normalized domain model shared by all schema
// versions.
func (p *Project) CanonicalDigest() string {
	if p == nil {
		return ""
	}
	normalized := p.Clone()
	_ = ValidateProject(normalized)
	payload, _ := json.Marshal(struct {
		ID                string        `json:"id"`
		Seed              int64         `json:"seed"`
		DeadlockDetection bool          `json:"deadlock_detection"`
		WorkerBudget      int           `json:"worker_budget"`
		MemoryBudgetBytes int64         `json:"memory_budget_bytes"`
		ClockOriginMs     int64         `json:"clock_origin_ms"`
		Items             []Item        `json:"items"`
		Transactions      []Transaction `json:"transactions"`
	}{
		ID: normalized.ID, Seed: normalized.Seed, DeadlockDetection: normalized.DeadlockDetection,
		WorkerBudget: normalized.WorkerBudget, MemoryBudgetBytes: normalized.MemoryBudgetBytes,
		ClockOriginMs: normalized.ClockOriginMs, Items: normalized.Items,
		Transactions: normalized.Transactions,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

type ScheduledOp struct {
	GlobalStep int    `json:"global_step"`
	TxIndex    int    `json:"tx_index"`
	TxID       string `json:"tx_id"`
	OpIndex    int    `json:"op_index"`
	LocalSeq   int    `json:"local_seq"`
	AtMs       int64  `json:"at_ms"`
}

// BuildSchedule gives every declared operation one stable logical step.
func BuildSchedule(p *Project) []ScheduledOp {
	schedule := make([]ScheduledOp, 0)
	for txIndex, tx := range p.Transactions {
		for opIndex, op := range tx.Ops {
			schedule = append(schedule, ScheduledOp{
				TxIndex: txIndex, TxID: tx.ID, OpIndex: opIndex,
				LocalSeq: op.LocalSeq, AtMs: op.AtMs,
			})
		}
	}
	sort.SliceStable(schedule, func(i, j int) bool {
		a, b := schedule[i], schedule[j]
		if a.AtMs != b.AtMs {
			return a.AtMs < b.AtMs
		}
		if a.TxIndex != b.TxIndex {
			return a.TxIndex < b.TxIndex
		}
		return a.LocalSeq < b.LocalSeq
	})
	for i := range schedule {
		schedule[i].GlobalStep = i + 1
	}
	return schedule
}

func (p *Project) ItemByID(id string) (Item, bool) {
	for _, item := range p.Items {
		if item.ID == id {
			return item, true
		}
	}
	return Item{}, false
}

func (p *Project) HasTable(table string) bool {
	for _, item := range p.Items {
		if item.Table == table {
			return true
		}
	}
	return false
}

func ResourceKey(granularity Granularity, table, item string) string {
	if granularity == TableGranularity {
		return "table:" + table
	}
	return "row:" + item
}

func joinSorted(values []string) string {
	v := append([]string(nil), values...)
	sort.Strings(v)
	return strings.Join(v, ",")
}

func normalizeOpSignature(op Operation) string {
	return fmt.Sprintf("%d|%d|%s|%s|%s|%s|%s|%s|%d|%t",
		op.LocalSeq, op.AtMs, op.Kind, op.ItemID, op.Table,
		op.Granularity, op.Mode, op.Value, op.WaitTimeoutMs, op.NoWait)
}
