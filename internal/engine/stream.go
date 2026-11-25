package engine

import (
	"context"
	"sort"
	"time"

	"mvccworkbench/internal/checkpoint"
	"mvccworkbench/internal/domain"
)

type StreamRunner struct {
	k       *Kernel
	pos     int
	lastSeq int
}

func NewStreamRunner(project *domain.Project, opts Options) *StreamRunner {
	if opts.Workers == 0 {
		opts = DefaultOptions()
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
	k.snapshots[HorizonSnapshotID] = &snapshot{id: HorizonSnapshotID, horizon: true, committedSet: map[uint64]bool{}}
	return &StreamRunner{k: k}
}

// Step applies exactly the next declared operation. It is the incremental
// entry point used by checkpoints and signal-safe CLI execution.
func (r *StreamRunner) Step(ctx context.Context) (domain.Op, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Op{}, false, domain.NewError(domain.EInterrupted, "step canceled before operation boundary").With("committed_op_seq", r.lastSeq)
	}
	if r.pos >= len(r.k.project.Ops) {
		return domain.Op{}, true, nil
	}
	op := r.k.project.Ops[r.pos]
	if err := r.k.applyOp(op); err != nil {
		return op, false, err
	}
	horizon := r.k.snapshots[HorizonSnapshotID]
	r.refreshHorizon(horizon)
	r.pos++
	r.lastSeq = op.Index
	return op, false, nil
}

func (r *StreamRunner) refreshHorizon(horizon *snapshot) {
	if horizon == nil {
		return
	}
	horizon.committedSet = copySet(r.k.committed)
	horizon.cutoffSeq = r.k.cutoffSeq(r.k.committed)
}

