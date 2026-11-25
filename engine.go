package lipsync

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

const (
	DefaultWindowSize       = 12
	DefaultMaxWorkers       = 2
	DefaultFrameBufferLimit = 4
)

type QueueEvent struct {
	DialogueID string `json:"dialogue_id"`
	Window     int    `json:"window"`
	Frame      int    `json:"frame"`
	Reason     string `json:"reason"`
}

type RunOptions struct {
	WindowSize        int
	MaxWorkers        int
	FrameBufferLimit  int
	InjectStageWindow int
	InjectStageReason string
}

func normalizeOptions(options RunOptions) RunOptions {
	if options.WindowSize <= 0 {
		options.WindowSize = DefaultWindowSize
	}
	if options.MaxWorkers <= 0 {
		options.MaxWorkers = DefaultMaxWorkers
	}
	if options.FrameBufferLimit <= 0 {
		options.FrameBufferLimit = DefaultFrameBufferLimit
	}
	if options.InjectStageWindow == 0 && options.InjectStageReason == "" {
		options.InjectStageWindow = -1
	}
	if options.InjectStageReason == "" {
		options.InjectStageWindow = -1
	}
	return options
}

type PathResult struct {
	Name               string           `json:"name"`
	Frames             []FrameResult    `json:"frames"`
	Transitions        []LipTransition  `json:"transitions"`
	Switches           []LipSwitch      `json:"switches"`
	ReusedFrames       int              `json:"reused_frames"`
	RecomputedFrames   int              `json:"recomputed_frames"`
	RestoredWindows    int              `json:"restored_windows"`
	CommittedWindows   int              `json:"committed_windows"`
	ResetReason        string           `json:"reset_reason,omitempty"`
	QueueEvents        []QueueEvent     `json:"queue_events,omitempty"`
	CheckpointMismatch string           `json:"checkpoint_mismatch,omitempty"`
	Windows            []WindowSnapshot `json:"windows,omitempty"`
}

type WindowSnapshot struct {
	Index      int               `json:"index"`
	FrameStart int               `json:"frame_start"`
	FrameEnd   int               `json:"frame_end"`
	State      WindowState       `json:"state"`
	Progress   int               `json:"progress"`
	History    []StateTransition `json:"history"`
}

type windowComputation struct {
	window *Window
	frames []FrameResult
	deps   []FrameDependency
	reused int
	queue  []QueueEvent
	err    error
}

type frameBuffer struct {
	mu     sync.Mutex
	events []QueueEvent
}

func newFrameBuffer() *frameBuffer {
	return &frameBuffer{}
}

func (b *frameBuffer) recordBlocked(dialogueID string, window, frame int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, QueueEvent{
		DialogueID: dialogueID,
		Window:     window,
		Frame:      frame,
		Reason:     "frame_buffer_exhausted_blocking",
	})
}

func (b *frameBuffer) snapshot() []QueueEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := append([]QueueEvent(nil), b.events...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Window != out[j].Window {
			return out[i].Window < out[j].Window
		}
		return out[i].Frame < out[j].Frame
	})
	return out
}

