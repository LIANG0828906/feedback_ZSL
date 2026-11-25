package engine

import (
	"context"
	"sort"
	"sync"
	"time"

	"mvccworkbench/internal/domain"
)

type shardResult struct {
	itemID  string
	reads   []domain.ReadDecision
	writes  []domain.WriteDecision
	chain   []domain.Version
	metrics time.Duration
	err     error
}

// RunParallel evaluates independent data-item shards with a bounded worker
// budget. Decisions are merged in declared item/op order, so scheduling cannot
// alter correctness. Per-item elapsed time is diagnostic only.
func RunParallel(ctx context.Context, project *domain.Project, opts Options) (*Report, error) {
	if opts.Workers <= 0 {
		opts.Workers = DefaultOptions().Workers
	}
	if opts.ItemBufferLimit <= 0 {
		opts.ItemBufferLimit = DefaultOptions().ItemBufferLimit
	}
	if opts.ChainCap <= 0 {
		opts.ChainCap = DefaultOptions().ChainCap
	}
	commitSeq := DeriveCommitOrder(project)
	sem := make(chan struct{}, opts.Workers)
	results := make([]shardResult, len(project.Items))
	var wg sync.WaitGroup

	for i, item := range project.Items {
		if err := ctx.Err(); err != nil {
			return nil, domain.NewError(domain.EInterrupted, "parallel run canceled before shard start").With("item_id", item.ID)
		}
		wg.Add(1)
		go func(i int, item domain.Item) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = shardResult{itemID: item.ID, err: ctx.Err()}
				return
			}
			results[i] = runShard(ctx, project, item.ID, opts, commitSeq)
		}(i, item)
	}
	wg.Wait()

	for _, result := range results {
		if result.err != nil {
			if de, ok := domain.AsError(result.err); ok {
				return nil, de
			}
			return nil, result.err
		}
	}

	k := &Kernel{
		project:       project,
		opts:          opts,
		status:        map[uint64]domain.TxnStatus{},
		committed:     map[uint64]bool{},
		commitSeq:     commitSeq,
		chains:        map[string][]domain.Version{},
		reclaimed:     map[string]bool{},
		snapshots:     map[string]*snapshot{},
		snapshotOrder: []string{},
		readSnapshot:  map[uint64]string{},
		readMap:       map[string]domain.ReadDecision{},
		writeMap:      map[string]domain.WriteDecision{},
		metrics:       map[string]time.Duration{},
	}
	for _, txn := range project.Txns {
		k.status[txn.ID] = domain.TxnActive
	}
	for _, result := range results {
		k.chains[result.itemID] = result.chain
		k.metrics[result.itemID] = result.metrics
		for _, read := range result.reads {
			k.readMap[read.OpID] = read
		}
		for _, write := range result.writes {
			k.writeMap[write.OpID] = write
		}
	}

	gc, err := evaluateParallelGC(ctx, k)
	if err != nil {
		return nil, err
	}
	k.gcList = gc
	k.finalizeConflicts()
	commits := k.finalizeCommits()
	return &Report{
		ProjectID: project.ProjectID,
		BatchID:   project.BatchID,
		Reads:     sortedReads(k.readMap),
		Writes:    sortedWrites(k.writeMap),
		Commits:   commits,
		GC:        gc,
		Metrics:   k.metrics,
		Reclaimed: k.reclaimed,
	}, nil
}

