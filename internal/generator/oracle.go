package generator

import (
	"fmt"
	"sort"

	"mvccworkbench/internal/domain"
)

const horizonSnapshotID = "SNAP-HORIZON"

func sortedUintSet(set map[uint64]bool) []uint64 {
	out := make([]uint64, 0, len(set))
	for id := range set {
		if set[id] {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func copyUintSet(in map[uint64]bool) map[uint64]bool {
	out := make(map[uint64]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (b *Builder) replayTruth() {
	status := map[uint64]domain.TxnStatus{}
	for _, txn := range b.project.Txns {
		status[txn.ID] = domain.TxnActive
	}
	committed := map[uint64]bool{}
	b.snapshots = map[string]*activeSnapshot{}
	b.snapshotOrder = []string{}
	b.readSnapshot = map[uint64]string{}

	horizon := &activeSnapshot{
		id:           horizonSnapshotID,
		horizon:      true,
		committedSet: map[uint64]bool{},
	}
	b.snapshots[horizonSnapshotID] = horizon

	for opIndex := 1; opIndex <= len(b.project.Ops); opIndex++ {
		op := b.project.Ops[opIndex-1]
		switch op.Kind {
		case domain.OpWrite:
			b.applyOracleWrite(op)
		case domain.OpCommit:
			status[op.TxnID] = domain.TxnCommitted
			committed[op.TxnID] = true
			b.releaseTxnSnapshots(op.TxnID)
		case domain.OpRollback:
			status[op.TxnID] = domain.TxnAborted
			b.releaseTxnSnapshots(op.TxnID)
		case domain.OpRead:
			txn, ok := b.project.TxnByID(op.TxnID)
			if !ok {
				continue
			}
			snapshotID := b.ensureReadSnapshot(txn, op.Index, copyUintSet(committed))
			snapshotCommitted := b.snapshots[snapshotID].committedSet
			decision := b.oracleRead(op, snapshotID, snapshotCommitted, status)
			b.truth.Reads[op.ID] = decision
		case domain.OpGC:
			decision := b.oracleGC(op, status)
			b.truth.GC = append(b.truth.GC, decision)
		}

		horizon.committedSet = copyUintSet(committed)
		horizon.cutoffSeq = b.cutoffSeq(committed)
	}
}

func (b *Builder) applyOracleWrite(op domain.Op) {
	chain := b.chains[op.ItemID]
	if len(chain) > 0 {
		chain[len(chain)-1].DeleteTxn = &op.TxnID
	}
	chain = append(chain, domain.Version{
		ID:        op.VersionID,
		ItemID:    op.ItemID,
		CreateTxn: op.TxnID,
		Digest:    op.Digest,
		WriteGen:  op.WriteGen,
		ChainPos:  len(chain),
	})
	b.chains[op.ItemID] = chain
}

func (b *Builder) cutoffSeq(set map[uint64]bool) int {
	maxSeq := 0
	for id := range set {
		txn, ok := b.project.TxnByID(id)
		if ok && txn.RelativeCommitSeq > maxSeq {
			maxSeq = txn.RelativeCommitSeq
		}
	}
	return maxSeq
}

func (b *Builder) ensureReadSnapshot(txn domain.Txn, opIndex int, committed map[uint64]bool) string {
	cutoff := b.cutoffSeq(committed)
	if txn.Isolation == domain.ReadCommitted {
		id := fmt.Sprintf("SNAP-R-%03d", opIndex)
		snap := &activeSnapshot{
			id:             id,
			ownerTxn:       txn.ID,
			createdOpIndex: opIndex,
			committedSet:   committed,
			cutoffSeq:      cutoff,
		}
		b.snapshots[id] = snap
		b.snapshotOrder = append(b.snapshotOrder, id)
		return id
	}
	if id, ok := b.readSnapshot[txn.ID]; ok {
		return id
	}
	id := fmt.Sprintf("SNAP-T-%d-SS", txn.ID)
	snap := &activeSnapshot{
		id:             id,
		ownerTxn:       txn.ID,
		createdOpIndex: opIndex,
		committedSet:   committed,
		cutoffSeq:      cutoff,
	}
	b.snapshots[id] = snap
	b.snapshotOrder = append(b.snapshotOrder, id)
	b.readSnapshot[txn.ID] = id
	return id
}

func (b *Builder) releaseTxnSnapshots(txnID uint64) {
	kept := b.snapshotOrder[:0]
	for _, id := range b.snapshotOrder {
		if b.snapshots[id].ownerTxn == txnID {
			delete(b.snapshots, id)
			continue
		}
		kept = append(kept, id)
	}
	b.snapshotOrder = append([]string{}, kept...)
	delete(b.readSnapshot, txnID)
}

func (b *Builder) oracleRead(op domain.Op, snapshotID string, committed map[uint64]bool, status map[uint64]domain.TxnStatus) domain.ReadDecision {
	decision := domain.ReadDecision{
		OpID:            op.ID,
		OpIndex:         op.Index,
		TxnID:           op.TxnID,
		ItemID:          op.ItemID,
		SnapshotID:      snapshotID,
		Skipped:         []domain.SkippedVersion{},
		CommittedSet:    sortedUintSet(committed),
		CutoffCommitSeq: b.cutoffSeq(committed),
	}
	chain := b.chains[op.ItemID]
	for i := len(chain) - 1; i >= 0; i-- {
		version := chain[i]
		if b.reclaimed[version.ID] {
			continue
		}
		st := status[version.CreateTxn]
		skip := domain.SkippedVersion{
			VersionID: version.ID,
			ItemID:    version.ItemID,
			CreateTxn: version.CreateTxn,
		}
		switch st {
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
		if !committed[version.CreateTxn] {
			skip.Reason = domain.SkipTxnAfterSnapshot
			skip.Detail = "create transaction committed outside fixed snapshot"
			decision.Skipped = append(decision.Skipped, skip)
			continue
		}
		if version.DeleteTxn != nil && committed[*version.DeleteTxn] {
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

func visibleForSnapshot(chain []domain.Version, set map[uint64]bool, reclaimed map[string]bool) string {
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

func (b *Builder) oracleGC(op domain.Op, status map[uint64]domain.TxnStatus) domain.GCDecision {
	activeIDs := append([]string{}, b.snapshotOrder...)
	activeIDs = append(activeIDs, horizonSnapshotID)
	oldest := horizonSnapshotID
	if len(b.snapshotOrder) > 0 {
		oldest = b.snapshotOrder[0]
	}
	protectedSet := map[string]bool{}
	for _, id := range activeIDs {
		snap := b.snapshots[id]
		if snap == nil {
			continue
		}
		for itemID, chain := range b.chains {
			visible := visibleForSnapshot(chain, snap.committedSet, b.reclaimed)
			if visible != "" {
				protectedSet[visible] = true
			}
			_ = itemID
		}
	}
	protected := make([]string, 0, len(protectedSet))
	for id := range protectedSet {
		protected = append(protected, id)
	}
	sort.Strings(protected)

	candidates := []string{}
	for _, chain := range b.chains {
		for _, version := range chain {
			if b.reclaimed[version.ID] || protectedSet[version.ID] {
				continue
			}
			switch status[version.CreateTxn] {
			case domain.TxnCommitted, domain.TxnAborted:
				candidates = append(candidates, version.ID)
			}
		}
	}
	sort.Strings(candidates)
	for _, id := range candidates {
		b.reclaimed[id] = true
	}
	horizon := b.snapshots[horizonSnapshotID]
	return domain.GCDecision{
		StepID:              op.GCStepID,
		AfterOpIndex:        op.Index,
		OldestSnapshotID:    oldest,
		ActiveSnapshotIDs:   activeIDs,
		ProtectedVersions:   protected,
		ReclaimableVersions: candidates,
		ReclaimedVersions:   append([]string{}, candidates...),
		HorizonCommitSeq:    horizon.cutoffSeq,
	}
}

func conflictKey(a, c uint64, item string) string {
	if a > c {
		a, c = c, a
	}
	return fmt.Sprintf("%s:%d:%d", item, a, c)
}

func (b *Builder) finalizeConflicts() {
	writes := b.truth.WriteList()
	for i := 0; i < len(writes); i++ {
		for j := i + 1; j < len(writes); j++ {
			x := writes[i]
			y := writes[j]
			if x.ItemID != y.ItemID || x.TxnID == y.TxnID {
				continue
			}
			first, second := x, y
			if first.OpIndex > second.OpIndex {
				first, second = second, first
			}
			if b.txnAtReplay(first.TxnID, second.OpIndex-1) != domain.TxnActive {
				continue
			}
			key := conflictKey(x.TxnID, y.TxnID, x.ItemID)
			if _, exists := b.conflictPairs[key]; exists {
				continue
			}
			winner, loser := b.resolveConflict(first.TxnID, second.TxnID)
			if winner == domain.NoTxn {
				continue
			}
			pair := [2]uint64{x.TxnID, y.TxnID}
			if pair[0] > pair[1] {
				pair[0], pair[1] = pair[1], pair[0]
			}
			conflict := &domain.ConflictDecision{
				Code:          "WRITE_CONFLICT_FIRST_COMMITTER_WINS",
				Rule:          string(domain.ConflictFirstCommitterWins),
				ItemID:        x.ItemID,
				WinnerTxnID:   winner,
				LoserTxnID:    loser,
				CompetingTxns: pair,
			}
			b.conflictPairs[key] = conflict
			for _, opID := range []string{x.OpID, y.OpID} {
				decision := b.truth.Writes[opID]
				decision.Conflict = conflict
				b.truth.Writes[opID] = decision
			}
		}
	}
}

func (b *Builder) resolveConflict(a, c uint64) (winner, loser uint64) {
	ta, oka := b.project.TxnByID(a)
	tc, okc := b.project.TxnByID(c)
	if !oka || !okc {
		return 0, 0
	}
	aCommitted := ta.CommitMS != nil
	cCommitted := tc.CommitMS != nil
	if aCommitted && !cCommitted {
		return a, c
	}
	if cCommitted && !aCommitted {
		return c, a
	}
	if aCommitted && cCommitted {
		if ta.RelativeCommitSeq < tc.RelativeCommitSeq {
			return a, c
		}
		return c, a
	}
	return 0, 0
}

func (b *Builder) finalizeCommitAspectsAndImpacts() {
	commits := make([]domain.CommitDecision, 0)
	for _, txn := range b.project.Txns {
		if txn.CommitMS == nil {
			continue
		}
		aspect := domain.VisibilityAspect{
			AtCommitSeq:    txn.RelativeCommitSeq,
			CommitMS:       *txn.CommitMS,
			CommittedOrder: []uint64{},
			VisibleHeads:   map[string]string{},
		}
		for _, other := range b.project.Txns {
			if other.CommitMS != nil && other.RelativeCommitSeq <= txn.RelativeCommitSeq {
				aspect.CommittedOrder = append(aspect.CommittedOrder, other.ID)
			}
		}
		sort.Slice(aspect.CommittedOrder, func(i, j int) bool {
			si := b.project.Txns[aspect.CommittedOrder[i]-1].RelativeCommitSeq
			sj := b.project.Txns[aspect.CommittedOrder[j]-1].RelativeCommitSeq
			return si < sj
		})
		set := map[uint64]bool{}
		for _, id := range aspect.CommittedOrder {
			set[id] = true
		}
		for _, item := range b.project.Items {
			if visible := visibleForSnapshot(b.project.VersionsForItem(item.ID), set, nil); visible != "" {
				aspect.VisibleHeads[item.ID] = visible
			}
		}
		affected := map[string]bool{}
		for _, read := range b.truth.ReadList() {
			if read.TxnID == txn.ID {
				affected[read.OpID] = true
				continue
			}
			if read.VisibleVersionID != "" {
				if version := findVersion(&b.project, read.VisibleVersionID); version != nil && version.CreateTxn == txn.ID {
					affected[read.OpID] = true
				}
			}
			for _, skipped := range read.Skipped {
				if skipped.CreateTxn == txn.ID {
					affected[read.OpID] = true
				}
			}
		}
		affectedList := make([]string, 0, len(affected))
		for id := range affected {
			affectedList = append(affectedList, id)
		}
		sort.Slice(affectedList, func(i, j int) bool {
			return b.opIndex(affectedList[i]) < b.opIndex(affectedList[j])
		})
		commits = append(commits, domain.CommitDecision{
			TxnID:           txn.ID,
			CommitSeq:       txn.RelativeCommitSeq,
			CommitMS:        *txn.CommitMS,
			TieRule:         string(b.project.Rules.CommitTieRule),
			SnapshotID:      fmt.Sprintf("SNAP-COMMIT-%03d", txn.RelativeCommitSeq),
			Aspect:          aspect,
			AffectedReadOps: affectedList,
		})
	}
	for _, decision := range commits {
		b.truth.Commits[decision.TxnID] = decision
	}
}

func (b *Builder) opIndex(opID string) int {
	for _, op := range b.project.Ops {
		if op.ID == opID {
			return op.Index
		}
	}
	return 0
}

func findVersion(project *domain.Project, id string) *domain.Version {
	for i := range project.Versions {
		if project.Versions[i].ID == id {
			return &project.Versions[i]
		}
	}
	return nil
}
