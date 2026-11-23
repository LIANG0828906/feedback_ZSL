package orchestrator

import (
	"fmt"
	"sync"
)

// DialogueResult is the persisted orchestration output of one dialogue.
type DialogueResult struct {
	DialogueID string           `json:"dialogue_id"`
	QueuePos   int              `json:"queue_pos"`
	Schedule   *Schedule        `json:"schedule"`
	Interrupt  *InterruptResult `json:"interrupt,omitempty"`
	State      State            `json:"state"`
}

// ManifestEntry describes, per dialogue, whether incremental
// recompute reused or recomputed the persisted result.
type ManifestEntry struct {
	DialogueID string `json:"dialogue_id"`
	Action     string `json:"action"` // "reused" or "recomputed"
	Reason     string `json:"reason"`
}

// Orchestrator drives per-dialogue orchestration in queue order.
type Orchestrator struct {
	Project       *Project
	Queue         *Queue
	States        map[string]*DialogueState
	Results       map[string]*DialogueResult
	commitOrder   []string
}

// NewOrchestrator validates the project and builds the queue.
func NewOrchestrator(p *Project) (*Orchestrator, *Error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	q, qerr := BuildQueue(p)
	if qerr != nil {
		return nil, qerr
	}
	o := &Orchestrator{
		Project: p,
		Queue:   q,
		States:  map[string]*DialogueState{},
		Results: map[string]*DialogueResult{},
	}
	for _, id := range q.Order() {
		o.States[id] = &DialogueState{DialogueID: id, State: StatePending}
	}
	return o, nil
}

// Progress returns submitted dialogue count and total count.
func (o *Orchestrator) Progress() (int, int) {
	done := 0
	for _, st := range o.States {
		if st.State == StateSubmitted {
			done++
		}
	}
	return done, o.Queue.Len()
}

// CommitOrder returns the stable commit sequence produced so far.
func (o *Orchestrator) CommitOrder() []string {
	out := make([]string, len(o.commitOrder))
	copy(out, o.commitOrder)
	return out
}

// OrchestrateOne runs the full pipeline for one dialogue: validate,
// schedule, optional scripted interrupt with cutoff and in-place
// resume, then submit. Failures mark the dialogue failed without
// affecting others.
func (o *Orchestrator) OrchestrateOne(id string) *Error {
	st, ok := o.States[id]
	if !ok {
		return newError(ERef, id, "id", "unknown dialogue")
	}
	if st.State == StateSubmitted {
		return newError(EState, id, "state", "dialogue already submitted and locked")
	}
	if err := st.Transition(StateOrchestrating); err != nil {
		return err
	}
	res, oerr := o.compute(id)
	if oerr != nil {
		if terr := st.Transition(StateFailed); terr != nil {
			return terr
		}
		return oerr
	}
	o.Results[id] = res
	if res.Interrupt != nil {
		// Drive the machine through the interrupt / resume arc.
		for _, next := range []State{StateInterrupted, StateResumePending, StateOrchestrating, StateSubmitted} {
			if terr := st.Transition(next); terr != nil {
				return terr
			}
		}
	} else if terr := st.Transition(StateSubmitted); terr != nil {
		return terr
	}
	o.commitOrder = append(o.commitOrder, id)
	return nil
}

// compute performs the pure orchestration math for one dialogue,
// driving the state machine through interrupt / resume when scripted.
func (o *Orchestrator) compute(id string) (*DialogueResult, *Error) {
	d := o.Project.DialogueByID(id)
	if d == nil {
		return nil, newError(ERef, id, "id", "unknown dialogue")
	}
	pos, _ := o.Queue.Position(id)
	sched, err := ScheduleDialogue(d)
	if err != nil {
		return nil, err
	}
	if cerr := sched.CheckContiguous(); cerr != nil {
		return nil, cerr
	}
	res := &DialogueResult{DialogueID: id, QueuePos: pos, Schedule: sched}

	if d.InterruptAt != nil && o.Project.Policy.Interruptible {
		// interrupted -> resume pending -> resume in original position
		pending := map[string]bool{id: true}
		if qerr := o.Queue.CheckResumePosition(id, pending); qerr != nil {
			return nil, qerr
		}
		ir, ierr := ComputeInterrupt(d, sched, *d.InterruptAt, o.Project.Policy.CutoffLimitSamples)
		if ierr != nil {
			return nil, ierr
		}
		if _, rerr := ResumeView(sched, ir); rerr != nil {
			return nil, rerr
		}
		res.Interrupt = ir
	}
	res.State = StateSubmitted
	return res, nil
}

