package domain

import "sort"

const (
	SchemaV1 = "mvcc.project/v1"
	SchemaV2 = "mvcc.project/v2"
	SchemaV3 = "mvcc.project/v3"

	// ProjectModelVersion is the version of the lossless canonical model. It
	// is intentionally independent from the three external schema editions.
	ProjectModelVersion = "mvcc-project-canonical/v1"

	GeneratorVersion = "generator-1"
	KernelVersion    = "kernel-1"
	CheckpointFormat = "mvcc-checkpoint/v1"

	// TickMilliseconds is the declared deterministic conversion used by the
	// first schema. Logical values inside the kernel are always milliseconds.
	TickMilliseconds int64 = 10

	NoTxn uint64 = 0
)

type Isolation string

const (
	ReadCommitted  Isolation = "READ_COMMITTED"
	RepeatableRead Isolation = "REPEATABLE_READ"
	Serializable   Isolation = "SERIALIZABLE"
)

type TxnStatus string

const (
	TxnActive    TxnStatus = "ACTIVE"
	TxnCommitted TxnStatus = "COMMITTED"
	TxnAborted   TxnStatus = "ABORTED"
)

type OpKind string

const (
	OpRead     OpKind = "READ"
	OpWrite    OpKind = "WRITE"
	OpCommit   OpKind = "COMMIT"
	OpRollback OpKind = "ROLLBACK"
	OpGC       OpKind = "GC"
)

type CommitTieRule string

const (
	TieGlobalTxnID    CommitTieRule = "GLOBAL_TXN_ID_ASC"
	TieStartThenTxnID CommitTieRule = "START_MS_ASC_THEN_TXN_ID"
)

type ConflictRule string

const (
	ConflictFirstCommitterWins ConflictRule = "FIRST_COMMITTER_WINS"
)

type Item struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
}

type Txn struct {
	ID                uint64    `json:"id"`
	Ref               string    `json:"ref"`
	StartMS           int64     `json:"start_ms"`
	CommitMS          *int64    `json:"commit_ms,omitempty"`
	RollbackMS        *int64    `json:"rollback_ms,omitempty"`
	Isolation         Isolation `json:"isolation"`
	RelativeCommitSeq int       `json:"relative_commit_seq,omitempty"`
}

type Version struct {
	ID        string  `json:"id"`
	ItemID    string  `json:"item_id"`
	CreateTxn uint64  `json:"create_txn"`
	DeleteTxn *uint64 `json:"delete_txn,omitempty"`
	Digest    string  `json:"digest"`
	WriteGen  int     `json:"write_gen"`
	ChainPos  int     `json:"chain_pos"`
}

type Op struct {
	Index     int    `json:"index"`
	ID        string `json:"id"`
	Kind      OpKind `json:"kind"`
	TxnID     uint64 `json:"txn_id,omitempty"`
	ItemID    string `json:"item_id,omitempty"`
	AtMS      int64  `json:"at_ms"`
	VersionID string `json:"version_id,omitempty"`
	Digest    string `json:"digest,omitempty"`
	WriteGen  int    `json:"write_gen,omitempty"`
	GCStepID  string `json:"gc_step_id,omitempty"`
}

type Rules struct {
	CommitTieRule CommitTieRule `json:"commit_tie_rule"`
	ConflictRule  ConflictRule  `json:"conflict_rule"`
	TimeUnit      string        `json:"time_unit,omitempty"`
}

type Project struct {
	SchemaVersion string    `json:"schema_version"`
	SourceSchema  string    `json:"source_schema,omitempty"`
	ProjectID     string    `json:"project_id"`
	BatchID       string    `json:"batch_id"`
	Seed          int64     `json:"seed"`
	Generator     string    `json:"generator_version"`
	Rules         Rules     `json:"rules"`
	Items         []Item    `json:"items"`
	Txns          []Txn     `json:"txns"`
	Versions      []Version `json:"versions"`
	Ops           []Op      `json:"ops"`
}

func (p *Project) ItemIDs() []string {
	out := make([]string, 0, len(p.Items))
	for _, item := range p.Items {
		out = append(out, item.ID)
	}
	sort.Strings(out)
	return out
}

// Canonicalize establishes deterministic slice order for representations whose
// external organization differs (flat table vs grouped trees). Chain order is
// always represented by ChainPos; the global versions slice is grouped by the
// declared item order.
func (p *Project) Canonicalize() {
	itemOrder := map[string]int{}
	for i, item := range p.Items {
		itemOrder[item.ID] = i
	}
	sort.SliceStable(p.Versions, func(i, j int) bool {
		a, b := p.Versions[i], p.Versions[j]
		ai, ao := itemOrder[a.ItemID]
		bi, bo := itemOrder[b.ItemID]
		if !ao {
			ai = len(itemOrder)
		}
		if !bo {
			bi = len(itemOrder)
		}
		if ai != bi {
			return ai < bi
		}
		if a.ChainPos != b.ChainPos {
			return a.ChainPos < b.ChainPos
		}
		return a.ID < b.ID
	})
	sort.SliceStable(p.Ops, func(i, j int) bool {
		if p.Ops[i].Index != p.Ops[j].Index {
			return p.Ops[i].Index < p.Ops[j].Index
		}
		return p.Ops[i].ID < p.Ops[j].ID
	})
}

