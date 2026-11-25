package model

type SessionFSM struct {
	State    SessionState
	Progress int
}

func NewSessionFSM() *SessionFSM {
	return &SessionFSM{State: SessionEstablished}
}

func sessionTransitionAllowed(from, to SessionState) bool {
	if from == to {
		return false
	}
	switch from {
	case SessionEstablished:
		return to == SessionNegotiating || to == SessionEnded
	case SessionNegotiating:
		return to == SessionStreaming || to == SessionEnded
	case SessionStreaming:
		return to == SessionPaused || to == SessionEnded
	case SessionPaused:
		return to == SessionStreaming || to == SessionEnded
	default:
		return false
	}
}

func (s *SessionFSM) Transition(to SessionState) *KernelError {
	if !sessionTransitionAllowed(s.State, to) {
		return &KernelError{
			Code:        CodeState,
			Message:     "illegal session transition",
			State:       string(s.State),
			TargetState: string(to),
			Progress:    s.Progress,
		}
	}
	s.State = to
	return nil
}

func (s *SessionFSM) AdvanceProgress(window int) {
	if window > s.Progress {
		s.Progress = window
	}
}

type ScreenFSM struct {
	State    ScreenState
	Progress int
}

func NewScreenFSM() *ScreenFSM {
	return &ScreenFSM{State: ScreenPending}
}

func screenTransitionAllowed(from, to ScreenState) bool {
	if from == to {
		return false
	}
	switch from {
	case ScreenPending:
		return to == ScreenNegotiating || to == ScreenFailed
	case ScreenNegotiating:
		return to == ScreenCommitted || to == ScreenFailed
	case ScreenCommitted:
		return to == ScreenNegotiating || to == ScreenFailed
	default:
		return false
	}
}

func (s *ScreenFSM) Transition(to ScreenState) *KernelError {
	if !screenTransitionAllowed(s.State, to) {
		return &KernelError{
			Code:        CodeState,
			Message:     "illegal screen transition",
			State:       string(s.State),
			TargetState: string(to),
			Progress:    s.Progress,
		}
	}
	s.State = to
	return nil
}

func (s *ScreenFSM) AdvanceProgress(window int) {
	if window > s.Progress {
		s.Progress = window
	}
}

type WindowFSM struct {
	State    WindowState
	Progress int
}

func NewWindowFSM() *WindowFSM {
	return &WindowFSM{State: WindowPending}
}

func windowTransitionAllowed(from, to WindowState) bool {
	if from == to {
		return false
	}
	switch from {
	case WindowPending:
		return to == WindowDistributing || to == WindowFailed
	case WindowDistributing:
		return to == WindowCommitted || to == WindowFailed
	default:
		return false
	}
}

func (w *WindowFSM) Transition(to WindowState) *KernelError {
	if !windowTransitionAllowed(w.State, to) {
		return &KernelError{
			Code:        CodeState,
			Message:     "illegal frame-window transition",
			State:       string(w.State),
			TargetState: string(to),
			Progress:    w.Progress,
		}
	}
	w.State = to
	return nil
}

func (w *WindowFSM) AdvanceProgress(screenCount int) {
	w.Progress = screenCount
}