func checkpointForWindow(timeline *Timeline, window *Window, frames []FrameResult, deps []FrameDependency) (Checkpoint, error) {
	contentHash, err := ContentHash(timeline.Project)
	if err != nil {
		return Checkpoint{}, err
	}
	chars := make([]CharInterval, len(frames))
	lips := make([]LipCategory, len(frames))
	keys := make([]string, len(frames))
	for i, frame := range frames {
		chars[i] = CharInterval{Frame: frame.Frame, CharStart: frame.CharStart, CharEnd: frame.CharEnd}
		lips[i] = frame.Lip
		keys[i] = deps[i].CacheKey
	}
	consumedSamples := int64(0)
	if window.FrameEnd > 0 {
		frameSamples := int64(timeline.Project.SampleRate) / int64(timeline.Project.FPS)
		consumedSamples = int64(window.FrameEnd) * frameSamples
		if consumedSamples > timeline.Project.TotalSamples() {
			consumedSamples = timeline.Project.TotalSamples()
		}
	}
	cp := Checkpoint{
		DialogueID:            timeline.Project.DialogueID,
		WindowIndex:           window.Index,
		FrameStart:            window.FrameStart,
		FrameEnd:              window.FrameEnd,
		Status:                StateCommitted,
		ContentHash:           contentHash,
		QuantizationVersion:   timeline.Project.Quantization.Version,
		AlgorithmVersion:      timeline.Project.AlgorithmVersion,
		Frames:                frames,
		Dependencies:          deps,
		CharIntervals:         chars,
		LipSequence:           lips,
		DepKeys:               keys,
		ConsumedPhonemeOffset: consumedPhonemeOffset(timeline.Timing, window.FrameEnd),
		ConsumedSamples:       consumedSamples,
	}
	hash, err := checkpointPayloadHash(cp)
	if err != nil {
		return Checkpoint{}, err
	}
	cp.PayloadHash = hash
	return cp, nil
}

type runOutput struct {
	frames      []FrameResult
	transitions []LipTransition
	switches    []LipSwitch
	reused      int
	recomputed  int
	committed   int
	queueEvents []QueueEvent
	windows     []WindowSnapshot
}

func runWindows(
	ctx context.Context,
	store *Store,
	timeline *Timeline,
	startWindow int,
	options RunOptions,
) (runOutput, error) {
	options = normalizeOptions(options)
	frameCount := len(timeline.Frames)
	windowCount := (frameCount + options.WindowSize - 1) / options.WindowSize
	if startWindow < 0 || startWindow > windowCount {
		return runOutput{}, &DomainError{
			Code:       CodeStage,
			DialogueID: timeline.Project.DialogueID,
			Window:     intPtr(startWindow),
			Field:      "window",
			Reason:     "start window outside dialogue",
			Message:    "start window is outside the dialogue",
		}
	}
	computations := make([]windowComputation, windowCount-startWindow)
	buffer := newFrameBuffer()
	semaphore := make(chan struct{}, options.MaxWorkers)
	var wg sync.WaitGroup
	for offset := range computations {
		index := startWindow + offset
		frameStart := index * options.WindowSize
		frameEnd := frameStart + options.WindowSize
		if frameEnd > frameCount {
			frameEnd = frameCount
		}
		window := NewWindow(timeline.Project.DialogueID, index, frameStart, frameEnd)
		wg.Add(1)
		go func(offset int, window *Window, frameStart, frameEnd int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			computation := windowComputation{window: window}
			if err := window.Transition(StateScheduling); err != nil {
				computation.err = err
				computations[offset] = computation
				return
			}
			if err := window.Transition(StateLipMapping); err != nil {
				computation.err = err
				computations[offset] = computation
				return
			}
			if options.InjectStageWindow == window.Index {
				reason := options.InjectStageReason
				if reason == "" {
					reason = "injected_window_failure"
				}
				computation.err = window.Fail("lip_mapping", frameStart, reason)
				computations[offset] = computation
				return
			}
			frames := make([]FrameResult, 0, frameEnd-frameStart)
			deps := make([]FrameDependency, 0, frameEnd-frameStart)
			pending := make([]int, 0, options.FrameBufferLimit)
			processFrame := func(frameIndex int) error {
				dep := timeline.Dependencies[frameIndex]
				cached, ok, err := store.LoadFrameCache(ctx, timeline.Project.DialogueID, dep.CacheKey)
				if err != nil {
					return err
				}
				if ok && cacheFrameMatches(cached.Frame, timeline.Frames[frameIndex]) {
					cached.Frame.Reused = true
					frames = append(frames, cached.Frame)
					computation.reused++
				} else {
					frames = append(frames, timeline.Frames[frameIndex])
				}
				deps = append(deps, dep)
				return window.SetProgress(frameIndex - frameStart + 1)
			}
			for frameIndex := frameStart; frameIndex < frameEnd; frameIndex++ {
				if len(pending) >= options.FrameBufferLimit {
					buffer.recordBlocked(timeline.Project.DialogueID, window.Index, frameIndex)
					if err := processFrame(pending[0]); err != nil {
						_ = window.Transition(StateFailed)
						computation.err = err
						computations[offset] = computation
						return
					}
					pending = pending[1:]
				}
				pending = append(pending, frameIndex)
			}
			for len(pending) > 0 {
				if err := processFrame(pending[0]); err != nil {
					_ = window.Transition(StateFailed)
					computation.err = err
					computations[offset] = computation
					return
				}
				pending = pending[1:]
			}
			computation.frames = frames
			computation.deps = deps
			computations[offset] = computation
		}(offset, window, frameStart, frameEnd)
	}
	wg.Wait()
	queueEvents := buffer.snapshot()
	for i := range computations {
		if computations[i].err != nil {
			return runOutput{}, computations[i].err
		}
	}
	result := make([]FrameResult, 0, frameCount-startWindow*options.WindowSize)
	windows := make([]WindowSnapshot, 0, len(computations))
	reused := 0
	committed := 0
	for i := range computations {
		computation := &computations[i]
		cp, err := checkpointForWindow(timeline, computation.window, computation.frames, computation.deps)
		if err != nil {
			return runOutput{}, err
		}
		if err := store.CommitWindow(ctx, cp); err != nil {
			if stateErr := computation.window.Fail("checkpoint", computation.window.FrameStart, "checkpoint_commit_failed"); stateErr != nil {
				return runOutput{}, stateErr
			}
			return runOutput{}, err
		}
		if err := computation.window.Transition(StateCommitted); err != nil {
			return runOutput{}, err
		}
		committed++
		reused += computation.reused
		result = append(result, computation.frames...)
		windows = append(windows, WindowSnapshot{
			Index:      computation.window.Index,
			FrameStart: computation.window.FrameStart,
			FrameEnd:   computation.window.FrameEnd,
			State:      computation.window.State(),
			Progress:   computation.window.Progress(),
			History:    computation.window.History(),
		})
	}
	transitions, switches := deriveLipEvents(result)
	return runOutput{
		frames:      result,
		transitions: transitions,
		switches:    switches,
		reused:      reused,
		recomputed:  len(result) - reused,
		committed:   committed,
		queueEvents: queueEvents,
		windows:     windows,
	}, nil
}

