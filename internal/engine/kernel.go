package engine

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"mvccworkbench/internal/domain"
)

const HorizonSnapshotID = "SNAP-HORIZON"

type Options struct {
	Workers         int
	ItemBufferLimit int
	ChainCap        int
	Now             func() time.Time
}

func DefaultOptions() Options {
	return Options{Workers: 2, ItemBufferLimit: 64, ChainCap: 1024}
}

type Report struct {
	ProjectID string
	BatchID   string
	Reads     []domain.ReadDecision
	Writes    []domain.WriteDecision
	Commits   []domain.CommitDecision
	GC        []domain.GCDecision
	Metrics   map[string]time.Duration
	Reclaimed map[string]bool
}

type snapshot struct {
	id             string
	ownerTxn       uint64
	createdOpIndex int
	committedSet   map[uint64]bool
	cutoffSeq      int
	horizon        bool
}

type Kernel struct {
	project *domain.Project
	opts    Options

	status        map[uint64]domain.TxnStatus
	committed     map[uint64]bool
	commitSeq     map[uint64]int
	chains        map[string][]domain.Version
	reclaimed     map[string]bool
	snapshots     map[string]*snapshot
	snapshotOrder []string
	readSnapshot  map[uint64]string

	readMap  map[string]domain.ReadDecision
	writeMap map[string]domain.WriteDecision
	gcList   []domain.GCDecision
	metrics  map[string]time.Duration
}

func Run(ctx context.Context, project *domain.Project, opts Options) (*Report, error) {
	if opts.Workers == 0 {
		opts.Workers = DefaultOptions().Workers
	}
	if opts.ItemBufferLimit == 0 {
		opts.ItemBufferLimit = DefaultOptions().ItemBufferLimit
	}
	if opts.ChainCap == 0 {
		opts.ChainCap = DefaultOptions().ChainCap
	}
	k := &Kernel{
		project:      project,
		opts:         opts,
		status:       map[uint64]domain.TxnStatus{},
		committed:    map[uint64]bool{},
		commitSeq:    DeriveCommitOrder(project),
		chains:       map[string][]domain.Version{},
		reclaimed:    map[string]bool{},
		snapshots:    map[string]*snapshot{},
		readSnapshot: map[uint64]string{},
		readMap:      map[string]domain.ReadDecision{},
		writeMap:     map[string]domain.WriteDecision{},
		metrics:      map[string]time.Duration{},
	}
	for _, txn := range project.Txns {
		k.status[txn.ID] = domain.TxnActive
	}
	k.seedBaselineVersions()

	horizon := &snapshot{id: HorizonSnapshotID, horizon: true, committedSet: map[uint64]bool{}}
	k.snapshots[HorizonSnapshotID] = horizon

	for _, op := range project.Ops {
		if err := ctx.Err(); err != nil {
			return nil, domain.NewError(domain.EInterrupted, "kernel run interrupted").With("op_id", op.ID)
		}
		if err := k.applyOp(op); err != nil {
			return nil, err
		}
		horizon.committedSet = copySet(k.committed)
		horizon.cutoffSeq = k.cutoffSeq(k.committed)
	}

	k.finalizeConflicts()
	commits := k.finalizeCommits()

	return &Report{
		ProjectID: project.ProjectID,
		BatchID:   project.BatchID,
		Reads:     sortedReads(k.readMap),
		Writes:    sortedWrites(k.writeMap),
		Commits:   commits,
		GC:        append([]domain.GCDecision{}, k.gcList...),
		Metrics:   k.metrics,
		Reclaimed: k.reclaimed,
	}, nil
}

func (k *Kernel) seedBaselineVersions() {
	writeVersionIDs := map[string]bool{}
	for _, op := range k.project.Ops {
		if op.Kind == domain.OpWrite && op.VersionID != "" {
			writeVersionIDs[op.VersionID] = true
		}
	}
	for _, version := range k.project.Versions {
		if writeVersionIDs[version.ID] {
			continue
		}
		k.chains[version.ItemID] = append(k.chains[version.ItemID], version)
	}
	for item := range k.chains {
		chain := k.chains[item]
		sort.SliceStable(chain, func(i, j int) bool {
			if chain[i].ChainPos != chain[j].ChainPos {
				return chain[i].ChainPos < chain[j].ChainPos
			}
			return chain[i].ID < chain[j].ID
		})
		k.chains[item] = chain
	}
}

func (k *Kernel) applyOp(op domain.Op) error {
	start := time.Now()
	defer func() {
		if op.ItemID != "" {
			k.metrics[op.ItemID] += time.Since(start)
		}
	}()

	switch op.Kind {
	case domain.OpWrite:
		return k.applyWrite(op)
	case domain.OpRead:
		decision := k.evaluateRead(op)
		k.readMap[op.ID] = decision
	case domain.OpCommit:
		k.status[op.TxnID] = domain.TxnCommitted
		k.committed[op.TxnID] = true
		k.releaseTxnSnapshots(op.TxnID)
	case domain.OpRollback:
		k.status[op.TxnID] = domain.TxnAborted
		k.releaseTxnSnapshots(op.TxnID)
	case domain.OpGC:
		k.gcList = append(k.gcList, k.evaluateGC(op))
	}
	return nil
}

