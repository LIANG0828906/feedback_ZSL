package core

type CommitHook func(result FrameResult) *Error

type RunOptions struct {
	Path         string
	Parallel     bool
	Resources    ResourceConfig
	Prefix       []FrameResult
	Commit       CommitHook
	StateMachine *StateMachine
}

func (e *Engine) Run(opts RunOptions) ([]FrameResult, *Error) {
	frameCount := len(e.Clip.Frames)
	start := len(opts.Prefix)
	if start > frameCount {
		return nil, &Error{Code: ECodeState, Message: "prefix contains more frames than clip"}
	}
	sm := opts.StateMachine
	if sm == nil {
		sm = NewStateMachine(frameCount)
	}
	for _, f := range opts.Prefix {
		_ = sm.Transition(f.Frame, StateSolving)
		_ = sm.Transition(f.Frame, StateCommitted)
	}

	frames := make([]FrameResult, 0, frameCount)
	frames = append(frames, opts.Prefix...)
	var restored *FrameResult
	if len(opts.Prefix) > 0 {
		last := opts.Prefix[len(opts.Prefix)-1]
		restored = &last
	}
	contact := NewContactProcessor(e, restored)

	if opts.Parallel {
		resources := opts.Resources
		if resources.Workers <= 0 {
			resources = DefaultResources()
		}
		for f := start; f < frameCount; f++ {
			if err := sm.Transition(f, StateSolving); err != nil {
				return frames, err
			}
		}
		solved, firstErr := RunIndexedParallel(frameCount-start, resources, func(k int) (*solvedFrame, *Error) {
			return e.solveFrameData(start + k)
		})
		if firstErr != nil {
			if firstErr.Frame >= 0 {
				_ = sm.Transition(firstErr.Frame, StateFailed)
			}
			return frames, firstErr
		}
		for k, sf := range solved {
			f := start + k
			if err := contact.Apply(f, sf); err != nil {
				_ = sm.Transition(f, StateFailed)
				return frames, err
			}
			sf.result.State = string(StateCommitted)
			sf.result.ContentHash = sf.result.ComputeHash()
			if opts.Commit != nil {
				if err := opts.Commit(sf.result); err != nil {
					return frames, err
				}
			}
			if err := sm.Transition(f, StateCommitted); err != nil {
				return frames, err
			}
			frames = append(frames, sf.result)
		}
		return frames, nil
	}

	for f := start; f < frameCount; f++ {
		if err := sm.Transition(f, StateSolving); err != nil {
			return frames, err
		}
		sf, err := e.solveFrameData(f)
		if err != nil {
			_ = sm.Transition(f, StateFailed)
			return frames, err
		}
		if err := contact.Apply(f, sf); err != nil {
			_ = sm.Transition(f, StateFailed)
			return frames, err
		}
		sf.result.State = string(StateCommitted)
		sf.result.ContentHash = sf.result.ComputeHash()
		if opts.Commit != nil {
			if err := opts.Commit(sf.result); err != nil {
				return frames, err
			}
		}
		if err := sm.Transition(f, StateCommitted); err != nil {
			return frames, err
		}
		frames = append(frames, sf.result)
	}
	return frames, nil
}