func cacheFrameMatches(cached, current FrameResult) bool {
	return cached.Frame == current.Frame &&
		cached.CharStart == current.CharStart &&
		cached.CharEnd == current.CharEnd &&
		cached.BaseLip == current.BaseLip &&
		cached.Lip == current.Lip &&
		cached.Transition == current.Transition &&
		cached.PhonemeID == current.PhonemeID &&
		cached.SegmentID == current.SegmentID
}

func deriveLipEvents(frames []FrameResult) ([]LipTransition, []LipSwitch) {
	switches := make([]LipSwitch, 0)
	transitions := make([]LipTransition, 0)
	for f := 1; f < len(frames); f++ {
		previous := frames[f-1].BaseLip
		current := frames[f].BaseLip
		if previous != current {
			switches = append(switches, LipSwitch{Frame: f, From: previous, To: current})
		}
		if frames[f].Transition {
			left, right := lipFamily(previous), lipFamily(current)
			distance := left - right
			if distance < 0 {
				distance = -distance
			}
			transitions = append(transitions, LipTransition{
				Frame:          f,
				From:           previous,
				To:             current,
				Inserted:       []LipCategory{frames[f].Lip},
				InsertedFrames: []int{f},
				Distance:       distance + 1,
			})
		}
	}
	return transitions, switches
}

func RunRealtime(ctx context.Context, store *Store, project DialogueProject, options RunOptions) (PathResult, error) {
	timeline, err := BuildTimeline(project)
	if err != nil {
		return PathResult{}, err
	}
	run, err := runWindows(ctx, store, timeline, 0, options)
	if err != nil {
		return PathResult{}, err
	}
	return PathResult{
		Name:             "realtime",
		Frames:           run.frames,
		Transitions:      run.transitions,
		Switches:         run.switches,
		ReusedFrames:     run.reused,
		RecomputedFrames: run.recomputed,
		CommittedWindows: run.committed,
		QueueEvents:      run.queueEvents,
		Windows:          run.windows,
	}, nil
}