func (k *Kernel) applyWrite(op domain.Op) error {
	chain := k.chains[op.ItemID]
	if k.opts.ChainCap > 0 && len(chain)+1 > k.opts.ChainCap {
		return domain.NewError(domain.EBackpressureChainCap, "per-item version chain buffer cap exhausted").
			At("operations["+op.ID+"]").
			With("op_id", op.ID).With("item_id", op.ItemID).
			With("chain_len", len(chain)).With("chain_cap", k.opts.ChainCap)
	}
	gen := op.WriteGen
	if gen == 0 {
		gen = len(chain) + 1
	}
	versionID := op.VersionID
	if versionID == "" {
		versionID = fmtVersionID(op.ItemID, gen, op.Index)
	}
	digest := op.Digest
	if digest == "" {
		digest = "unresolved:" + versionID
	}

	var predecessorID string
	var predecessorDelete *uint64
	if len(chain) > 0 {
		predecessor := chain[len(chain)-1]
		predecessorID = predecessor.ID
		txnID := op.TxnID
		predecessor.DeleteTxn = &txnID
		predecessorDelete = &txnID
		chain[len(chain)-1] = predecessor
	}
	version := domain.Version{
		ID:        versionID,
		ItemID:    op.ItemID,
		CreateTxn: op.TxnID,
		Digest:    digest,
		WriteGen:  gen,
		ChainPos:  len(chain),
	}
	chain = append(chain, version)
	k.chains[op.ItemID] = chain
	k.writeMap[op.ID] = domain.WriteDecision{
		OpID:                 op.ID,
		OpIndex:              op.Index,
		TxnID:                op.TxnID,
		ItemID:               op.ItemID,
		NewVersionID:         versionID,
		PredecessorVersionID: predecessorID,
		PredecessorDeleteTxn: predecessorDelete,
		Applied:              true,
	}
	return nil
}

func (k *Kernel) cutoffSeq(set map[uint64]bool) int {
	maxSeq := 0
	for id := range set {
		if seq := k.commitSeq[id]; seq > maxSeq {
			maxSeq = seq
		}
	}
	return maxSeq
}

func (k *Kernel) ensureReadSnapshot(txnID uint64, iso domain.Isolation, opIndex int, committed map[uint64]bool) string {
	cutoff := k.cutoffSeq(committed)
	if iso == domain.ReadCommitted {
		id := snapshotReadID(opIndex)
		k.snapshots[id] = &snapshot{id: id, ownerTxn: txnID, createdOpIndex: opIndex, committedSet: copySet(committed), cutoffSeq: cutoff}
		k.snapshotOrder = append(k.snapshotOrder, id)
		return id
	}
	if id, ok := k.readSnapshot[txnID]; ok {
		return id
	}
	id := snapshotTxnID(txnID)
	k.snapshots[id] = &snapshot{id: id, ownerTxn: txnID, createdOpIndex: opIndex, committedSet: copySet(committed), cutoffSeq: cutoff}
	k.snapshotOrder = append(k.snapshotOrder, id)
	k.readSnapshot[txnID] = id
	return id
}

func (k *Kernel) releaseTxnSnapshots(txnID uint64) {
	kept := k.snapshotOrder[:0]
	for _, id := range k.snapshotOrder {
		if k.snapshots[id].ownerTxn == txnID {
			delete(k.snapshots, id)
			continue
		}
		kept = append(kept, id)
	}
	k.snapshotOrder = append([]string{}, kept...)
	delete(k.readSnapshot, txnID)
}

func (k *Kernel) evaluateRead(op domain.Op) domain.ReadDecision {
	txn, _ := k.project.TxnByID(op.TxnID)
	iso := txn.Isolation
	if iso == "" {
		iso = domain.ReadCommitted
	}
	snapshotID := k.ensureReadSnapshot(op.TxnID, iso, op.Index, k.committed)
	snap := k.snapshots[snapshotID]
	decision := domain.ReadDecision{
		OpID:            op.ID,
		OpIndex:         op.Index,
		TxnID:           op.TxnID,
		ItemID:          op.ItemID,
		SnapshotID:      snapshotID,
		Skipped:         []domain.SkippedVersion{},
		CommittedSet:    sortedSet(snap.committedSet),
		CutoffCommitSeq: snap.cutoffSeq,
	}
	chain := k.chains[op.ItemID]
	for i := len(chain) - 1; i >= 0; i-- {
		version := chain[i]
		if k.reclaimed[version.ID] {
			continue
		}
		skip := domain.SkippedVersion{VersionID: version.ID, ItemID: version.ItemID, CreateTxn: version.CreateTxn}
		switch k.status[version.CreateTxn] {
		case domain.TxnAborted:
			skip.Reason = domain.SkipRolledBack
			skip.Detail = "create transaction rolled back"
			decision.Skipped = append(decision.Skipped, skip)
			continue
		case domain.TxnActive:
			skip.Reason = domain.SkipUncommitted
			skip.Detail = "create transaction has not committed at read point"
			decision.Skipped = append(decision.Skipped, skip)
			continue
		}
		if !snap.committedSet[version.CreateTxn] {
			skip.Reason = domain.SkipTxnAfterSnapshot
			skip.Detail = "create transaction committed outside fixed snapshot"
			decision.Skipped = append(decision.Skipped, skip)
			continue
		}
		if version.DeleteTxn != nil && snap.committedSet[*version.DeleteTxn] {
			skip.Reason = domain.SkipDeleted
			skip.Detail = "delete transaction is included in snapshot"
			decision.Skipped = append(decision.Skipped, skip)
			continue
		}
		decision.Visible = true
		decision.VisibleVersionID = version.ID
		decision.Digest = version.Digest
		return decision
	}
	return decision
}

