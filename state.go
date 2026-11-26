package maskqa

var legalTransitions = map[State]map[State]bool{
	StatePending: {
		StateProcessing: true,
	},
	StateProcessing: {
		StateCommitted: true,
		StateFailed:    true,
		StatePending:   true,
	},
	StateFailed: {
		StatePending: true,
	},
	StateCommitted: {
		StatePending: true,
	},
}

func TransitionState(current, next State) (State, error) {
	if current == next {
		return current, nil
	}
	if legalTransitions[current] != nil && legalTransitions[current][next] {
		return next, nil
	}
	return current, stateError(current, next)
}

type NodeRef struct {
	SampleID string     `json:"sample_id"`
	Block    BlockCoord `json:"block"`
	Stage    Stage      `json:"stage"`
}

func (n NodeRef) Key() string {
	return n.SampleID + "/" + string(n.Stage) + "/" + n.Block.ID()
}

type InvalidationItem struct {
	SampleID   string     `json:"sample_id"`
	Block      BlockCoord `json:"block"`
	Stage      Stage      `json:"stage"`
	Reason     string     `json:"reason"`
	ParamField string     `json:"param_field,omitempty"`
	Upstream   []string   `json:"upstream"`
}