func runShard(ctx context.Context, project *domain.Project, itemID string, opts Options, commitSeq map[uint64]int) shardResult {
	result := shardResult{itemID: itemID, reads: []domain.ReadDecision{}, writes: []domain.WriteDecision{}}
	chain := []domain.Version{}
	firstRead := map[uint64]int{}
	for _, op := range project.Ops {
		if op.Kind == domain.OpRead && op.TxnID != domain.NoTxn {
			if _, exists := firstRead[op.TxnID]; !exists || op.Index < firstRead[op.TxnID] {
				firstRead[op.TxnID] = op.Index
			}
		}
	}

	for _, op := range project.Ops {
		if op.ItemID != itemID || (op.Kind != domain.OpRead && op.Kind != domain.OpWrite) {
			continue
		}
		if err := ctx.Err(); err != nil {
			result.err = domain.NewError(domain.EInterrupted, "item shard canceled").With("item_id", itemID).With("op_id", op.ID)
			return result
		}
		start := time.Now()
		switch op.Kind {
		case domain.OpWrite:
			if opts.ChainCap > 0 && len(chain)+1 > opts.ChainCap {
				result.err = domain.NewError(domain.EBackpressureChainCap, "per-item version chain buffer cap exhausted").
					With("item_id", itemID).With("op_id", op.ID).
					With("chain_len", len(chain)).With("chain_cap", opts.ChainCap)
				return result
			}
			var predecessorID string
			var predecessorDelete *uint64
			if len(chain) > 0 {
				predecessorID = chain[len(chain)-1].ID
				txnID := op.TxnID
				chain[len(chain)-1].DeleteTxn = &txnID
				predecessorDelete = &txnID
			}
			version := domain.Version{
				ID:        op.VersionID,
				ItemID:    itemID,
				CreateTxn: op.TxnID,
				Digest:    op.Digest,
				WriteGen:  op.WriteGen,
				ChainPos:  len(chain),
			}
			chain = append(chain, version)
			write := domain.WriteDecision{
				OpID:                 op.ID,
				OpIndex:              op.Index,
				TxnID:                op.TxnID,
				ItemID:               itemID,
				NewVersionID:         op.VersionID,
				PredecessorVersionID: predecessorID,
				PredecessorDeleteTxn: predecessorDelete,
				Applied:              true,
			}
			result.writes = append(result.writes, write)
		case domain.OpRead:
			txn, _ := project.TxnByID(op.TxnID)
			iso := txn.Isolation
			snapshotIndex := op.Index
			snapshotID := snapshotReadID(op.Index)
			if iso != domain.ReadCommitted {
				snapshotID = snapshotTxnID(op.TxnID)
				if first, ok := firstRead[op.TxnID]; ok {
					snapshotIndex = first
				}
			}
			committed := committedSetAt(project, snapshotIndex)
			status := statusAt(project, op.Index)
			cutoff := 0
			for txnID := range committed {
				if seq := commitSeq[txnID]; seq > cutoff {
					cutoff = seq
				}
			}
			decision := evaluateShardRead(op, snapshotID, chain, committed, status, cutoff)
			result.reads = append(result.reads, decision)
		}
		result.metrics += time.Since(start)
	}
	result.chain = chain
	return result
}

func committedSetAt(project *domain.Project, beforeOpIndex int) map[uint64]bool {
	set := map[uint64]bool{}
	for _, op := range project.Ops {
		if op.Index >= beforeOpIndex {
			break
		}
		if op.Kind == domain.OpCommit {
			set[op.TxnID] = true
		}
	}
	return set
}

func statusAt(project *domain.Project, opIndex int) map[uint64]domain.TxnStatus {
	status := map[uint64]domain.TxnStatus{}
	for _, txn := range project.Txns {
		status[txn.ID] = domain.TxnActive
	}
	for _, op := range project.Ops {
		if op.Index > opIndex {
			break
		}
		if op.Kind == domain.OpCommit {
			status[op.TxnID] = domain.TxnCommitted
		}
		if op.Kind == domain.OpRollback {
			status[op.TxnID] = domain.TxnAborted
		}
	}
	return status
}