// RunResume deliberately takes only a Store and project: no previous result
// object is accepted. A valid checkpoint prefix is the only source of restored
// frames; any mismatch purges both checkpoints and the frame cache.
func RunResume(ctx context.Context, store *Store, project DialogueProject, options RunOptions) (PathResult, error) {
	options = normalizeOptions(options)
	timeline, err := BuildTimeline(project)
	if err != nil {
		return PathResult{}, err
	}
	checkpoints, err := store.LoadCheckpoints(ctx, project.DialogueID)
	if err != nil {
		return PathResult{}, err
	}
	mismatch, valid := ValidateCheckpoints(project, checkpoints, len(timeline.Frames), options.WindowSize)
	if valid {
		if cacheMismatch, cacheValid := store.ValidateFrameCacheForCheckpoints(ctx, checkpoints); !cacheValid {
			mismatch = cacheMismatch
			valid = false
		}
	}
	if !valid {
		if err := store.PurgeDialogue(ctx, project.DialogueID); err != nil {
			return PathResult{}, err
		}
		result, err := RunRealtime(ctx, store, project, options)
		if err != nil {
			return PathResult{}, err
		}
		result.Name = "resume"
		result.ResetReason = mismatch
		result.CheckpointMismatch = mismatch
		return result, nil
	}
	restored := make([]FrameResult, 0)
	restoredWindows := make([]WindowSnapshot, 0)
	for _, cp := range checkpoints {
		for i, frame := range cp.Frames {
			if !cacheFrameMatches(frame, timeline.Frames[cp.FrameStart+i]) {
				if err := store.PurgeDialogue(ctx, project.DialogueID); err != nil {
					return PathResult{}, err
				}
				result, err := RunRealtime(ctx, store, project, options)
				if err != nil {
					return PathResult{}, err
				}
				result.Name = "resume"
				result.ResetReason = fmt.Sprintf("restored_frame_mismatch:%d", frame.Frame)
				result.CheckpointMismatch = result.ResetReason
				return result, nil
			}
			frame.Reused = true
			restored = append(restored, frame)
		}
		restoredWindows = append(restoredWindows, WindowSnapshot{
			Index:      cp.WindowIndex,
			FrameStart: cp.FrameStart,
			FrameEnd:   cp.FrameEnd,
			State:      StateCommitted,
			Progress:   cp.FrameEnd - cp.FrameStart,
			History: []StateTransition{
				{From: StatePending, To: StateScheduling},
				{From: StateScheduling, To: StateLipMapping},
				{From: StateLipMapping, To: StateCommitted},
			},
		})
	}
	startWindow := len(checkpoints)
	var frames []FrameResult
	var transitions []LipTransition
	var switches []LipSwitch
	var recomputed, committed int
	var suffixReused int
	var events []QueueEvent
	var suffixWindows []WindowSnapshot
	if startWindow*options.WindowSize < len(timeline.Frames) {
		run, runErr := runWindows(ctx, store, timeline, startWindow, options)
		err = runErr
		if err != nil {
			return PathResult{}, err
		}
		frames = run.frames
		transitions = run.transitions
		switches = run.switches
		recomputed = run.recomputed
		suffixReused = run.reused
		committed = run.committed
		events = run.queueEvents
		suffixWindows = run.windows
	} else {
		transitions, switches = deriveLipEvents(restored)
	}
	frames = append(restored, frames...)
	restoredWindows = append(restoredWindows, suffixWindows...)
	transitions, switches = deriveLipEvents(frames)
	return PathResult{
		Name:             "resume",
		Frames:           frames,
		Transitions:      transitions,
		Switches:         switches,
		ReusedFrames:     len(restored) + suffixReused,
		RecomputedFrames: recomputed,
		RestoredWindows:  len(checkpoints),
		CommittedWindows: committed,
		QueueEvents:      events,
		Windows:          restoredWindows,
	}, nil
}

