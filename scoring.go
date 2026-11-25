package thermalcache

import (
	"math"
	"math/bits"
)

type DecisionDirection string

const (
	DirectionPromote DecisionDirection = "promote"
	DirectionDemote  DecisionDirection = "demote"
	DirectionHold    DecisionDirection = "hold"
)

// ReasonCode is the stable numeric rationale emitted with every decision.
type ReasonCode int

const (
	ReasonHoldHysteresis  ReasonCode = 1
	ReasonPromoteNormal   ReasonCode = 2
	ReasonPromoteBoundary ReasonCode = 3
	ReasonDemoteNormal    ReasonCode = 4
	ReasonDemoteBoundary  ReasonCode = 5
	ReasonTTLExpired      ReasonCode = 6
	ReasonMigrating       ReasonCode = 7
	ReasonInstruction     ReasonCode = 8
)

func (r ReasonCode) String() string {
	switch r {
	case ReasonHoldHysteresis:
		return "R_HOLD_HYSTERESIS"
	case ReasonPromoteNormal:
		return "R_PROMOTE_THRESHOLD"
	case ReasonPromoteBoundary:
		return "R_PROMOTE_BOUNDARY"
	case ReasonDemoteNormal:
		return "R_DEMOTE_THRESHOLD"
	case ReasonDemoteBoundary:
		return "R_DEMOTE_BOUNDARY"
	case ReasonTTLExpired:
		return "R_TTL_EXPIRED"
	case ReasonMigrating:
		return "R_MIGRATION_HOLD"
	case ReasonInstruction:
		return "R_INSTRUCTION"
	default:
		return "R_UNKNOWN"
	}
}

type ScoreResult struct {
	Score      int64 `json:"score"`
	RawScore   int64 `json:"raw_score"`
	AgeMs      int64 `json:"age_ms"`
	Expired    bool  `json:"expired"`
	Saturated  bool  `json:"saturated"`
	Overflowed bool  `json:"overflowed"`
}

type Decision struct {
	Direction DecisionDirection `json:"direction"`
	Desired   Tier              `json:"desired_tier"`
	Reason    ReasonCode        `json:"reason"`
	ReasonID  string            `json:"reason_id"`
	Threshold int64             `json:"threshold"`
	Boundary  bool              `json:"boundary"`
}

func signedMax(bits int) int64 {
	if bits >= 63 {
		return math.MaxInt64
	}
	return int64(uint64(1)<<(bits-1)) - 1
}

func saturatingAddUnsigned(a, b, limit int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, true
	}
	if a > limit-b {
		return limit, true
	}
	return a + b, false
}

func saturatingMulUnsigned(a, b, limit int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, true
	}
	if a == 0 || b == 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo > uint64(limit) {
		return limit, true
	}
	return int64(lo), false
}

// ComputeScore evaluates the declared fixed-point heat waterline. The
// arithmetic never overflows: every overflow is clipped to the signed maximum
// for ScoreConfig.Bits and reported through Saturated/Overflowed.
func ComputeScore(cfg ScoreConfig, accessTotal, lastAccessMs, nowMs, ttlMs, tickMs int64) ScoreResult {
	max := signedMax(cfg.Bits)
	out := ScoreResult{}
	if nowMs < lastAccessMs {
		out.AgeMs = 0
	} else {
		out.AgeMs = nowMs - lastAccessMs
	}
	if ttlMs > 0 && nowMs-lastAccessMs > ttlMs {
		out.Expired = true
		return out
	}
	if accessTotal < 0 {
		accessTotal = 0
	}
	if cfg.CountWeight > 0 {
		accessTotal, out.Saturated = saturatingMulUnsigned(accessTotal, cfg.CountWeight, max)
		out.Overflowed = out.Overflowed || out.Saturated
	}
	accessTotal, saturated := saturatingMulUnsigned(accessTotal, cfg.Scale, max)
	out.Saturated = out.Saturated || saturated
	out.Overflowed = out.Overflowed || saturated

	ageSteps := int64(0)
	if tickMs > 0 && out.AgeMs > 0 {
		ageSteps = out.AgeMs / tickMs
	}
	denom := ageSteps + 1
	if denom <= 0 {
		denom = 1
		out.Saturated = true
		out.Overflowed = true
	}
	out.RawScore = accessTotal
	out.Score = accessTotal / denom
	if out.Score > max {
		out.Score = max
		out.Saturated = true
		out.Overflowed = true
	}
	if out.Score < 0 {
		out.Score = 0
		out.Saturated = true
		out.Overflowed = true
	}
	return out
}

func Decide(cfg ScoreConfig, current Tier, score ScoreResult, watermark *int64) Decision {
	if score.Expired {
		return Decision{
			Direction: DirectionDemote, Desired: TierCold,
			Reason: ReasonTTLExpired, ReasonID: ReasonTTLExpired.String(),
		}
	}
	promoteThreshold := cfg.PromoteThreshold
	if watermark != nil {
		promoteThreshold = *watermark
	}
	if current == TierCold {
		promote := score.Score > promoteThreshold
		if cfg.Boundary == PromoteAtBoundary {
			promote = score.Score >= promoteThreshold
		}
		if promote {
			reason := ReasonPromoteNormal
			boundary := score.Score == promoteThreshold
			if boundary {
				reason = ReasonPromoteBoundary
			}
			return Decision{
				Direction: DirectionPromote, Desired: TierHot,
				Reason: reason, ReasonID: reason.String(),
				Threshold: promoteThreshold, Boundary: boundary,
			}
		}
		return Decision{
			Direction: DirectionHold, Desired: TierCold,
			Reason: ReasonHoldHysteresis, ReasonID: ReasonHoldHysteresis.String(),
			Threshold: promoteThreshold,
		}
	}
	demote := score.Score <= cfg.DemoteThreshold
	if cfg.Boundary == DemoteAtBoundary {
		demote = score.Score <= cfg.DemoteThreshold
	}
	if demote {
		reason := ReasonDemoteNormal
		boundary := score.Score == cfg.DemoteThreshold
		if boundary {
			reason = ReasonDemoteBoundary
		}
		return Decision{
			Direction: DirectionDemote, Desired: TierCold,
			Reason: reason, ReasonID: reason.String(),
			Threshold: cfg.DemoteThreshold, Boundary: boundary,
		}
	}
	return Decision{
		Direction: DirectionHold, Desired: TierHot,
		Reason: ReasonHoldHysteresis, ReasonID: ReasonHoldHysteresis.String(),
		Threshold: cfg.DemoteThreshold,
	}
}
