package morph

import (
	"fmt"
	"sync"
)

type StageSnapshot struct {
	PathID string `json:"path_id"`
	Stage  Stage  `json:"stage"`
	State  State  `json:"state"`
	Error  string `json:"error,omitempty"`
}

type SampleSnapshot struct {
	Index     int             `json:"index"`
	Time      float64         `json:"time"`
	Committed bool            `json:"committed"`
	Stages    []StageSnapshot `json:"stages"`
}

type TimelineSnapshot struct {
	TotalSamples     int              `json:"total_samples"`
	CommittedSamples int              `json:"committed_samples"`
	Samples          []SampleSnapshot `json:"samples"`
}

type Timeline struct {
	mu          sync.RWMutex
	times       []float64
	pathIDs     []string
	states      [][][3]State
	errors      map[string]string
	committed   []bool
	commitOrder []int
}

func NewTimeline(scene *Scene) *Timeline {
	tl := &Timeline{
		times:  append([]float64(nil), scene.SampleTimes...),
		errors: map[string]string{},
	}
	for _, p := range scene.Paths {
		tl.pathIDs = append(tl.pathIDs, p.ID)
	}
	tl.states = make([][][3]State, len(tl.times))
	for si := range tl.states {
		tl.states[si] = make([][3]State, len(tl.pathIDs))
		for pi := range tl.states[si] {
			tl.states[si][pi] = [3]State{StatePending, StatePending, StatePending}
		}
	}
	tl.committed = make([]bool, len(tl.times))
	return tl
}

func stageIndex(s Stage) int {
	switch s {
	case StageFlatten:
		return 0
	case StageCoverage:
		return 1
	case StageComposite:
		return 2
	default:
		return -1
	}
}

func pathIndex(ids []string, id string) int {
	for i, v := range ids {
		if v == id {
			return i
		}
	}
	return -1
}

func (tl *Timeline) stateKey(sample int, pathID string, stage Stage) string {
	return fmt.Sprintf("%d/%s/%s", sample, pathID, stage)
}

func (tl *Timeline) State(sample int, pathID string, stage Stage) State {
	tl.mu.RLock()
	defer tl.mu.RUnlock()
	pi := pathIndex(tl.pathIDs, pathID)
	si := stageIndex(stage)
	if sample < 0 || sample >= len(tl.states) || pi < 0 || si < 0 {
		return StateFailed
	}
	return tl.states[sample][pi][si]
}

func (tl *Timeline) Transition(sample int, pathID string, stage Stage, to State) error {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	pi := pathIndex(tl.pathIDs, pathID)
	si := stageIndex(stage)
	if sample < 0 || sample >= len(tl.states) || pi < 0 || si < 0 {
		e := withPath(withField(newError(CodeState, "invalid state coordinates"), "state"), pathID)
		e.Sample = sample
		return e
	}
	if tl.committed[sample] {
		e := withPath(withField(newError(CodeState, "sample is committed and frozen"), "state"), pathID)
		e.Sample = sample
		return e
	}
	from := tl.states[sample][pi][si]
	if !legalTransition(from, to) {
		e := withPath(withField(newError(CodeState, fmt.Sprintf("illegal transition %s -> %s", from, to)), "state"), pathID)
		e.Sample = sample
		return e
	}
	tl.states[sample][pi][si] = to
	if to == StateFailed {
		tl.errors[tl.stateKey(sample, pathID, stage)] = "stage failed"
	} else if to == StateProcessing {
		delete(tl.errors, tl.stateKey(sample, pathID, stage))
	}
	return nil
}

func legalTransition(from, to State) bool {
	switch from {
	case StatePending:
		return to == StateProcessing
	case StateProcessing:
		return to == StateCommitted || to == StateFailed
	case StateFailed:
		return to == StateProcessing
	case StateCommitted:
		return false
	default:
		return false
	}
}

// CommitSample verifies that every path has committed all three stages and
// freezes the sample permanently.
func (tl *Timeline) CommitSample(sample int) error {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if sample < 0 || sample >= len(tl.states) {
		e := withField(newError(CodeState, "invalid sample index"), "sample")
		e.Sample = sample
		return e
	}
	if tl.committed[sample] {
		e := withField(newError(CodeState, "sample already committed"), "sample")
		e.Sample = sample
		return e
	}
	for pi := range tl.pathIDs {
		for si := range []Stage{StageFlatten, StageCoverage, StageComposite} {
			if tl.states[sample][pi][si] != StateCommitted {
				e := withPath(withField(newError(CodeState, "cannot commit sample before all stages commit"), "state"), tl.pathIDs[pi])
				e.Sample = sample
				return e
			}
		}
	}
	tl.committed[sample] = true
	tl.commitOrder = append(tl.commitOrder, sample)
	return nil
}

func (tl *Timeline) CommittedSamples() int {
	tl.mu.RLock()
	defer tl.mu.RUnlock()
	n := 0
	for _, v := range tl.committed {
		if v {
			n++
		}
	}
	return n
}

func (tl *Timeline) TotalSamples() int { return len(tl.times) }

func (tl *Timeline) Progress() (int, int) {
	return tl.CommittedSamples(), tl.TotalSamples()
}

func (tl *Timeline) Snapshot() TimelineSnapshot {
	tl.mu.RLock()
	defer tl.mu.RUnlock()
	s := TimelineSnapshot{TotalSamples: len(tl.times), Samples: make([]SampleSnapshot, len(tl.times))}
	for i := range tl.committed {
		if tl.committed[i] {
			s.CommittedSamples++
		}
	}
	for si := range tl.times {
		ss := SampleSnapshot{Index: si, Time: tl.times[si], Committed: tl.committed[si]}
		for pi, id := range tl.pathIDs {
			for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
				stageIdx := stageIndex(stage)
				ss.Stages = append(ss.Stages, StageSnapshot{
					PathID: id, Stage: stage, State: tl.states[si][pi][stageIdx],
					Error: tl.errors[tl.stateKey(si, id, stage)],
				})
			}
		}
		s.Samples[si] = ss
	}
	return s
}

// AbortInFlight clears only transient processing markers from a cancelled or
// paused run. Committed stages and failed stages are left untouched so retry
// semantics and frozen frames remain intact.
func (tl *Timeline) AbortInFlight(fromSample int) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if fromSample < 0 {
		fromSample = 0
	}
	for si := fromSample; si < len(tl.states); si++ {
		if tl.committed[si] {
			continue
		}
		for pi := range tl.states[si] {
			for st := range tl.states[si][pi] {
				if tl.states[si][pi][st] == StateProcessing {
					tl.states[si][pi][st] = StatePending
					delete(tl.errors, tl.stateKey(si, tl.pathIDs[pi], []Stage{StageFlatten, StageCoverage, StageComposite}[st]))
				}
			}
		}
	}
}
