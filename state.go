package macrokit

type ExecutorConfig struct {
	FrameCount  int64
	Arbitration ArbitrationPolicy
	Flags       map[string]bool
}

type ArbitrationPolicy string

const (
	ArbitrationDeterministic ArbitrationPolicy = "deterministic"
	ArbitrationStrict        ArbitrationPolicy = "strict"
)

type RuntimeState struct {
	BranchID  string `json:"branch_id"`
	NextIndex int    `json:"next_index"`
	NextFrame int64  `json:"next_frame"`
}

type ExecutionState struct {
	NextFrame      int64                    `json:"next_frame"`
	Down           map[string]bool          `json:"down,omitempty"`
	PressedAt      map[string]int64         `json:"pressed_at,omitempty"`
	Flags          map[string]bool          `json:"flags,omitempty"`
	ConsumedOffset int                      `json:"consumed_offset"`
	Results        map[string]*BranchState  `json:"results,omitempty"`
	Active         map[string]*RuntimeState `json:"active,omitempty"`
	Frames         []FrameTrace             `json:"frames,omitempty"`
	Adjudications  []Adjudication           `json:"adjudications,omitempty"`
	Commits        []CommitRecord           `json:"commits,omitempty"`
}

func cloneBranchState(in *BranchState) *BranchState {
	if in == nil {
		return nil
	}
	out := *in
	out.ExecutedSteps = append([]StepExecution(nil), in.ExecutedSteps...)
	return &out
}

func cloneTrace(in FrameTrace) FrameTrace {
	out := in
	out.Triggered = append([]string(nil), in.Triggered...)
	out.ExecutedSteps = append([]StepExecution(nil), in.ExecutedSteps...)
	out.RemainingDelays = append([]DelayState(nil), in.RemainingDelays...)
	out.Cancellations = append([]Cancellation(nil), in.Cancellations...)
	out.Adjudications = append([]Adjudication(nil), in.Adjudications...)
	if in.ConditionSnapshot != nil {
		out.ConditionSnapshot = make(map[string]bool, len(in.ConditionSnapshot))
		for k, v := range in.ConditionSnapshot {
			out.ConditionSnapshot[k] = v
		}
	}
	return out
}
