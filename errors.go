package syncgame

import (
	"errors"
	"fmt"
)

const (
	CodeTick        = "E_TICK"
	CodeRollback    = "E_ROLLBACK"
	CodeLatency     = "E_LAT"
	CodeDiverge     = "E_DIVERGE"
	CodeSnapshot    = "E_SNAPSHOT"
	CodeOrder       = "E_ORDER"
	CodeConflict    = "E_CONFLICT"
	CodeBounds      = "E_BOUNDS"
	CodeUnsupported = "E_UNSUPPORTED"
)

// GameError is the machine-readable error returned by validation and runtime
// checks. Fields are intentionally value based so they serialize cleanly for
// the local HTTP demo.
type GameError struct {
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	Tick     int            `json:"tick"`
	PlayerID string         `json:"player_id,omitempty"`
	Frame    int            `json:"frame,omitempty"`
	Field    string         `json:"field,omitempty"`
	Measured int            `json:"measured,omitempty"`
	Limit    int            `json:"limit,omitempty"`
	Left     any            `json:"left,omitempty"`
	Right    any            `json:"right,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
}

func (e *GameError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := e.Code + ": " + e.Message
	if e.Tick != 0 || e.PlayerID != "" || e.Field != "" {
		msg += fmt.Sprintf(" (tick=%d", e.Tick)
		if e.PlayerID != "" {
			msg += ", player=" + e.PlayerID
		}
		if e.Field != "" {
			msg += ", field=" + e.Field
		}
		msg += ")"
	}
	return msg
}

func gameErr(code, message string) *GameError {
	return &GameError{Code: code, Message: message}
}

func tickErr(tick int, playerID, field, message string) *GameError {
	return &GameError{
		Code:     CodeTick,
		Message:  message,
		Tick:     tick,
		PlayerID: playerID,
		Field:    field,
	}
}

func IsCode(err error, code string) bool {
	var ge *GameError
	return errors.As(err, &ge) && ge.Code == code
}
