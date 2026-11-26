package kernel

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	CheckpointFormatVersion = "quotakernel-checkpoint-v1"
	KernelVersion           = "0.1.0"
)

type HoldRecord struct {
	HoldID    string `json:"hold_id"`
	TenantID  ID     `json:"tenant_id"`
	NodeID    ID     `json:"node_id"`
	Amount    int64  `json:"amount_bytes"`
	Status    string `json:"status"`
	RequestID string `json:"request_id"`
}

type Checkpoint struct {
	FormatVersion      string        `json:"format_version"`
	KernelVersion      string        `json:"kernel_version"`
	ProjectID          string        `json:"project_id"`
	ProjectFingerprint string        `json:"project_fingerprint"`
	CommittedTick      int64         `json:"committed_tick"`
	Budgets            Budgets       `json:"budgets"`
	Tenants            []TenantState `json:"tenants"`
	Nodes              []NodeState   `json:"nodes"`
	Holds              []HoldRecord  `json:"holds"`
	Queues             []QueueItem   `json:"queues"`
	CommitOrder        []string      `json:"commit_order"`
}

func BuildCheckpoint(p *Project, budgets Budgets, tick TickReport) *Checkpoint {
	cp := &Checkpoint{
		FormatVersion:      CheckpointFormatVersion,
		KernelVersion:      KernelVersion,
		ProjectID:          p.ProjectID,
		ProjectFingerprint: p.Fingerprint(),
		CommittedTick:      tick.Tick,
		Budgets:            budgets,
		Tenants:            append([]TenantState(nil), tick.Ledgers...),
		Nodes:              append([]NodeState(nil), tick.Nodes...),
		Queues:             append([]QueueItem(nil), tick.Queues...),
		CommitOrder: []string{
			"tenant_states",
			"node_states",
			"holds",
			"queues",
			"global_committed_tick",
		},
	}
	for _, a := range tick.Allocations {
		if a.PreHoldBytes > 0 {
			cp.Holds = append(cp.Holds, HoldRecord{
				HoldID: "hold:" + a.RequestID, TenantID: a.TenantID, NodeID: a.NodeID,
				Amount: a.PreHoldBytes, Status: "settled", RequestID: a.RequestID,
			})
		}
	}
	return cp
}

func SaveCheckpoint(path string, cp *Checkpoint) error {
	if cp == nil {
		return NewBizError(CodeCheckpointCorrupt, "nil checkpoint")
	}
	cp.FormatVersion = CheckpointFormatVersion
	if cp.CommitOrder == nil {
		cp.CommitOrder = []string{"tenant_states", "node_states", "holds", "queues", "global_committed_tick"}
	}
	bundle, err := json.Marshal(cp)
	if err != nil {
		return NewBizError(CodeCheckpointCorrupt, "encode checkpoint: %v", err)
	}
	tables := checkpointTables(cp, string(bundle))
	data, err := BuildNativeSQLite(tables)
	if err != nil {
		return err
	}
	if err := atomicWrite(path, data); err != nil {
		return err
	}
	_ = atomicWrite(path+".sql", []byte(CheckpointSQLScript(cp)))
	return nil
}

func LoadCheckpoint(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, NewBizError(CodeReferenceNotFound, "checkpoint %q does not exist", path).WithPath(path)
		}
		return nil, NewBizError(CodeCheckpointCorrupt, "read checkpoint: %v", err).WithPath(path)
	}
	if len(data) < len(sqliteMagic) || string(data[:len(sqliteMagic)]) != sqliteMagic {
		code := CodeCheckpointCorrupt
		if len(data) < 100 {
			code = CodeCheckpointTruncated
		}
		return nil, NewBizError(code, "not a SQLite database or file is truncated (size=%d)", len(data)).WithPath(path).
			WithDetail("size", len(data))
	}
	pageSize := int(data[16])<<8 | int(data[17])
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize > 65536 {
		return nil, NewBizError(CodeCheckpointCorrupt, "invalid SQLite page size %d", pageSize).WithPath(path)
	}
	if len(data) < pageSize {
		return nil, NewBizError(CodeCheckpointTruncated, "checkpoint size %d is smaller than page size %d", len(data), pageSize).
			WithPath(path).WithDetail("size", len(data)).WithDetail("page_size", pageSize)
	}
	bundle, err := ReadMetaBundle(data, pageSize, 2)
	if err != nil {
		return nil, err.(*BizError).WithPath(path)
	}
	var cp Checkpoint
	if err := json.Unmarshal([]byte(bundle), &cp); err != nil {
		return nil, NewBizError(CodeCheckpointCorrupt, "decode meta bundle: %v", err).WithPath(path)
	}
	if cp.FormatVersion != CheckpointFormatVersion {
		return nil, NewBizError(CodeCheckpointVersion, "checkpoint format %q != supported %q", cp.FormatVersion, CheckpointFormatVersion).
			WithPath(path).WithConstraint(ConstraintCheckpointVersion).
			WithDetail("checkpoint_version", cp.FormatVersion).WithDetail("supported_version", CheckpointFormatVersion)
	}
	return &cp, nil
}