func evaluateShardRead(op domain.Op, snapshotID string, chain []domain.Version, committed map[uint64]bool, status map[uint64]domain.TxnStatus, cutoff int) domain.ReadDecision {
	decision := domain.ReadDecision{
		OpID:            op.ID,
		OpIndex:         op.Index,
		TxnID:           op.TxnID,
		ItemID:          op.ItemID,
		SnapshotID:      snapshotID,
		Skipped:         []domain.SkippedVersion{},
		CommittedSet:    sortedSet(committed),
		CutoffCommitSeq: cutoff,
	}
	for i := len(chain) - 1; i >= 0; i-- {
		version := chain[i]
		skip := domain.SkippedVersion{VersionID: version.ID, ItemID: version.ItemID, CreateTxn: version.CreateTxn}
		switch status[version.CreateTxn] {
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

func evaluateParallelGC(ctx context.Context, k *Kernel) ([]domain.GCDecision, error) {
	readIDs := make([]string, 0, len(k.readMap))
	for id := range k.readMap {
		readIDs = append(readIDs, id)
	}
	sort.Slice(readIDs, func(i, j int) bool { return k.readMap[readIDs[i]].OpIndex < k.readMap[readIDs[j]].OpIndex })
	for _, id := range readIDs {
		read := k.readMap[id]
		if _, exists := k.snapshots[read.SnapshotID]; exists {
			continue
		}
		set := map[uint64]bool{}
		for _, txnID := range read.CommittedSet {
			set[txnID] = true
		}
		k.snapshots[read.SnapshotID] = &snapshot{
			id:             read.SnapshotID,
			ownerTxn:       read.TxnID,
			createdOpIndex: read.OpIndex,
			committedSet:   set,
			cutoffSeq:      read.CutoffCommitSeq,
		}
		k.snapshotOrder = append(k.snapshotOrder, read.SnapshotID)
	}
	sort.SliceStable(k.snapshotOrder, func(i, j int) bool {
		return k.snapshots[k.snapshotOrder[i]].createdOpIndex < k.snapshots[k.snapshotOrder[j]].createdOpIndex
	})
	horizon := &snapshot{id: HorizonSnapshotID, horizon: true, committedSet: map[uint64]bool{}}
	k.snapshots[HorizonSnapshotID] = horizon

	gc := []domain.GCDecision{}
	for _, op := range k.project.Ops {
		if err := ctx.Err(); err != nil {
			return nil, domain.NewError(domain.EInterrupted, "gc pass canceled").With("op_id", op.ID)
		}
		switch op.Kind {
		case domain.OpCommit:
			k.status[op.TxnID] = domain.TxnCommitted
			k.committed[op.TxnID] = true
			k.releaseTxnSnapshots(op.TxnID)
		case domain.OpRollback:
			k.status[op.TxnID] = domain.TxnAborted
			k.releaseTxnSnapshots(op.TxnID)
		case domain.OpGC:
			horizon.committedSet = copySet(k.committed)
			horizon.cutoffSeq = k.cutoffSeq(k.committed)
			gc = append(gc, k.evaluateGC(op))
		}
	}
	return gc, nil
}

// Scheduler exposes deterministic admission errors for diagnostics and tests.
type Scheduler struct {
	workers         int
	itemBufferLimit int
	slots           chan struct{}
	mu              sync.Mutex
	buffers         map[string]chan func(context.Context)
}

func NewScheduler(workers, itemBufferLimit int) *Scheduler {
	if workers <= 0 {
		workers = 1
	}
	if itemBufferLimit <= 0 {
		itemBufferLimit = 1
	}
	return &Scheduler{
		workers:         workers,
		itemBufferLimit: itemBufferLimit,
		slots:           make(chan struct{}, workers),
		buffers:         map[string]chan func(context.Context){},
	}
}

func (s *Scheduler) TryAcquireWorker() (func(), error) {
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, nil
	default:
		return nil, domain.NewError(domain.EBackpressureWorkers, "declared worker budget is exhausted").
			With("workers", s.workers)
	}
}

func (s *Scheduler) Submit(itemID string, task func(context.Context)) error {
	s.mu.Lock()
	buffer, ok := s.buffers[itemID]
	if !ok {
		buffer = make(chan func(context.Context), s.itemBufferLimit)
		s.buffers[itemID] = buffer
	}
	s.mu.Unlock()
	select {
	case buffer <- task:
		return nil
	default:
		return domain.NewError(domain.EBackpressureItemBuf, "per-item operation buffer is full").
			With("item_id", itemID).With("buffer_limit", s.itemBufferLimit).With("pending", len(buffer))
	}
}

func (s *Scheduler) Pending(itemID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buffers[itemID])
}
