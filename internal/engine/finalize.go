package engine

import (
	"fmt"
	"sort"

	"mvccworkbench/internal/domain"
)

func (k *Kernel) finalizeConflicts() {
	writes := sortedWrites(k.writeMap)
	for i := 0; i < len(writes); i++ {
		for j := i + 1; j < len(writes); j++ {
			x, y := writes[i], writes[j]
			if x.ItemID != y.ItemID || x.TxnID == y.TxnID {
				continue
			}
			first, second := x, y
			if first.OpIndex > second.OpIndex {
				first, second = second, first
			}
			if k.statusBeforeOp(first.TxnID, second.OpIndex) != domain.TxnActive {
				continue
			}
			winner, loser := k.resolveConflict(first.TxnID, second.TxnID)
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
			for _, opID := range []string{x.OpID, y.OpID} {
				decision := k.writeMap[opID]
				decision.Conflict = conflict
				k.writeMap[opID] = decision
			}
		}
	}
}

func (k *Kernel) statusBeforeOp(txnID uint64, opIndex int) domain.TxnStatus {
	status := domain.TxnActive
	for _, op := range k.project.Ops {
		if op.Index >= opIndex {
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

func (k *Kernel) resolveConflict(a, b uint64) (winner, loser uint64) {
	ta, oka := k.project.TxnByID(a)
	tb, okb := k.project.TxnByID(b)
	if !oka || !okb {
		return 0, 0
	}
	if ta.CommitMS != nil && tb.CommitMS == nil {
		return a, b
	}
	if tb.CommitMS != nil && ta.CommitMS == nil {
		return b, a
	}
	if ta.CommitMS != nil && tb.CommitMS != nil {
		if k.commitSeq[a] < k.commitSeq[b] {
			return a, b
		}
		return b, a
	}
	return 0, 0
}

func (k *Kernel) finalizeCommits() []domain.CommitDecision {
	out := make([]domain.CommitDecision, 0)
	for _, txn := range k.project.Txns {
		if txn.CommitMS == nil {
			continue
		}
		seq := k.commitSeq[txn.ID]
		order := make([]uint64, 0)
		set := map[uint64]bool{}
		for otherID, otherSeq := range k.commitSeq {
			if otherSeq <= seq {
				order = append(order, otherID)
				set[otherID] = true
			}
		}
		sort.Slice(order, func(i, j int) bool { return k.commitSeq[order[i]] < k.commitSeq[order[j]] })

		heads := map[string]string{}
		for _, item := range k.project.Items {
			if visible := visibleVersion(k.project.VersionsForItem(item.ID), set, nil); visible != "" {
				heads[item.ID] = visible
			}
		}

		affectedSet := map[string]bool{}
		for _, read := range k.readMap {
			if read.TxnID == txn.ID {
				affectedSet[read.OpID] = true
				continue
			}
			if read.VisibleVersionID != "" {
				if version := findDomainVersion(k.project, read.VisibleVersionID); version != nil && version.CreateTxn == txn.ID {
					affectedSet[read.OpID] = true
				}
			}
			for _, skipped := range read.Skipped {
				if skipped.CreateTxn == txn.ID {
					affectedSet[read.OpID] = true
				}
			}
		}
		affected := make([]string, 0, len(affectedSet))
		for id := range affectedSet {
			affected = append(affected, id)
		}
		sort.Slice(affected, func(i, j int) bool { return k.opIndex(affected[i]) < k.opIndex(affected[j]) })

		out = append(out, domain.CommitDecision{
			TxnID:      txn.ID,
			CommitSeq:  seq,
			CommitMS:   *txn.CommitMS,
			TieRule:    string(k.project.Rules.CommitTieRule),
			SnapshotID: fmt.Sprintf("SNAP-COMMIT-%03d", seq),
			Aspect: domain.VisibilityAspect{
				AtCommitSeq:    seq,
				CommitMS:       *txn.CommitMS,
				CommittedOrder: order,
				VisibleHeads:   heads,
			},
			AffectedReadOps: affected,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CommitSeq < out[j].CommitSeq })
	return out
}

func (k *Kernel) opIndex(opID string) int {
	for _, op := range k.project.Ops {
		if op.ID == opID {
			return op.Index
		}
	}
	return 0
}

func findDomainVersion(project *domain.Project, id string) *domain.Version {
	for i := range project.Versions {
		if project.Versions[i].ID == id {
			return &project.Versions[i]
		}
	}
	return nil
}