type RecoveryReport struct {
	Action             string `json:"action"`
	CheckpointTick     int64  `json:"checkpoint_tick,omitempty"`
	RecomputedFromTick int64  `json:"recomputed_from_tick,omitempty"`
	ReasonCode         string `json:"reason_code,omitempty"`
	Reason             string `json:"reason,omitempty"`
}

// RecoverOrRun accepts only format- and fingerprint-matching checkpoints. Any
// version/fingerprint mismatch or corruption starts from the canonical source.
func RecoverOrRun(p *Project, budgets Budgets, path string) (*RunReport, *Checkpoint, *RecoveryReport, error) {
	cp, err := LoadCheckpoint(path)
	if err != nil {
		code := BizCode(err)
		report := &RecoveryReport{Action: "full_recompute", RecomputedFromTick: 0, ReasonCode: code, Reason: err.Error()}
		full, runErr := RunReference(p, budgets)
		if runErr != nil {
			return nil, nil, report, runErr
		}
		if tick := full.Tick(lastTick(p)); tick != nil {
			_ = SaveCheckpoint(path, BuildCheckpoint(p, budgets, *tick))
		}
		return full, nil, report, nil
	}
	if cp.ProjectFingerprint != p.Fingerprint() {
		report := &RecoveryReport{Action: "full_recompute", RecomputedFromTick: 0, ReasonCode: CodeProjectMismatch}
		full, runErr := RunReference(p, budgets)
		if runErr != nil {
			return nil, cp, report, runErr
		}
		if tick := full.Tick(lastTick(p)); tick != nil {
			_ = SaveCheckpoint(path, BuildCheckpoint(p, budgets, *tick))
		}
		return full, cp, report, nil
	}
	resumed, err := ResumeFromCheckpoint(p, budgets, cp)
	if err != nil {
		report := &RecoveryReport{Action: "full_recompute", RecomputedFromTick: 0, ReasonCode: BizCode(err), Reason: err.Error()}
		full, runErr := RunReference(p, budgets)
		return full, cp, report, runErr
	}
	return resumed, cp, &RecoveryReport{Action: "resume", CheckpointTick: cp.CommittedTick, RecomputedFromTick: cp.CommittedTick + 1}, nil
}

func ResumeFromCheckpoint(p *Project, budgets Budgets, cp *Checkpoint) (*RunReport, error) {
	tick := TickReport{Tick: cp.CommittedTick, Ledgers: cp.Tenants, Nodes: cp.Nodes, Queues: cp.Queues}
	cache := &ResultCache{Entries: map[int64]TickReport{cp.CommittedTick: tick}}
	suffix, err := runSeeded(p, budgets, ModeAuto, cp.CommittedTick+1, cache)
	if err != nil {
		return nil, err
	}
	full := &RunReport{ProjectID: p.ProjectID, Seed: p.Seed, Path: "recovered", Budgets: budgets}
	full.Ticks = append(full.Ticks, tick)
	full.Ticks = append(full.Ticks, suffix.Ticks...)
	return full, nil
}

func lastTick(p *Project) int64 {
	var max int64
	for _, e := range p.Events {
		if e.Tick > max {
			max = e.Tick
		}
	}
	return max
}

