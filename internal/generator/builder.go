package generator

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"

	"mvccworkbench/internal/domain"
)

type activeSnapshot struct {
	id             string
	ownerTxn       uint64
	createdOpIndex int
	committedSet   map[uint64]bool
	cutoffSeq      int
	horizon        bool
}

type Builder struct {
	project domain.Project
	truth   domain.Truth

	status        map[uint64]domain.TxnStatus
	reclaimed     map[string]bool
	chains        map[string][]domain.Version
	snapshots     map[string]*activeSnapshot
	snapshotOrder []string
	readSnapshot  map[uint64]string
	conflictPairs map[string]*domain.ConflictDecision
	seed          int64
}

func NewBuilder(batchID string, seed int64, tie domain.CommitTieRule, conflict domain.ConflictRule) *Builder {
	return &Builder{
		project: domain.Project{
			SchemaVersion: domain.ProjectModelVersion,
			ProjectID:     "proj-" + batchID,
			BatchID:       batchID,
			Seed:          seed,
			Generator:     domain.GeneratorVersion,
			Rules: domain.Rules{
				CommitTieRule: tie,
				ConflictRule:  conflict,
				TimeUnit:      "millisecond",
			},
			Items:    []domain.Item{},
			Txns:     []domain.Txn{},
			Versions: []domain.Version{},
			Ops:      []domain.Op{},
		},
		truth: domain.Truth{
			BatchID: batchID,
			Reads:   map[string]domain.ReadDecision{},
			Writes:  map[string]domain.WriteDecision{},
			Commits: map[uint64]domain.CommitDecision{},
			GC:      []domain.GCDecision{},
		},
		status:        map[uint64]domain.TxnStatus{},
		reclaimed:     map[string]bool{},
		chains:        map[string][]domain.Version{},
		snapshots:     map[string]*activeSnapshot{},
		readSnapshot:  map[uint64]string{},
		conflictPairs: map[string]*domain.ConflictDecision{},
		seed:          seed,
	}
}

func (b *Builder) AddItem(id, description string) *Builder {
	b.project.Items = append(b.project.Items, domain.Item{ID: id, Description: description})
	return b
}

func (b *Builder) BeginTxn(startMS int64, iso domain.Isolation) uint64 {
	id := uint64(len(b.project.Txns) + 1)
	b.project.Txns = append(b.project.Txns, domain.Txn{
		ID:        id,
		Ref:       fmt.Sprintf("txn-%d", id),
		StartMS:   startMS,
		Isolation: iso,
	})
	b.status[id] = domain.TxnActive
	return id
}

func (b *Builder) digest(item string, gen int) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%d|%s|%s|%d", b.seed, b.project.BatchID, item, gen)))
	return "sha1:" + hex.EncodeToString(sum[:])[:16]
}

func (b *Builder) appendOp(kind domain.OpKind, txn uint64, item string, atMS int64) domain.Op {
	index := len(b.project.Ops) + 1
	op := domain.Op{
		Index:  index,
		ID:     fmt.Sprintf("OP-%03d", index),
		Kind:   kind,
		TxnID:  txn,
		ItemID: item,
		AtMS:   atMS,
	}
	b.project.Ops = append(b.project.Ops, op)
	return op
}

func (b *Builder) Write(txn uint64, item string, atMS int64) domain.WriteDecision {
	chain := b.project.VersionsForItem(item)
	gen := len(chain) + 1
	versionID := fmt.Sprintf("V-%s-%03d", item, gen)
	digest := b.digest(item, gen)
	op := b.appendOp(domain.OpWrite, txn, item, atMS)
	op.VersionID = versionID
	op.Digest = digest
	op.WriteGen = gen
	b.project.Ops[op.Index-1] = op

	var predecessorID string
	var predecessorDelete *uint64
	if len(chain) > 0 {
		predecessor := chain[len(chain)-1]
		predecessorID = predecessor.ID
		for i := range b.project.Versions {
			if b.project.Versions[i].ID == predecessor.ID {
				id := txn
				b.project.Versions[i].DeleteTxn = &id
				predecessorDelete = &id
				break
			}
		}
	}
	b.project.Versions = append(b.project.Versions, domain.Version{
		ID:        versionID,
		ItemID:    item,
		CreateTxn: txn,
		Digest:    digest,
		WriteGen:  gen,
		ChainPos:  gen - 1,
	})
	decision := domain.WriteDecision{
		OpID:                 op.ID,
		OpIndex:              op.Index,
		TxnID:                txn,
		ItemID:               item,
		NewVersionID:         versionID,
		PredecessorVersionID: predecessorID,
		PredecessorDeleteTxn: predecessorDelete,
		Applied:              true,
	}
	b.truth.Writes[op.ID] = decision
	return decision
}

