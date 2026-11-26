package runtime

import (
	"schedlab/internal/domain"
	"schedlab/internal/engine"
	"schedlab/internal/faults"
)

// DeterministicRunner models admission without goroutines. A fixed-size
// semaphore tracks in-flight pods; Acquire fails with E_BACKPRESSURE when the
// concurrency budget is exhausted, so the backpressure code is fully
// deterministic and replayable across machines.
type DeterministicRunner struct {
	cfg       Config
	project   *domain.Project
	inflight  int
	completed []Frame
}

// NewDeterministicRunner validates budgets and creates the runner.
func NewDeterministicRunner(p *domain.Project, cfg Config) (*DeterministicRunner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &DeterministicRunner{cfg: cfg, project: p}, nil
}

// InFlight returns the current number of admitted but unfinished pods.
func (d *DeterministicRunner) InFlight() int { return d.inflight }

// Acquire admits one pod against the concurrency budget. Exhaustion returns
// E_BACKPRESSURE rather than blocking or exceeding the cap.
func (d *DeterministicRunner) Acquire(podID string) error {
	if d.inflight >= d.cfg.Concurrency {
		return faults.New(faults.E_BACKPRESSURE, "concurrency budget exhausted",
			"pod", podID, "in_flight", itoa(d.inflight),
			"budget", itoa(d.cfg.Concurrency))
	}
	if d.cfg.BatchBuffer < len(d.project.Nodes) {
		return faults.New(faults.E_BACKPRESSURE, "buffer budget exhausted",
			"pod", podID, "nodes", itoa(len(d.project.Nodes)),
			"buffer", itoa(d.cfg.BatchBuffer))
	}
	d.inflight++
	return nil
}

// Release marks one admitted pod as finished and records its scored frame.
func (d *DeterministicRunner) Release(pod *domain.Pod, seq int64) {
	if d.inflight > 0 {
		d.inflight--
	}
	feas := engine.Filter(d.project, pod)
	score := engine.Score(d.project, pod, feas.Feasible)
	d.completed = append(d.completed, Frame{
		PodID: pod.ID, Selected: score.SelectedNode,
		Normalized:  scoreOf(score, score.SelectedNode),
		RuleVersion: d.cfg.RuleVersion, SnapshotSeq: seq,
	})
}

// Frames returns all completed frames in submission order.
func (d *DeterministicRunner) Frames() []Frame { return d.completed }