// OrchestrateAll processes every dialogue in queue order. A failing
// dialogue is isolated: it is marked failed and the run continues.
// Returns the first error encountered, if any.
func (o *Orchestrator) OrchestrateAll() *Error {
	var first *Error
	for _, id := range o.Queue.Order() {
		if err := o.OrchestrateOne(id); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// EditDialogue applies an edit to one dialogue's rhythm weights or
// interruption strategy. Submitted dialogues are locked: editing them
// returns E_STATE and changes nothing.
func (o *Orchestrator) EditDialogue(id string, edit func(d *Dialogue)) *Error {
	st, ok := o.States[id]
	if !ok {
		return newError(ERef, id, "id", "unknown dialogue")
	}
	if st.State == StateSubmitted {
		return newError(EState, id, "state", "submitted dialogue is locked against edits")
	}
	d := o.Project.DialogueByID(id)
	edit(d)
	return nil
}

// RecomputeIncremental invalidates only the edited dialogue plus its
// resume-pending chain (every unsubmitted dialogue at or after its
// queue position), recomputes those, and reuses persisted results for
// the rest. It returns the per-dialogue manifest.
func (o *Orchestrator) RecomputeIncremental(editedID string) ([]ManifestEntry, *Error) {
	editedPos, ok := o.Queue.Position(editedID)
	if !ok {
		return nil, newError(ERef, editedID, "id", "unknown dialogue")
	}
	manifest := make([]ManifestEntry, 0, o.Queue.Len())
	for pos, id := range o.Queue.Order() {
		st := o.States[id]
		if st.State == StateSubmitted && pos < editedPos {
			manifest = append(manifest, ManifestEntry{DialogueID: id, Action: "reused",
				Reason: "submitted before edited position, locked"})
			continue
		}
		reason := fmt.Sprintf("invalidated: at or after edited dialogue %s", editedID)
		if id == editedID {
			reason = "invalidated: edited dialogue"
		} else if st.State == StateResumePending {
			reason = "invalidated: resume-pending chain of edited dialogue"
		}
		delete(o.Results, id)
		st.State = StatePending
		if err := o.OrchestrateOne(id); err != nil {
			manifest = append(manifest, ManifestEntry{DialogueID: id, Action: "recomputed",
				Reason: reason + " (failed: " + err.Code + ")"})
			continue
		}
		manifest = append(manifest, ManifestEntry{DialogueID: id, Action: "recomputed", Reason: reason})
	}
	return manifest, nil
}

// FullRecompute orchestrates the project from an empty cache.
func FullRecompute(p *Project) (map[string]*DialogueResult, []string, *Error) {
	o, err := NewOrchestrator(p)
	if err != nil {
		return nil, nil, err
	}
	runErr := o.OrchestrateAll()
	return o.Results, o.CommitOrder(), runErr
}

// CompareResults checks two result maps for per-character equality
// over the given order and returns E_DIFF locating the first mismatch.
func CompareResults(a, b map[string]*DialogueResult, order []string) *Error {
	for _, id := range order {
		ra, oka := a[id]
		rb, okb := b[id]
		if oka != okb {
			return newError(EDiff, id, "presence", "result present on one path only")
		}
		if !oka {
			continue
		}
		if derr := compareOne(id, ra, rb); derr != nil {
			return derr
		}
	}
	return nil
}

func compareOne(id string, a, b *DialogueResult) *Error {
	diff := func(field string) *Error {
		return newError(EDiff, id, field, "results differ between paths")
	}
	if a.Schedule.PresentStart != b.Schedule.PresentStart {
		return diff("present_start")
	}
	if a.Schedule.PresentEnd != b.Schedule.PresentEnd {
		return diff("present_end")
	}
	if len(a.Schedule.Chars) != len(b.Schedule.Chars) {
		return diff("chars")
	}
	for i := range a.Schedule.Chars {
		ca, cb := a.Schedule.Chars[i], b.Schedule.Chars[i]
		if ca.Start != cb.Start || ca.End != cb.End {
			e := diff("chars")
			e.Position = i
			return e
		}
	}
	if (a.Interrupt == nil) != (b.Interrupt == nil) {
		return diff("interrupt")
	}
	if a.Interrupt != nil {
		if a.Interrupt.CutoffEnd != b.Interrupt.CutoffEnd {
			return diff("cutoff_end")
		}
		if a.Interrupt.ResumeIndex != b.Interrupt.ResumeIndex {
			return diff("resume_index")
		}
		if a.Interrupt.ResumeStart != b.Interrupt.ResumeStart {
			return diff("resume_start")
		}
	}
	return nil
}

// ResourceLimits bound the worker pool: maximum parallelism and
// per-worker text / sample buffer caps.
type ResourceLimits struct {
	MaxWorkers            int
	MaxCharsPerDialogue   int
	MaxSamplesPerDialogue int64
}

func (l ResourceLimits) check(d *Dialogue) *Error {
	if l.MaxCharsPerDialogue > 0 && len(d.Chars()) > l.MaxCharsPerDialogue {
		e := newError(EPara, d.ID, "text_buffer", "dialogue text exceeds per-worker text buffer limit")
		e.Expected = int64(l.MaxCharsPerDialogue)
		e.Actual = int64(len(d.Chars()))
		return e
	}
	start, end := presentRange(d)
	if l.MaxSamplesPerDialogue > 0 && end-start > l.MaxSamplesPerDialogue {
		e := newError(EPara, d.ID, "sample_buffer", "dialogue exceeds per-worker sample buffer limit")
		e.Expected = l.MaxSamplesPerDialogue
		e.Actual = end - start
		return e
	}
	return nil
}

// OrchestrateParallel orchestrates with a bounded worker pool.
// Computation is parallel but commit order is the stable queue order,
// so results and commit sequence match the single-threaded run.
func OrchestrateParallel(p *Project, limits ResourceLimits) (map[string]*DialogueResult, []string, *Error) {
	o, err := NewOrchestrator(p)
	if err != nil {
		return nil, nil, err
	}
	order := o.Queue.Order()
	workers := limits.MaxWorkers
	if workers <= 0 {
		workers = 1
	}

	type job struct {
		id  string
		res *DialogueResult
		err *Error
	}
	jobs := make(chan string)
	done := make(chan job, len(order))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				d := o.Project.DialogueByID(id)
				if lerr := limits.check(d); lerr != nil {
					done <- job{id: id, err: lerr}
					continue
				}
				res, cerr := o.compute(id)
				done <- job{id: id, res: res, err: cerr}
			}
		}()
	}
	for _, id := range order {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	close(done)

	computed := map[string]job{}
	for j := range done {
		computed[j.id] = j
	}

	// Commit strictly in queue order, serialized.
	var first *Error
	for _, id := range order {
		j := computed[id]
		st := o.States[id]
		st.State = StateOrchestrating
		if j.err != nil {
			st.State = StateFailed
			if first == nil {
				first = j.err
			}
			continue
		}
		o.Results[id] = j.res
		st.State = j.res.State
		o.commitOrder = append(o.commitOrder, id)
	}
	return o.Results, o.CommitOrder(), first
}
