package channelpipe

type RunState string

const (
	StatePending    RunState = "pending"
	StateProcessing RunState = "processing"
	StateCommitted  RunState = "committed"
	StateFailed     RunState = "failed"
)

var legalTransitions = map[RunState]map[RunState]bool{
	StatePending: {
		StateProcessing: true,
	},
	StateProcessing: {
		StateCommitted: true,
		StateFailed:    true,
		StatePending:   true,
	},
	StateFailed: {
		StateProcessing: true,
		StatePending:    true,
	},
	StateCommitted: {
		StatePending: true, // explicit graph invalidation/new revision
	},
}

func CanTransition(from, to RunState) bool {
	return legalTransitions[from] != nil && legalTransitions[from][to]
}

func TransitionState(from, to RunState) (RunState, error) {
	if from == to {
		return from, nil
	}
	if !CanTransition(from, to) {
		return from, FieldError(CodeState, "state", "illegal transition %s -> %s", from, to)
	}
	return to, nil
}

type Progress struct {
	CommittedChunks int `json:"committed_chunks"`
	TotalChunks     int `json:"total_chunks"`
}

func (p Progress) Percent() float64 {
	if p.TotalChunks == 0 {
		return 100
	}
	return float64(p.CommittedChunks) / float64(p.TotalChunks) * 100
}