func (r *StreamRunner) RunAll(ctx context.Context) error {
	for {
		_, done, err := r.Step(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func (r *StreamRunner) LastCommittedSeq() int { return r.lastSeq }

func (r *StreamRunner) ProcessedIndexes() []int {
	out := make([]int, 0, r.pos)
	for i := 0; i < r.pos; i++ {
		out = append(out, r.k.project.Ops[i].Index)
	}
	return out
}

func (r *StreamRunner) ExportState() checkpoint.State {
	snapshots := make([]checkpoint.SnapshotState, 0, len(r.k.snapshots))
	ids := append([]string{}, r.k.snapshotOrder...)
	ids = append(ids, HorizonSnapshotID)
	for _, id := range ids {
		snap := r.k.snapshots[id]
		if snap == nil {
			continue
		}
		snapshots = append(snapshots, checkpoint.SnapshotState{
			Version:        checkpoint.StateSnapshotVersion,
			ID:             snap.id,
			OwnerTxn:       snap.ownerTxn,
			CreatedOpIndex: snap.createdOpIndex,
			Horizon:        snap.horizon,
			CommittedSet:   sortedSet(snap.committedSet),
			CutoffSeq:      snap.cutoffSeq,
		})
	}
	chains := make(map[string][]domain.Version, len(r.k.chains))
	for itemID, chain := range r.k.chains {
		chains[itemID] = append([]domain.Version{}, chain...)
	}
	reclaimed := map[string]bool{}
	for id, value := range r.k.reclaimed {
		reclaimed[id] = value
	}
	return checkpoint.State{
		FormatVersion:  domain.CheckpointFormat,
		ProjectVersion: domain.ProjectModelVersion,
		SourceSchema:   r.k.project.SourceSchema,
		ProjectID:      r.k.project.ProjectID,
		BatchID:        r.k.project.BatchID,
		Seed:           r.k.project.Seed,
		CommittedOpSeq: r.lastSeq,
		Chains:         chains,
		Reclaimed:      reclaimed,
		Snapshots:      snapshots,
	}
}

func (r *StreamRunner) SaveCheckpoint(ctx context.Context, store *checkpoint.Store) error {
	return store.Save(ctx, r.ExportState())
}

func (r *StreamRunner) RestoreState(state *checkpoint.State) error {
	if state.ProjectVersion != domain.ProjectModelVersion {
		return domain.NewError(domain.ECkptVersionMismatch, "refusing to restore incompatible project version").
			With("got", state.ProjectVersion).With("expected", domain.ProjectModelVersion)
	}
	if state.ProjectID != r.k.project.ProjectID || state.BatchID != r.k.project.BatchID {
		return domain.NewError(domain.ECkptVersionMismatch, "checkpoint identity does not match project").
			With("checkpoint_project_id", state.ProjectID).With("project_id", r.k.project.ProjectID).
			With("checkpoint_batch_id", state.BatchID).With("batch_id", r.k.project.BatchID)
	}
	r.lastSeq = state.CommittedOpSeq
	r.k.chains = map[string][]domain.Version{}
	for itemID, chain := range state.Chains {
		r.k.chains[itemID] = append([]domain.Version{}, chain...)
	}
	r.k.reclaimed = map[string]bool{}
	for id, value := range state.Reclaimed {
		r.k.reclaimed[id] = value
	}
	r.k.snapshots = map[string]*snapshot{}
	r.k.snapshotOrder = []string{}
	r.k.readSnapshot = map[uint64]string{}
	for _, saved := range state.Snapshots {
		if saved.Version != checkpoint.StateSnapshotVersion {
			return domain.NewError(domain.ESnapshotVersionBad, "refusing to restore incompatible snapshot").
				With("snapshot_id", saved.ID).With("got", saved.Version)
		}
		set := map[uint64]bool{}
		for _, txnID := range saved.CommittedSet {
			set[txnID] = true
		}
		snap := &snapshot{
			id:             saved.ID,
			ownerTxn:       saved.OwnerTxn,
			createdOpIndex: saved.CreatedOpIndex,
			committedSet:   set,
			cutoffSeq:      saved.CutoffSeq,
			horizon:        saved.Horizon || saved.ID == HorizonSnapshotID,
		}
		r.k.snapshots[snap.id] = snap
		if !snap.horizon {
			r.k.snapshotOrder = append(r.k.snapshotOrder, snap.id)
			if _, exists := r.k.readSnapshot[snap.ownerTxn]; !exists && !isReadCommittedSnapshotID(snap.id) {
				r.k.readSnapshot[snap.ownerTxn] = snap.id
			}
		}
	}
	sort.SliceStable(r.k.snapshotOrder, func(i, j int) bool {
		a := r.k.snapshots[r.k.snapshotOrder[i]]
		b := r.k.snapshots[r.k.snapshotOrder[j]]
		if a.createdOpIndex != b.createdOpIndex {
			return a.createdOpIndex < b.createdOpIndex
		}
		return a.id < b.id
	})

	// Reconstruct only transaction terminal status from the declared stream.
	// Chain and snapshot state comes from the checkpoint; this avoids treating
	// an operation after CommittedOpSeq as visible.
	r.k.status = map[uint64]domain.TxnStatus{}
	r.k.committed = map[uint64]bool{}
	for _, txn := range r.k.project.Txns {
		r.k.status[txn.ID] = domain.TxnActive
	}
	for _, op := range r.k.project.Ops {
		if op.Index > state.CommittedOpSeq {
			break
		}
		if op.Kind == domain.OpCommit {
			r.k.status[op.TxnID] = domain.TxnCommitted
			r.k.committed[op.TxnID] = true
		}
		if op.Kind == domain.OpRollback {
			r.k.status[op.TxnID] = domain.TxnAborted
		}
	}

	r.pos = 0
	for r.pos < len(r.k.project.Ops) && r.k.project.Ops[r.pos].Index <= state.CommittedOpSeq {
		r.pos++
	}
	r.rebuildHistoricalWrites()
	return nil
}

func (r *StreamRunner) rebuildHistoricalWrites() {
	for _, op := range r.k.project.Ops {
		if op.Kind != domain.OpWrite || op.Index > r.lastSeq {
			continue
		}
		decision := domain.WriteDecision{
			OpID:         op.ID,
			OpIndex:      op.Index,
			TxnID:        op.TxnID,
			ItemID:       op.ItemID,
			NewVersionID: op.VersionID,
			Applied:      true,
		}
		chain := r.k.project.VersionsForItem(op.ItemID)
		for i, version := range chain {
			if version.ID == op.VersionID && i > 0 {
				predecessor := chain[i-1]
				decision.PredecessorVersionID = predecessor.ID
				decision.PredecessorDeleteTxn = predecessor.DeleteTxn
			}
		}
		r.k.writeMap[op.ID] = decision
	}
}

func (r *StreamRunner) Report() *Report {
	r.k.finalizeConflicts()
	commits := r.k.finalizeCommits()
	return &Report{
		ProjectID: r.k.project.ProjectID,
		BatchID:   r.k.project.BatchID,
		Reads:     sortedReads(r.k.readMap),
		Writes:    sortedWrites(r.k.writeMap),
		Commits:   commits,
		GC:        append([]domain.GCDecision{}, r.k.gcList...),
		Metrics:   r.k.metrics,
		Reclaimed: r.k.reclaimed,
	}
}

func isReadCommittedSnapshotID(id string) bool {
	return len(id) >= 7 && id[:6] == "SNAP-R"
}
