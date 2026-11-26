package lightprobe

type RoomState string

const (
	StatePending       RoomState = "pending"
	StateCollecting    RoomState = "collecting"
	StateReconstructed RoomState = "reconstructed"
	StateCommitted     RoomState = "committed"
	StateFailed        RoomState = "failed"
)

type RoomProgress struct {
	RoomID    string    `json:"room_id"`
	State     RoomState `json:"state"`
	Progress  float64   `json:"progress"`
	Attempts  int       `json:"attempts"`
	LastError *Error    `json:"last_error,omitempty"`
}

var legalTransitions = map[RoomState]map[RoomState]bool{
	StatePending: {
		StateCollecting: true,
	},
	StateCollecting: {
		StateReconstructed: true,
		StateFailed:        true,
	},
	StateReconstructed: {
		StateCommitted:  true,
		StateCollecting: true,
		StateFailed:     true,
	},
	StateFailed: {
		StateCollecting: true,
	},
	StateCommitted: {
		StateCollecting: true,
	},
}

func NewRoomProgress(roomID string) RoomProgress {
	return RoomProgress{RoomID: roomID, State: StatePending}
}

func CanTransition(from, to RoomState) bool {
	return legalTransitions[from] != nil && legalTransitions[from][to]
}

func (rp *RoomProgress) Transition(to RoomState) error {
	if !CanTransition(rp.State, to) {
		return domainError(EState, rp.RoomID, "state",
			"illegal transition %s -> %s; state and progress %.3f preserved", rp.State, to, rp.Progress)
	}
	rp.State = to
	switch to {
	case StateCollecting:
		rp.Attempts++
		if rp.Progress < 0.05 {
			rp.Progress = 0.05
		}
	case StateReconstructed:
		rp.Progress = 0.8
	case StateCommitted:
		rp.Progress = 1
	case StateFailed:
		rp.Progress = 0
	}
	return nil
}

func (rp *RoomProgress) SetProgress(v float64) error {
	if v < 0 || v > 1 {
		return domainError(EVal, rp.RoomID, "progress", "progress %.3f outside [0,1]", v)
	}
	rp.Progress = v
	return nil
}
