package syncgame

import (
	"fmt"
)

type AuthorityRun struct {
	States       []WorldState `json:"states"`
	InputOffset  int          `json:"input_offset"`
	RandomCursor int          `json:"random_cursor"`
	Settlement   Settlement   `json:"settlement"`
	Algorithm    string       `json:"algorithm_version"`
	StreamHash   string       `json:"stream_fingerprint"`
}

func RunAuthoritative(sample *Sample, parallel bool, workers int) (*AuthorityRun, error) {
	engine, err := NewEngine(sample)
	if err != nil {
		return nil, err
	}
	state := engine.InitialState()
	states := []WorldState{CloneWorld(state)}
	limit := len(sample.Intents)
	if limit > sample.MaxTicks {
		limit = sample.MaxTicks
	}
	for i := 0; i < limit; i++ {
		intent := sample.Intents[i]
		if state.Finished {
			break
		}
		if intent.Tick != state.Tick {
			return nil, tickErr(intent.Tick, "*", "tick",
				fmt.Sprintf("intent stream discontinuity at state tick %d", state.Tick))
		}
		if parallel {
			state, err = engine.StepParallel(state, intent, workers)
		} else {
			state, err = engine.Step(state, intent)
		}
		if err != nil {
			return nil, err
		}
		states = append(states, CloneWorld(state))
	}
	return &AuthorityRun{
		States:       states,
		InputOffset:  len(states) - 1,
		RandomCursor: len(states) - 1,
		Settlement:   engine.Settlement(state),
		Algorithm:    sample.AlgorithmVersion,
		StreamHash:   sample.StreamFingerprint(),
	}, nil
}

func SimulateAuthoritative(sample *Sample) (*AuthorityRun, error) {
	return RunAuthoritative(sample, false, 1)
}
