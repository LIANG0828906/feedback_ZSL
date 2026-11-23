package level

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

type GenFunc func(proj *Project, pc *PartitionConstraint, density int) (*PartitionResult, error)

// Session is the editing-session component. History is modeled with
// constraint snapshots as nodes (commits) and edit ops as edges.
type Session struct {
	Proj    *Project
	Ops     []EditOp
	GenFunc GenFunc

	pool  *Pool
	store *Store

	cur     *Project
	base    map[int]*PartitionResult
	parts   map[int]*PartitionResult
	conn    *ConnectivityResult
	commits []*Commit
	nextIdx int
	lastTS  int64
}

func NewSession(proj *Project, ops []EditOp, store *Store, pool *Pool) (*Session, error) {
	if err := ValidateProject(proj); err != nil {
		return nil, err
	}
	if err := ValidateOps(proj, ops); err != nil {
		return nil, err
	}
	if pool == nil {
		pool = NewPool(4, 8)
	}
	s := &Session{
		Proj: proj, Ops: ops, GenFunc: GeneratePartition,
		pool: pool, store: store,
	}
	if err := s.bootstrap(); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenSession resumes from durable commits when the persisted fingerprint
// chain and sub-seed version match the given project and ops; otherwise
// the whole history is invalidated and a fresh full generation starts.
// After a forced kill only committed ops are honored.
func OpenSession(proj *Project, ops []EditOp, store *Store, pool *Pool) (*Session, error) {
	if store == nil {
		return NewSession(proj, ops, nil, pool)
	}
	commits, err := store.Load()
	if err == nil && len(commits) > 0 {
		err = validateChain(proj, ops, commits)
	}
	if err != nil || len(commits) == 0 {
		if ierr := store.Invalidate(); ierr != nil {
			return nil, ierr
		}
		return NewSession(proj, ops, store, pool)
	}
	if pool == nil {
		pool = NewPool(4, 8)
	}
	last := commits[len(commits)-1]
	s := &Session{
		Proj: proj, Ops: ops, GenFunc: GeneratePartition,
		pool: pool, store: store,
		cur:     last.Constraints.Clone(),
		base:    CloneParts(last.Base),
		parts:   CloneParts(last.Partitions),
		conn:    last.Connectivity.Clone(),
		commits: commits,
		nextIdx: last.OpSeq,
		lastTS:  last.Timestamp,
	}
	return s, nil
}

func validateChain(proj *Project, ops []EditOp, commits []*Commit) error {
	if commits[0].OpSeq != 0 {
		return fmt.Errorf("history does not start at initial commit")
	}
	if commits[0].SubSeedVersion != proj.SubSeedVersion {
		return fmt.Errorf("sub-seed version mismatch at commit 0")
	}
	cur := proj.Clone()
	if commits[0].Fingerprint != Fingerprint(cur) {
		return fmt.Errorf("fingerprint mismatch at commit 0")
	}
	for i := 1; i < len(commits); i++ {
		if i > len(ops) {
			return fmt.Errorf("history longer than op sequence")
		}
		op := ops[i-1]
		if commits[i].OpSeq != op.Seq {
			return fmt.Errorf("commit %d op sequence mismatch", i)
		}
		if commits[i].SubSeedVersion != proj.SubSeedVersion {
			return fmt.Errorf("sub-seed version mismatch at commit %d", i)
		}
		if err := ApplyOpToConstraints(cur, op); err != nil {
			return err
		}
		if commits[i].Fingerprint != Fingerprint(cur) {
			return fmt.Errorf("fingerprint mismatch at commit %d", i)
		}
	}
	return nil
}

func (s *Session) bootstrap() error {
	s.cur = s.Proj.Clone()
	ids := make([]int, 0, len(s.cur.Partitions))
	for _, pc := range s.cur.Partitions {
		ids = append(ids, pc.ID)
	}
	sort.Ints(ids)
	base, err := s.genSet(ids, s.cur, s.Proj.PreciseDensity)
	if err != nil {
		return err
	}
	parts := CloneParts(base)
	conn, err := EnsureConnectivity(s.cur, parts)
	if err != nil {
		return err
	}
	s.base = base
	s.parts = parts
	s.conn = conn
	s.nextIdx = 0
	s.lastTS = 0
	commit := &Commit{
		OpSeq:          0,
		Fingerprint:    Fingerprint(s.cur),
		SubSeedVersion: s.cur.SubSeedVersion,
		Constraints:    s.cur.Clone(),
		Base:           CloneParts(base),
		Partitions:     CloneParts(parts),
		Connectivity:   conn.Clone(),
		Manifest:       s.buildManifest(nil, nil, "initial"),
		Timestamp:      0,
	}
	return s.appendCommit(commit)
}

func (s *Session) genSet(ids []int, constraints *Project, density int) (map[int]*PartitionResult, error) {
	jobs := make([]GenJob, 0, len(ids))
	for _, id := range ids {
		pc := constraints.PartitionByID(id)
		if pc == nil {
			return nil, paraErr("partition", id, "partition missing from constraints")
		}
		pcCopy := *pc
		projSnap := constraints
		jobs = append(jobs, GenJob{
			PartitionID: id,
			Fn: func() (*PartitionResult, error) {
				return s.GenFunc(projSnap, &pcCopy, density)
			},
		})
	}
	outcomes := s.pool.Run(jobs)
	out := make(map[int]*PartitionResult, len(ids))
	for _, oc := range outcomes {
		if oc.Err != nil {
			if oc.Result != nil {
				oc.Result.Failed = true
				oc.Result.Err = oc.Err.Error()
			}
			if e, ok := oc.Err.(*Error); ok {
				return nil, e
			}
			return nil, fmt.Errorf("partition %d failed: %w", oc.PartitionID, oc.Err)
		}
		out[oc.PartitionID] = oc.Result
	}
	return out, nil
}

// affectedPartitions returns the invalidated partition subtree for an op:
// always the target partition, plus orthogonal neighbors whenever the op
// can change connectivity-relevant state (budget, quota, sub-seed).
func affectedPartitions(proj *Project, op EditOp, forceNeighbors bool) ([]int, map[int]string) {
	reasons := map[int]string{op.Partition: "target"}
	if forceNeighbors || op.Kind != OpSetResourceCap {
		pc := proj.PartitionByID(op.Partition)
		idx := gridIndex(proj)
		for dir := 0; dir < 4; dir++ {
			nid, ok := idx[[2]int{pc.Row + dirDelta[dir][0], pc.Col + dirDelta[dir][1]}]
			if ok {
				if _, dup := reasons[nid]; !dup {
					reasons[nid] = "neighbor_connectivity"
				}
			}
		}
	}
	ids := make([]int, 0, len(reasons))
	for id := range reasons {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids, reasons
}

func (s *Session) HasNext() bool { return s.nextIdx < len(s.Ops) }

func (s *Session) NextOp() EditOp { return s.Ops[s.nextIdx] }

// ApplyNextOp applies the next op incrementally: only the invalidated
// partition subtree is regenerated and atomically swapped into the cache;
// unaffected partitions reuse the previous round's results. On any
// failure the session state is left untouched.
func (s *Session) ApplyNextOp() (*Commit, error) {
	if !s.HasNext() {
		return nil, fmt.Errorf("no more ops to apply")
	}
	op := s.Ops[s.nextIdx]
	if op.Timestamp <= s.lastTS {
		return nil, histErr(op.Seq, "op timestamp regression")
	}
	if s.cur.PartitionByID(op.Partition) == nil {
		return nil, paraErr("partition", op.Partition, "op targets missing partition")
	}
	newCur := s.cur.Clone()
	if err := ApplyOpToConstraints(newCur, op); err != nil {
		if e, ok := err.(*Error); ok && e.OpSeq < 0 {
			e.OpSeq = op.Seq
		}
		return nil, err
	}
	affected, reasons := affectedPartitions(s.cur, op, false)
	fresh, err := s.genSet(affected, newCur, s.Proj.PreciseDensity)
	if err != nil {
		return nil, err
	}
	newBase := CloneParts(s.base)
	for id, r := range fresh {
		newBase[id] = r
	}
	newParts := CloneParts(newBase)
	conn, err := EnsureConnectivity(newCur, newParts)
	if err != nil {
		return nil, err
	}
	s.cur = newCur
	s.base = newBase
	s.parts = newParts
	s.conn = conn
	s.lastTS = op.Timestamp
	s.nextIdx++
	commit := &Commit{
		OpSeq:          op.Seq,
		Fingerprint:    Fingerprint(newCur),
		SubSeedVersion: newCur.SubSeedVersion,
		Constraints:    newCur.Clone(),
		Base:           CloneParts(newBase),
		Partitions:     CloneParts(newParts),
		Connectivity:   conn.Clone(),
		Manifest:       s.buildManifest(affected, reasons, ""),
		Timestamp:      op.Timestamp,
	}
	if err := s.appendCommit(commit); err != nil {
		return nil, err
	}
	return commit, nil
}

func (s *Session) ApplyAll() error {
	for s.HasNext() {
		if _, err := s.ApplyNextOp(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) appendCommit(c *Commit) error {
	if s.store != nil {
		if err := s.store.Append(c); err != nil {
			return err
		}
	}
	s.commits = append(s.commits, c)
	return nil
}

func (s *Session) buildManifest(affected []int, reasons map[int]string, initReason string) *Manifest {
	m := &Manifest{
		GlobalResourceTotal: GlobalResourceTotal(s.parts),
		Connected:           s.conn.Connected,
	}
	in := map[int]bool{}
	for _, id := range affected {
		in[id] = true
	}
	for _, id := range SortedIDs(s.parts) {
		entry := ManifestEntry{Partition: id, Hash: s.parts[id].Hash()}
		if initReason != "" {
			entry.Reason = initReason
		} else if in[id] {
			entry.Reason = reasons[id]
		} else {
			entry.Reason = "reused"
			entry.Reused = true
		}
		m.Entries = append(m.Entries, entry)
	}
	return m
}

// Rollback restores the committed snapshot with the given op sequence and
// drops all later commits, so subsequent ops can be replayed from it.
func (s *Session) Rollback(opSeq int) error {
	idx := -1
	for i, c := range s.commits {
		if c.OpSeq == opSeq {
			idx = i
			break
		}
	}
	if idx < 0 {
		return histErr(opSeq, "no committed snapshot for op sequence")
	}
	c := s.commits[idx]
	s.cur = c.Constraints.Clone()
	s.base = CloneParts(c.Base)
	s.parts = CloneParts(c.Partitions)
	s.conn = c.Connectivity.Clone()
	s.commits = s.commits[:idx+1]
	s.nextIdx = c.OpSeq
	s.lastTS = c.Timestamp
	if s.store != nil {
		if err := s.store.Truncate(opSeq); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) Commits() []*Commit { return s.commits }

func (s *Session) Parts() map[int]*PartitionResult { return s.parts }

func (s *Session) Connectivity() *ConnectivityResult { return s.conn }

// PathState captures everything the three generation paths must agree on.
type PathState struct {
	Parts         map[int]*PartitionResult
	Conn          *ConnectivityResult
	Fingerprints  []string
	ResourceTotal int
}

func (s *Session) Capture() *PathState {
	fps := make([]string, 0, len(s.commits))
	for _, c := range s.commits {
		fps = append(fps, c.Fingerprint)
	}
	return &PathState{
		Parts:         CloneParts(s.parts),
		Conn:          s.conn.Clone(),
		Fingerprints:  fps,
		ResourceTotal: GlobalResourceTotal(s.parts),
	}
}

// FullRegen is the one-shot path: starting from the initial constraints,
// apply the same op sequence and regenerate every partition after each
// op, recording the commit fingerprint sequence.
func FullRegen(proj *Project, ops []EditOp, pool *Pool) (*PathState, error) {
	if pool == nil {
		pool = NewPool(4, 8)
	}
	s, err := NewSession(proj, ops, nil, pool)
	if err != nil {
		return nil, err
	}
	cur := proj.Clone()
	fps := []string{Fingerprint(cur)}
	var parts map[int]*PartitionResult
	var conn *ConnectivityResult
	gen := func() error {
		ids := make([]int, 0, len(cur.Partitions))
		for _, pc := range cur.Partitions {
			ids = append(ids, pc.ID)
		}
		sort.Ints(ids)
		base, err := s.genSet(ids, cur, proj.PreciseDensity)
		if err != nil {
			return err
		}
		parts = CloneParts(base)
		conn, err = EnsureConnectivity(cur, parts)
		return err
	}
	if err := gen(); err != nil {
		return nil, err
	}
	for _, op := range ops {
		if err := ApplyOpToConstraints(cur, op); err != nil {
			return nil, err
		}
		if err := gen(); err != nil {
			return nil, err
		}
		fps = append(fps, Fingerprint(cur))
	}
	return &PathState{
		Parts:         parts,
		Conn:          conn,
		Fingerprints:  fps,
		ResourceTotal: GlobalResourceTotal(parts),
	}, nil
}

// ComparePaths locates the first divergence between two path states and
// reports it as E_DIFF with op sequence, partition id and item name.
func ComparePaths(a, b *PathState) error {
	if len(a.Fingerprints) != len(b.Fingerprints) {
		return diffErr(minInt(len(a.Fingerprints), len(b.Fingerprints)), -1, "commit_sequence_length")
	}
	for i := range a.Fingerprints {
		if a.Fingerprints[i] != b.Fingerprints[i] {
			return diffErr(i, -1, "commit_fingerprint")
		}
	}
	if a.ResourceTotal != b.ResourceTotal {
		return diffErr(-1, -1, "global_resource_total")
	}
	if !bytes.Equal(a.Conn.Canonical(), b.Conn.Canonical()) {
		return diffErr(-1, -1, "connectivity")
	}
	idsA, idsB := SortedIDs(a.Parts), SortedIDs(b.Parts)
	if len(idsA) != len(idsB) {
		return diffErr(-1, -1, "partition_set")
	}
	for i := range idsA {
		if idsA[i] != idsB[i] {
			return diffErr(-1, idsA[i], "partition_set")
		}
		ra, rb := a.Parts[idsA[i]], b.Parts[idsB[i]]
		if !bytes.Equal(ra.Canonical(), rb.Canonical()) {
			return diffErr(-1, idsA[i], firstDiffItem(ra, rb))
		}
	}
	return nil
}

func firstDiffItem(a, b *PartitionResult) string {
	if a.Difficulty != b.Difficulty {
		return "difficulty"
	}
	if a.PassablePct != b.PassablePct {
		return "passable_pct"
	}
	if a.Edges != b.Edges {
		return "edges"
	}
	if !jsonEqual(a.Obstacles, b.Obstacles) {
		return "obstacles"
	}
	if !jsonEqual(a.Resources, b.Resources) {
		return "resources"
	}
	if !jsonEqual(a.Heights, b.Heights) {
		return "heights"
	}
	if !jsonEqual(a.Tradeoffs, b.Tradeoffs) {
		return "tradeoffs"
	}
	return "unknown"
}

func jsonEqual(a, b any) bool {
	da, _ := json.Marshal(a)
	db, _ := json.Marshal(b)
	return bytes.Equal(da, db)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// CompareThreePaths verifies byte-identical agreement between the
// continuous incremental path (a), the rollback-and-replay path (b) and
// the one-shot full regeneration path (c) for the same op sequence.
func CompareThreePaths(sess *Session, rollbackSeq int) error {
	a := sess.Capture()

	replay, err := NewSession(sess.Proj, sess.Ops, nil, sess.pool)
	if err != nil {
		return err
	}
	if err := replay.ApplyAll(); err != nil {
		return err
	}
	if err := replay.Rollback(rollbackSeq); err != nil {
		return err
	}
	if err := replay.ApplyAll(); err != nil {
		return err
	}
	if err := ComparePaths(a, replay.Capture()); err != nil {
		return err
	}

	full, err := FullRegen(sess.Proj, sess.Ops, sess.pool)
	if err != nil {
		return err
	}
	return ComparePaths(a, full)
}