type IncrementalResult struct {
	Frames            []FrameResult
	Transitions       []LipTransition
	Switches          []LipSwitch
	Invalidations     []Invalidation
	InvalidatedFrames int
	ReusedFrames      int
	RecomputedFrames  int
	OutputMatch       bool
	Mismatch          *Mismatch
}

// ComputeIncremental reuses persisted clean frames and recomputes only frame
// dependency ranges that differ from the baseline.
func ComputeIncremental(ctx context.Context, store *Store, project DialogueProject, baseline *Timeline) (IncrementalResult, error) {
	current, err := BuildTimeline(project)
	if err != nil {
		return IncrementalResult{}, err
	}
	if baseline == nil {
		return IncrementalResult{}, &DomainError{Code: CodeStage, DialogueID: project.DialogueID, Field: "baseline", Message: "incremental computation requires a baseline"}
	}
	invalidations := InvalidateDependencies(baseline.Dependencies, current.Dependencies)
	dirty := make([]bool, len(current.Frames))
	for _, invalidation := range invalidations {
		start := invalidation.FrameStart
		if start < 0 {
			start = 0
		}
		end := invalidation.FrameEnd
		if end > len(dirty) {
			end = len(dirty)
		}
		for f := start; f < end; f++ {
			dirty[f] = true
		}
	}
	frames := make([]FrameResult, len(current.Frames))
	reused := 0
	for f := range current.Frames {
		if !dirty[f] {
			cached, ok, err := store.LoadFrameCache(ctx, project.DialogueID, current.Dependencies[f].CacheKey)
			if err != nil {
				return IncrementalResult{}, err
			}
			if ok && cacheFrameMatches(cached.Frame, current.Frames[f]) {
				cached.Frame.Reused = true
				frames[f] = cached.Frame
				reused++
				continue
			}
			dirty[f] = true
			invalidations = append(invalidations, Invalidation{
				DialogueID: project.DialogueID,
				FrameStart: f,
				FrameEnd:   f + 1,
				Reasons:    []string{"cache_miss"},
				Fields:     []string{"frame_cache"},
			})
		}
		frames[f] = current.Frames[f]
	}
	incrementalDirty := make([]CachedFrame, 0)
	contentHash, err := ContentHash(project)
	if err != nil {
		return IncrementalResult{}, err
	}
	for f := range frames {
		if dirty[f] {
			incrementalDirty = append(incrementalDirty, CachedFrame{
				Dependency: current.Dependencies[f],
				Frame:      current.Frames[f],
			})
		}
	}
	if len(incrementalDirty) > 0 {
		if err := store.UpsertFrameCacheBatch(ctx, contentHash, project.Quantization.Version, project.AlgorithmVersion, incrementalDirty); err != nil {
			return IncrementalResult{}, err
		}
	}
	transitions, switches := deriveLipEvents(frames)
	removedFrames := 0
	if len(baseline.Dependencies) > len(current.Dependencies) {
		removedFrames = len(baseline.Dependencies) - len(current.Dependencies)
	}
	invalidations = NormalizeInvalidations(invalidations)
	comparison := CompareFrames(project.DialogueID, "incremental_vs_full", frames, current.Frames)
	comparison = func() Comparison {
		if !comparison.Consistent {
			return comparison
		}
		return CompareTransitions(project.DialogueID, "incremental_transitions_vs_full", transitions, current.Transitions)
	}()
	return IncrementalResult{
		Frames:            frames,
		Transitions:       transitions,
		Switches:          switches,
		Invalidations:     invalidations,
		InvalidatedFrames: len(incrementalDirty) + removedFrames,
		ReusedFrames:      reused,
		RecomputedFrames:  len(incrementalDirty),
		OutputMatch:       comparison.Consistent,
		Mismatch:          comparison.Mismatch,
	}, nil
}