func (b *Builder) Read(txn uint64, item string, atMS int64) domain.ReadDecision {
	op := b.appendOp(domain.OpRead, txn, item, atMS)
	return domain.ReadDecision{OpID: op.ID, OpIndex: op.Index, TxnID: txn, ItemID: item}
}

func (b *Builder) Commit(txn uint64, atMS int64) {
	op := b.appendOp(domain.OpCommit, txn, "", atMS)
	_ = op
	for i := range b.project.Txns {
		if b.project.Txns[i].ID == txn {
			ms := atMS
			b.project.Txns[i].CommitMS = &ms
		}
	}
	b.status[txn] = domain.TxnCommitted
}

func (b *Builder) Rollback(txn uint64, atMS int64) {
	op := b.appendOp(domain.OpRollback, txn, "", atMS)
	_ = op
	for i := range b.project.Txns {
		if b.project.Txns[i].ID == txn {
			ms := atMS
			b.project.Txns[i].RollbackMS = &ms
		}
	}
	b.status[txn] = domain.TxnAborted
}

func (b *Builder) GC(atMS int64) string {
	index := len(b.project.Ops) + 1
	gcCount := 0
	for _, op := range b.project.Ops {
		if op.Kind == domain.OpGC {
			gcCount++
		}
	}
	stepID := fmt.Sprintf("GC-STEP-%02d", gcCount+1)
	op := b.appendOp(domain.OpGC, domain.NoTxn, "", atMS)
	op.GCStepID = stepID
	op.Index = index
	b.project.Ops[index-1] = op
	return stepID
}

func (b *Builder) Build() (*domain.Project, *domain.Truth, error) {
	if err := b.finalizeCommitSequences(); err != nil {
		return nil, nil, err
	}
	b.replayTruth()
	b.finalizeConflicts()
	b.finalizeCommitAspectsAndImpacts()
	b.project.Canonicalize()
	if err := b.project.Validate(); err != nil {
		return nil, nil, err
	}
	truth := b.truth
	return &b.project, &truth, nil
}

func (b *Builder) finalizeCommitSequences() error {
	committed := make([]int, 0)
	for i, txn := range b.project.Txns {
		if txn.CommitMS != nil {
			committed = append(committed, i)
		}
	}
	sort.SliceStable(committed, func(a, c int) bool {
		x := b.project.Txns[committed[a]]
		y := b.project.Txns[committed[c]]
		if *x.CommitMS != *y.CommitMS {
			return *x.CommitMS < *y.CommitMS
		}
		switch b.project.Rules.CommitTieRule {
		case domain.TieStartThenTxnID:
			if x.StartMS != y.StartMS {
				return x.StartMS < y.StartMS
			}
			return x.ID < y.ID
		case domain.TieGlobalTxnID, "":
			return x.ID < y.ID
		default:
			return false
		}
	})
	for seq, idx := range committed {
		b.project.Txns[idx].RelativeCommitSeq = seq + 1
	}
	return nil
}

func (b *Builder) txnAtReplay(txnID uint64, processedIndex int) domain.TxnStatus {
	status := domain.TxnActive
	for _, op := range b.project.Ops {
		if op.Index > processedIndex {
			break
		}
		if op.TxnID != txnID {
			continue
		}
		if op.Kind == domain.OpCommit {
			status = domain.TxnCommitted
		}
		if op.Kind == domain.OpRollback {
			status = domain.TxnAborted
		}
	}
	return status
}