func (p *Project) TxnByID(id uint64) (Txn, bool) {
	for _, txn := range p.Txns {
		if txn.ID == id {
			return txn, true
		}
	}
	return Txn{}, false
}

func (p *Project) VersionsForItem(itemID string) []Version {
	out := make([]Version, 0)
	for _, version := range p.Versions {
		if version.ItemID == itemID {
			out = append(out, version)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ChainPos != out[j].ChainPos {
			return out[i].ChainPos < out[j].ChainPos
		}
		return out[i].ID < out[j].ID
	})
	return out
}

type SkipReason string

const (
	SkipUncommitted      SkipReason = "SKIP_UNCOMMITTED"
	SkipRolledBack       SkipReason = "SKIP_ROLLED_BACK"
	SkipTxnAfterSnapshot SkipReason = "SKIP_TXN_AFTER_SNAPSHOT"
	SkipDeleted          SkipReason = "SKIP_DELETED_IN_SNAPSHOT"
)

type SkippedVersion struct {
	VersionID string     `json:"version_id"`
	ItemID    string     `json:"item_id"`
	CreateTxn uint64     `json:"create_txn"`
	Reason    SkipReason `json:"reason"`
	Detail    string     `json:"detail,omitempty"`
}

type ReadDecision struct {
	OpID             string           `json:"op_id"`
	OpIndex          int              `json:"op_index"`
	TxnID            uint64           `json:"txn_id"`
	ItemID           string           `json:"item_id"`
	SnapshotID       string           `json:"snapshot_id"`
	Visible          bool             `json:"visible"`
	VisibleVersionID string           `json:"visible_version_id,omitempty"`
	Digest           string           `json:"digest,omitempty"`
	Skipped          []SkippedVersion `json:"skipped"`
	CommittedSet     []uint64         `json:"committed_set"`
	CutoffCommitSeq  int              `json:"cutoff_commit_seq"`
}

type ConflictDecision struct {
	Code          string    `json:"code"`
	Rule          string    `json:"rule"`
	ItemID        string    `json:"item_id"`
	WinnerTxnID   uint64    `json:"winner_txn_id"`
	LoserTxnID    uint64    `json:"loser_txn_id"`
	CompetingTxns [2]uint64 `json:"competing_txns"`
}

type WriteDecision struct {
	OpID                 string            `json:"op_id"`
	OpIndex              int               `json:"op_index"`
	TxnID                uint64            `json:"txn_id"`
	ItemID               string            `json:"item_id"`
	NewVersionID         string            `json:"new_version_id"`
	PredecessorVersionID string            `json:"predecessor_version_id,omitempty"`
	PredecessorDeleteTxn *uint64           `json:"predecessor_delete_txn,omitempty"`
	Conflict             *ConflictDecision `json:"conflict,omitempty"`
	Applied              bool              `json:"applied"`
}

type VisibilityAspect struct {
	AtCommitSeq    int               `json:"at_commit_seq"`
	CommitMS       int64             `json:"commit_ms"`
	CommittedOrder []uint64          `json:"committed_order"`
	VisibleHeads   map[string]string `json:"visible_heads"`
}

type CommitDecision struct {
	TxnID           uint64           `json:"txn_id"`
	CommitSeq       int              `json:"commit_seq"`
	CommitMS        int64            `json:"commit_ms"`
	TieRule         string           `json:"tie_rule"`
	SnapshotID      string           `json:"snapshot_id"`
	Aspect          VisibilityAspect `json:"aspect"`
	AffectedReadOps []string         `json:"affected_read_ops"`
}

type GCDecision struct {
	StepID              string   `json:"step_id"`
	AfterOpIndex        int      `json:"after_op_index"`
	OldestSnapshotID    string   `json:"oldest_snapshot_id"`
	ActiveSnapshotIDs   []string `json:"active_snapshot_ids"`
	ProtectedVersions   []string `json:"protected_versions"`
	ReclaimableVersions []string `json:"reclaimable_versions"`
	ReclaimedVersions   []string `json:"reclaimed_versions"`
	HorizonCommitSeq    int      `json:"horizon_commit_seq"`
}

type Truth struct {
	BatchID string                    `json:"batch_id"`
	Reads   map[string]ReadDecision   `json:"reads"`
	Writes  map[string]WriteDecision  `json:"writes"`
	Commits map[uint64]CommitDecision `json:"commits"`
	GC      []GCDecision              `json:"gc"`
}

func (t *Truth) ReadList() []ReadDecision {
	out := make([]ReadDecision, 0, len(t.Reads))
	for _, d := range t.Reads {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpIndex < out[j].OpIndex })
	return out
}

func (t *Truth) WriteList() []WriteDecision {
	out := make([]WriteDecision, 0, len(t.Writes))
	for _, d := range t.Writes {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpIndex < out[j].OpIndex })
	return out
}

func (t *Truth) CommitList() []CommitDecision {
	out := make([]CommitDecision, 0, len(t.Commits))
	for _, d := range t.Commits {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CommitSeq < out[j].CommitSeq })
	return out
}
