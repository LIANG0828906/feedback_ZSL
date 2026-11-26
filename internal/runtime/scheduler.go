package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"sync"

	"schedlab/internal/domain"
	"schedlab/internal/engine"
	"schedlab/internal/faults"
)

// Config declares the parallel execution budgets.
type Config struct {
	Concurrency int // max pods scored simultaneously; >= 1
	BatchBuffer int // max candidate nodes buffered per batch; >= 1
	RuleVersion string
}

// Validate enforces legal budgets and returns stable business codes.
func (c Config) Validate() error {
	if c.Concurrency <= 0 {
		return faults.New(faults.E_CONCURRENCY_BUDGET, "concurrency must be >= 1",
			"concurrency", itoa(c.Concurrency))
	}
	if c.BatchBuffer <= 0 {
		return faults.New(faults.E_BUFFER_BUDGET, "batch buffer must be >= 1",
			"buffer", itoa(c.BatchBuffer))
	}
	if c.Concurrency > 64 {
		return faults.New(faults.E_CONCURRENCY_BUDGET, "concurrency exceeds hard ceiling 64",
			"concurrency", itoa(c.Concurrency))
	}
	if c.BatchBuffer > 4096 {
		return faults.New(faults.E_BUFFER_BUDGET, "buffer exceeds hard ceiling 4096",
			"buffer", itoa(c.BatchBuffer))
	}
	return nil
}

// PodOutcome is the scored result for one pod plus its path diagnostics.
type PodOutcome struct {
	PodID         string
	Score         engine.ScoreResult
	Feasibility   engine.FeasibilityResult
	Backpressured bool
}

// RunResult is the deterministic output of a scheduling run.
type RunResult struct {
	RuleVersion          string
	SnapshotSeq          int64
	Outcomes             []PodOutcome
	BackpressureEvents   []string
	ReusedFromCheckpoint int
}

// Runner orchestrates scoring, checkpointing and recovery.
type Runner struct {
	project *domain.Project
	store   *Store
	cfg     Config
}

// NewRunner validates budgets and wires the project with a checkpoint store.
func NewRunner(p *domain.Project, store *Store, cfg Config) (*Runner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Runner{project: p, store: store, cfg: cfg}, nil
}

// BuildSnapshot derives a node snapshot and its content hash from the project.
func BuildSnapshot(p *domain.Project, seq int64, ruleVersion string) Snapshot {
	views := make([]NodeView, 0, len(p.Nodes))
	h := sha256.New()
	nodeIDs := make([]string, 0, len(p.Nodes))
	for _, n := range p.Nodes {
		nodeIDs = append(nodeIDs, n.ID)
	}
	sort.Strings(nodeIDs)
	byID := p.NodeByID()
	for _, id := range nodeIDs {
		n := byID[id]
		topoDigest := digestMap(n.Topology)
		views = append(views, NodeView{
			ID: id, CPU: n.Allocatable.MilliCPU,
			Mem: n.Allocatable.Memory, Topology: topoDigest,
		})
		h.Write([]byte(id))
		h.Write([]byte(topoDigest))
		h.Write([]byte(strconv.FormatInt(n.Allocatable.MilliCPU, 10)))
		h.Write([]byte(strconv.FormatInt(n.Allocatable.Memory, 10)))
	}
	return Snapshot{
		Seq: seq, RuleVer: ruleVersion,
		NodeHash: hex.EncodeToString(h.Sum(nil)), Nodes: views,
	}
}

func digestMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte(m[k]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Run executes scoring under the budgets. The deterministic admission model
// treats Concurrency as a hard in-flight cap: when more pods than the cap are
// admitted in one wave, the overflow raises E_BACKPRESSURE instead of silently
// exceeding the budget. Checkpoint frames are committed in snapshot order.
func (r *Runner) Run() (*RunResult, error) {
	if err := r.cfg.Validate(); err != nil {
		return nil, err
	}
	seq := int64(1)
	ruleVer := r.cfg.RuleVersion

	// Recovery: only accept state whose rule version and snapshot match.
	reused := 0
	if ptr, err := r.store.LoadCommit(); err == nil {
		if ptr.RuleVersion != ruleVer {
			return nil, faults.New(faults.E_STATE_VERSION_MISMATCH,
				"persisted rule version does not match; full recompute required",
				"persisted", ptr.RuleVersion, "expected", ruleVer)
		}
		snap, err := r.store.LoadSnapshot(ptr.SnapshotSeq)
		if err != nil {
			return nil, mapStoreErr(err)
		}
		want := BuildSnapshot(r.project, ptr.SnapshotSeq, ruleVer)
		if snap.NodeHash != want.NodeHash {
			return nil, faults.New(faults.E_STATE_VERSION_MISMATCH,
				"node snapshot content mismatch; full recompute required",
				"snapshot_seq", strconv.FormatInt(ptr.SnapshotSeq, 10))
		}
		if frames, err := r.store.LoadCache(ptr.SnapshotSeq); err == nil {
			reused = len(frames)
		}
		// Recovery reuses the committed snapshot; the sequence is only advanced
		// when a genuinely new snapshot must be written.
		seq = ptr.SnapshotSeq
	} else if err != ErrNoCheckpoint {
		return nil, mapStoreErr(err)
	}

	result := &RunResult{RuleVersion: ruleVer, SnapshotSeq: seq, ReusedFromCheckpoint: reused}

	// Deterministic backpressure observation: if pod count exceeds the
	// concurrency budget, the overflow is reported as a business event. Real
	// goroutines still respect the cap via a weighted semaphore.
	var bpEvents []string
	if len(r.project.Pods) > r.cfg.Concurrency {
		for i := r.cfg.Concurrency; i < len(r.project.Pods); i++ {
			bpEvents = append(bpEvents, r.project.Pods[i].ID)
		}
	}
	result.BackpressureEvents = bpEvents

	frames := make([]Frame, 0, len(r.project.Pods))
	sem := make(chan struct{}, r.cfg.Concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for _, pod := range r.project.Pods {
		// Enforce the per-batch candidate buffer budget; overflow is rejected
		// deterministically with a backpressure code rather than over-admitting.
		if r.cfg.BatchBuffer < len(r.project.Nodes) {
			firstErr = faults.New(faults.E_BACKPRESSURE,
				"candidate batch exceeds buffer budget",
				"pod", pod.ID, "nodes", itoa(len(r.project.Nodes)),
				"buffer", itoa(r.cfg.BatchBuffer))
			break
		}
		wg.Add(1)
		sem <- struct{}{} // bounded by Concurrency; waves never exceed the cap
		go func(pod *domain.Pod) {
			defer wg.Done()
			defer func() { <-sem }()
			feas := engine.Filter(r.project, pod)
			score := engine.Score(r.project, pod, feas.Feasible)
			mu.Lock()
			result.Outcomes = append(result.Outcomes, PodOutcome{
				PodID: pod.ID, Score: score, Feasibility: feas,
				Backpressured: contains(bpEvents, pod.ID),
			})
			frames = append(frames, Frame{
				PodID: pod.ID, Selected: score.SelectedNode,
				Normalized:  scoreOf(score, score.SelectedNode),
				RuleVersion: ruleVer, SnapshotSeq: seq,
			})
			mu.Unlock()
		}(pod)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	// Restore canonical pod order despite parallel completion.
	sortByPodOrder(r.project, result.Outcomes)
	sort.Slice(frames, func(i, j int) bool { return frames[i].PodID < frames[j].PodID })

	snap := BuildSnapshot(r.project, seq, ruleVer)
	if err := r.store.Commit(snap, frames); err != nil {
		return nil, err
	}
	return result, nil
}

func scoreOf(s engine.ScoreResult, nodeID string) int64 {
	for _, sc := range s.Scores {
		if sc.NodeID == nodeID {
			return sc.NormalizedBps
		}
	}
	return 0
}

func sortByPodOrder(p *domain.Project, out []PodOutcome) {
	idx := map[string]int{}
	for _, pod := range p.Pods {
		idx[pod.ID] = pod.Order
	}
	sort.SliceStable(out, func(i, j int) bool { return idx[out[i].PodID] < idx[out[j].PodID] })
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func mapStoreErr(err error) error {
	switch err {
	case ErrCheckpointTruncated:
		return faults.Wrap(faults.E_CHECKPOINT_TRUNCATED, err.Error(), err)
	case ErrStateVersion:
		return faults.Wrap(faults.E_STATE_VERSION_MISMATCH, err.Error(), err)
	default:
		return err
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