func (k *Kernel) evaluateGC(op domain.Op) domain.GCDecision {
	activeIDs := append([]string{}, k.snapshotOrder...)
	activeIDs = append(activeIDs, HorizonSnapshotID)
	oldest := HorizonSnapshotID
	if len(k.snapshotOrder) > 0 {
		oldest = k.snapshotOrder[0]
	}
	protectedSet := map[string]bool{}
	for _, id := range activeIDs {
		snap := k.snapshots[id]
		if snap == nil {
			continue
		}
		for itemID, chain := range k.chains {
			visible := visibleVersion(chain, snap.committedSet, k.reclaimed)
			if visible != "" {
				protectedSet[visible] = true
			}
			_ = itemID
		}
	}
	protected := sortedKeys(protectedSet)
	candidates := []string{}
	for _, chain := range k.chains {
		for _, version := range chain {
			if k.reclaimed[version.ID] || protectedSet[version.ID] {
				continue
			}
			switch k.status[version.CreateTxn] {
			case domain.TxnCommitted, domain.TxnAborted:
				candidates = append(candidates, version.ID)
			}
		}
	}
	sort.Strings(candidates)
	for _, id := range candidates {
		k.reclaimed[id] = true
	}
	stepID := op.GCStepID
	if stepID == "" {
		stepID = fmtGCStepID(op.Index)
	}
	return domain.GCDecision{
		StepID:              stepID,
		AfterOpIndex:        op.Index,
		OldestSnapshotID:    oldest,
		ActiveSnapshotIDs:   activeIDs,
		ProtectedVersions:   protected,
		ReclaimableVersions: append([]string{}, candidates...),
		ReclaimedVersions:   append([]string{}, candidates...),
		HorizonCommitSeq:    k.snapshots[HorizonSnapshotID].cutoffSeq,
	}
}

func visibleVersion(chain []domain.Version, set map[uint64]bool, reclaimed map[string]bool) string {
	for i := len(chain) - 1; i >= 0; i-- {
		version := chain[i]
		if reclaimed != nil && reclaimed[version.ID] {
			continue
		}
		if !set[version.CreateTxn] {
			continue
		}
		if version.DeleteTxn != nil && set[*version.DeleteTxn] {
			continue
		}
		return version.ID
	}
	return ""
}

func DeriveCommitOrder(project *domain.Project) map[uint64]int {
	txns := make([]domain.Txn, 0)
	for _, txn := range project.Txns {
		if txn.CommitMS != nil {
			txns = append(txns, txn)
		}
	}
	rule := project.Rules.CommitTieRule
	sort.SliceStable(txns, func(i, j int) bool {
		a, b := txns[i], txns[j]
		if *a.CommitMS != *b.CommitMS {
			return *a.CommitMS < *b.CommitMS
		}
		if rule == domain.TieStartThenTxnID {
			if a.StartMS != b.StartMS {
				return a.StartMS < b.StartMS
			}
		}
		return a.ID < b.ID
	})
	out := map[uint64]int{}
	for i, txn := range txns {
		out[txn.ID] = i + 1
	}
	return out
}

func copySet(in map[uint64]bool) map[uint64]bool {
	out := make(map[uint64]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func sortedSet(in map[uint64]bool) []uint64 {
	out := make([]uint64, 0, len(in))
	for id := range in {
		if in[id] {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedKeys(in map[string]bool) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		if in[key] {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func sortedReads(in map[string]domain.ReadDecision) []domain.ReadDecision {
	out := make([]domain.ReadDecision, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpIndex < out[j].OpIndex })
	return out
}

func sortedWrites(in map[string]domain.WriteDecision) []domain.WriteDecision {
	out := make([]domain.WriteDecision, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpIndex < out[j].OpIndex })
	return out
}

func snapshotReadID(opIndex int) string {
	return "SNAP-R-" + pad3(opIndex)
}

func snapshotTxnID(txnID uint64) string {
	return "SNAP-T-" + strconv.FormatUint(txnID, 10) + "-SS"
}

func fmtGCStepID(opIndex int) string {
	return "GC-OP-" + pad3(opIndex)
}

func fmtVersionID(item string, gen, opIndex int) string {
	return "V-" + item + "-" + pad3(gen) + "-OP" + pad3(opIndex)
}

func pad3(v int) string {
	return fmt.Sprintf("%03d", v)
}