func atomicWrite(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".quota-checkpoint-*")
	if err != nil {
		return NewBizError(CodeCheckpointCorrupt, "create temp checkpoint: %v", err).WithPath(path)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return NewBizError(CodeCheckpointCorrupt, "write temp checkpoint: %v", err).WithPath(path)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return NewBizError(CodeCheckpointCorrupt, "fsync temp checkpoint: %v", err).WithPath(path)
	}
	if err := tmp.Close(); err != nil {
		return NewBizError(CodeCheckpointCorrupt, "close temp checkpoint: %v", err).WithPath(path)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return NewBizError(CodeCheckpointCorrupt, "atomic rename checkpoint: %v", err).WithPath(path)
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func checkpointTables(cp *Checkpoint, bundle string) []SQLiteTable {
	metaRows := [][]any{
		{"format_version", cp.FormatVersion},
		{"kernel_version", cp.KernelVersion},
		{"project_id", cp.ProjectID},
		{"project_fingerprint", cp.ProjectFingerprint},
		{"committed_tick", itoa(cp.CommittedTick)},
		{"bundle_json", bundle},
	}
	tenantRows := make([][]any, 0, len(cp.Tenants))
	for _, t := range cp.Tenants {
		tenantRows = append(tenantRows, []any{
			t.Tick, string(t.TenantID), t.QuotaBytes, t.GuaranteeBytes, t.BurstLimitBytes,
			t.UsedBytes, t.HeldBytes, t.AvailableQuotaBytes, t.GuaranteeRemainingBytes, t.BurstRemainingBytes,
			t.HardHeadroomBytes, t.EffectivePriority, t.EffectiveWeightBP,
		})
	}
	nodeRows := make([][]any, 0, len(cp.Nodes))
	for _, n := range cp.Nodes {
		nodeRows = append(nodeRows, []any{n.Tick, string(n.NodeID), n.CapacityBytes, n.OccupiedBytes, n.HeldBytes, n.AvailableBytes, n.ProtectedGuaranteeBytes})
	}
	holdRows := make([][]any, 0, len(cp.Holds))
	for _, h := range cp.Holds {
		holdRows = append(holdRows, []any{h.HoldID, string(h.TenantID), string(h.NodeID), h.Amount, h.Status, h.RequestID})
	}
	queueRows := make([][]any, 0, len(cp.Queues))
	for _, q := range cp.Queues {
		protected, _ := json.Marshal(q.ProtectedTenantIDs)
		queueRows = append(queueRows, []any{
			q.RequestID, string(q.TenantID), string(q.NodeID), q.AmountRemaining, q.EnqueuedTick,
			q.BudgetMS, q.RetryAfterTick, q.ReasonCode, q.ConstraintID, string(q.InvaderTenantID), string(protected),
		})
	}
	return []SQLiteTable{
		{Name: "meta", SQL: "CREATE TABLE meta(key TEXT, value TEXT)", Rows: metaRows},
		{Name: "tenant_state", SQL: "CREATE TABLE tenant_state(tick INTEGER, tenant_id TEXT, quota_bytes INTEGER, guarantee_bytes INTEGER, burst_limit_bytes INTEGER, used_bytes INTEGER, held_bytes INTEGER, available_bytes INTEGER, guarantee_remaining_bytes INTEGER, burst_remaining_bytes INTEGER, hard_headroom_bytes INTEGER, priority INTEGER, weight_bp INTEGER)", Rows: tenantRows},
		{Name: "node_state", SQL: "CREATE TABLE node_state(tick INTEGER, node_id TEXT, capacity_bytes INTEGER, occupied_bytes INTEGER, held_bytes INTEGER, available_bytes INTEGER, protected_guarantee_bytes INTEGER)", Rows: nodeRows},
		{Name: "hold", SQL: "CREATE TABLE hold(hold_id TEXT, tenant_id TEXT, node_id TEXT, amount_bytes INTEGER, status TEXT, request_id TEXT)", Rows: holdRows},
		{Name: "queue_item", SQL: "CREATE TABLE queue_item(request_id TEXT, tenant_id TEXT, node_id TEXT, amount_remaining INTEGER, enqueued_tick INTEGER, budget_ms INTEGER, retry_tick INTEGER, reason_code TEXT, constraint_id TEXT, invader_tenant_id TEXT, protected_tenant_ids TEXT)", Rows: queueRows},
	}
}

func CheckpointSQLScript(cp *Checkpoint) string {
	b, _ := json.MarshalIndent(cp, "", "  ")
	return "-- Quota kernel logical checkpoint.\n" +
		"-- SQLite native file is written atomically; this SQL script makes commit order auditable.\n" +
		"BEGIN IMMEDIATE;\n" +
		"DELETE FROM tenant_state;\nDELETE FROM node_state;\nDELETE FROM hold;\nDELETE FROM queue_item;\n" +
		"-- 1. tenant states\n" +
		"-- 2. node states\n" +
		"-- 3. holds and queues\n" +
		"-- 4. finally upsert meta.committed_tick = " + itoa(cp.CommittedTick) + "\n" +
		"COMMIT;\n" +
		"-- bundle_json:\n-- " + string(b) + "\n"
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
