// Package timeconv performs checked integer conversion between turn serial
// numbers and millisecond timestamps. No floating point is ever used.
package timeconv

import (
	"math"

	"replayaudit/internal/errs"
	"replayaudit/internal/model"
)

// Basis labels the semantic interpretation of a source "at" field.
const (
	BasisTurn = "turn"
	BasisMS   = "ms"
)

// TurnToMS maps a 1-based turn serial to its declared representative ms.
//
// With anchor "start": ms = (turn-1)*msPerTurn.
// With anchor "end":   ms = turn*msPerTurn - 1.
//
// All arithmetic is checked; overflow yields E_OVF tagged with eventID.
func TurnToMS(turn, msPerTurn int64, anchor, eventID string) (int64, error) {
	if msPerTurn <= 0 {
		return 0, errs.New(errs.ESchema, "params.ms_per_turn", eventID, "must be positive, got %d", msPerTurn)
	}
	if turn <= 0 {
		return 0, errs.New(errs.ESchema, "at", eventID, "turn serial must be >= 1, got %d", turn)
	}
	switch anchor {
	case "start", "":
		base := turn - 1
		if base > math.MaxInt64/msPerTurn {
			return 0, errs.New(errs.EOvf, "at", eventID, "turn->ms overflow: turn=%d unit=%d", turn, msPerTurn)
		}
		return base * msPerTurn, nil
	case "end":
		if turn > math.MaxInt64/msPerTurn {
			return 0, errs.New(errs.EOvf, "at", eventID, "turn->ms overflow: turn=%d unit=%d", turn, msPerTurn)
		}
		v := turn * msPerTurn
		return v - 1, nil
	default:
		return 0, errs.New(errs.ESchema, "params.anchor", eventID, "unsupported anchor %q", anchor)
	}
}

// MSToTurn maps a ms timestamp to a 1-based turn serial using the declared
// integer rounding direction: floor (default), ceil or nearest.
func MSToTurn(ms, msPerTurn int64, round string) (int64, error) {
	if msPerTurn <= 0 {
		return 0, errs.New(errs.ESchema, "params.ms_per_turn", "", "must be positive, got %d", msPerTurn)
	}
	if ms < 0 {
		return 0, errs.New(errs.ESchema, "at", "", "ms must be >= 0, got %d", ms)
	}
	q := ms / msPerTurn
	r := ms % msPerTurn
	switch round {
	case "floor", "":
		return q + 1, nil
	case "ceil":
		if r > 0 {
			q++
		}
		return q + 1, nil
	case "nearest":
		if 2*r >= msPerTurn {
			q++
		}
		return q + 1, nil
	default:
		return 0, errs.New(errs.ESchema, "params.round", "", "unsupported rounding %q", round)
	}
}

// ToInternal converts a source "at" value of the given basis into internal
// millisecond units according to the declared policy.
func ToInternal(value int64, basis string, p model.Params, eventID string) (int64, error) {
	switch basis {
	case BasisMS:
		if value < 0 {
			return 0, errs.New(errs.ESchema, "at", eventID, "ms timestamp must be >= 0, got %d", value)
		}
		return value, nil
	case BasisTurn:
		return TurnToMS(value, p.MsPerTurn, p.Anchor, eventID)
	default:
		return 0, errs.New(errs.EFormat, "at_basis", eventID, "unknown time basis %q", basis)
	}
}

// RoundTripMS performs high(ms) -> low(turn) -> high(ms) using the declared
// integer policy. Samples are aligned so this is the identity on effective
// time; unaligned inputs demonstrate declared floor quantization.
func RoundTripMS(ms int64, p model.Params, eventID string) (int64, int64, error) {
	turn, err := MSToTurn(ms, p.MsPerTurn, p.Round)
	if err != nil {
		return 0, 0, err
	}
	back, err := TurnToMS(turn, p.MsPerTurn, p.Anchor, eventID)
	if err != nil {
		return 0, 0, err
	}
	return turn, back, nil
}
