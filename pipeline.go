package bonerig

import (
	"context"
	"fmt"
	"sort"
)

// EngineHooks provides deterministic failure/cancellation injection.
type EngineHooks struct {
	FrameError     func(frame int) *Error
	AfterCommit    func(intervalID string) *Error
	BeforeCommit   func(intervalID string) *Error
	BlockJointPush bool
}

// Engine ties the dependency graph, bounded pool, state machine and store.
type Engine struct {
	Asset    Asset
	Budget   Budget
	Store    *SQLiteCheckpointStore
	Activity *ActivityIndex
	Recorder *WaitRecorder
	Hooks    EngineHooks
}

type IntervalRun struct {
	IntervalID string        `json:"interval_id"`
	CacheKey   string        `json:"cache_key"`
	State      IntervalState `json:"state"`
	Reused     bool          `json:"reused"`
	FrameCount int           `json:"frame_count"`
	Reason     string        `json:"reason,omitempty"`
	HitBasis   string        `json:"hit_basis,omitempty"`
}

type RunReport struct {
	Intervals     []IntervalRun    `json:"intervals"`
	Frames        []FrameResult    `json:"-"`
	Metrics       FramePoolMetrics `json:"metrics"`
	StoreSequence []string         `json:"store_sequence"`
}

func (e *Engine) activity() *ActivityIndex {
	if e.Activity == nil {
		e.Activity = BuildActivityIndex(e.Asset)
	}
	return e.Activity
}

func (e *Engine) recorder() *WaitRecorder {
	if e.Recorder == nil {
		e.Recorder = &WaitRecorder{}
	}
	return e.Recorder
}

func (e *Engine) intervalFrames(id string) []int {
	frames := []int{}
	for _, f := range e.Asset.Clip.Frames {
		if IntervalID(f.Index, e.Asset.Precision.IntervalFrames) == id {
			frames = append(frames, f.Index)
		}
	}
	sort.Ints(frames)
	return frames
}

func (e *Engine) cacheKey(id string) ([]string, string) {
	active := e.activity().IntervalJoints[id]
	return BuildIntervalDependencies(e.Asset, id, active)
}

