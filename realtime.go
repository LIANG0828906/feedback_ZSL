package macrokit

// RealtimeSession is the logical real-time path. Time is advanced explicitly
// one frame at a time so tests and local examples remain deterministic.
type RealtimeSession struct {
	exec *Executor
}

func NewRealtimeSession(project *Project, config ExecutorConfig) (*RealtimeSession, error) {
	exec, err := NewExecutor(project, config)
	if err != nil {
		return nil, err
	}
	return &RealtimeSession{exec: exec}, nil
}

func (s *RealtimeSession) Tick(frame int64, events ...KeyEvent) (FrameTrace, error) {
	return s.exec.Step(FrameInput{Frame: frame, Events: events})
}

func (s *RealtimeSession) Result() *RunResult { return s.exec.Result() }

func RunRealtime(project *Project, stream EventStream, config ExecutorConfig) (*RunResult, error) {
	inputs, err := BuildFrameInputs(stream, config.FrameCount)
	if err != nil {
		return nil, err
	}
	session, err := NewRealtimeSession(project, config)
	if err != nil {
		return nil, err
	}
	for _, input := range inputs {
		if _, err := session.Tick(input.Frame, input.Events...); err != nil {
			return session.Result(), err
		}
	}
	return session.Result(), nil
}