// Run evaluates every interval, reusing version-matching checkpoints.
func (e *Engine) Run(ctx context.Context) (*RunReport, *Error) {
	if e.Budget.MaxWorkers == 0 {
		e.Budget = DefaultBudget()
	}
	rec := e.recorder()
	report := &RunReport{}
	allFrames := []FrameResult{}

	for _, id := range allIntervalIDs(e.Asset) {
		deps, key := e.cacheKey(id)
		topoVersion := TopologyHash(e.Asset.Skeleton)
		if saved, ok := e.Store.Recoverable(id, key, topoVersion, e.Asset.Precision.Version, AlgorithmVersion); ok {
			report.Intervals = append(report.Intervals, IntervalRun{
				IntervalID: id, CacheKey: key, State: StateCommitted, Reused: true, FrameCount: len(saved.Frames),
				Reason: "persistent_cache_hit", HitBasis: "cache_key+topology+parameter+algorithm",
			})
			allFrames = append(allFrames, saved.Frames...)
			continue
		}

		machine := NewIntervalMachine(id)
		if err := machine.Transition(StatePendingRetarget); err != nil {
			return report, err
		}
		if err := machine.Transition(StateRetargeting); err != nil {
			return report, err
		}
		frameIDs := e.intervalFrames(id)
		results := make([]FrameResult, len(frameIDs))
		errs, metrics := RunFramePool(ctx, e.Budget, frameIDs, rec, func(ctx context.Context, workerID, frame int) *Error {
			if e.Hooks.FrameError != nil {
				if ferr := e.Hooks.FrameError(frame); ferr != nil {
					if ferr.Frame == nil {
						f := frame
						ferr.Frame = &f
					}
					return ferr
				}
			}
			fr, ferr := RetargetFrame(e.Asset, e.Asset.Clip.Frames[frame], id)
			if ferr != nil {
				return ferr
			}
			buf := NewBoundedJointBuffer(workerID, frame, e.Budget.Normalized().MaxJointBufferPerWorker, rec)
			done := make(chan []JointState, 1)
			go func() { done <- CollectJointStates(buf.Channel()) }()
			for _, prec := range fr.Precision {
				st := JointState{Frame: frame, Joint: prec.Joint, Output: fr.Outputs[prec.Joint], Precision: prec}
				if perr := buf.Push(ctx, st); perr != nil {
					buf.Close()
					return &Error{Code: CodeState, Message: perr.Error(), Frame: &frame, Joint: prec.Joint}
				}
			}
			buf.Close()
			_ = <-done
			var ordinal int
			for i, f := range frameIDs {
				if f == frame {
					ordinal = i
					break
				}
			}
			results[ordinal] = fr
			return nil
		})
		report.Metrics.MaxObservedWorkers = max32(report.Metrics.MaxObservedWorkers, metrics.MaxObservedWorkers)

		var firstErr *Error
		failedFrame := -1
		for i, err := range errs {
			if err != nil && firstErr == nil {
				firstErr = err
				failedFrame = frameIDs[i]
			}
		}
		if firstErr != nil {
			_ = machine.Fail(failedFrame, firstErr)
			report.Intervals = append(report.Intervals, IntervalRun{IntervalID: id, CacheKey: key, State: StateFailed, Reason: string(firstErr.Code), HitBasis: firstErr.Message})
			report.StoreSequence = e.Store.WriteSequence()
			return report, firstErr
		}
		machine.SetProgress(len(frameIDs))

		frames := make([]FrameResult, 0, len(results))
		digests := map[string]string{}
		precision := []PrecisionRecord{}
		for _, fr := range results {
			frames = append(frames, fr)
			precision = append(precision, fr.Precision...)
			for joint, out := range fr.Outputs {
				digests[fmt.Sprintf("%04d/%s", fr.Index, joint)] = CanonicalHash(out)[:20]
			}
		}
		sort.Slice(frames, func(i, j int) bool { return frames[i].Index < frames[j].Index })
		start, end := frameIDs[0], frameIDs[len(frameIDs)-1]+1
		if e.Hooks.BeforeCommit != nil {
			if berr := e.Hooks.BeforeCommit(id); berr != nil {
				_ = machine.Fail(-1, berr)
				return report, berr
			}
		}
		recCP := IntervalCheckpoint{
			IntervalID: id, CacheKey: key, StartFrame: start, EndFrame: end, FrameCount: len(frames),
			JointDigests: digests, Precision: precision, Frames: frames, DependencySet: deps,
			ActionHash: e.Asset.ContentHash, TopologyVersion: topoVersion,
			ParameterVersion: e.Asset.Precision.Version, AlgorithmVersion: AlgorithmVersion,
		}
		if err := e.Store.CommitInterval(recCP); err != nil {
			_ = machine.Fail(-1, err)
			return report, err
		}
		if err := machine.Transition(StateCommitted); err != nil {
			return report, err
		}
		if e.Hooks.AfterCommit != nil {
			if err := e.Hooks.AfterCommit(id); err != nil {
				return report, err
			}
		}
		report.Intervals = append(report.Intervals, IntervalRun{IntervalID: id, CacheKey: key, State: StateCommitted, FrameCount: len(frames), Reason: "recomputed_and_committed"})
		allFrames = append(allFrames, frames...)
	}

	sort.Slice(allFrames, func(i, j int) bool { return allFrames[i].Index < allFrames[j].Index })
	report.Frames = allFrames
	report.Metrics.Waits = rec.Records()
	report.StoreSequence = e.Store.WriteSequence()
	return report, nil
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// CanonicalSequence returns interval IDs in stable frame order.
func CanonicalSequence(report *RunReport) []string {
	out := []string{}
	for _, in := range report.Intervals {
		out = append(out, in.IntervalID)
	}
	sort.Strings(out)
	return out
}
