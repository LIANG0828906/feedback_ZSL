package macrokit

import (
	"math"
)

// ConvertFramesToMS converts frame durations using integer arithmetic.
//
// At 60 FPS the floor/ceil pair is deliberately asymmetric: frame->ms uses
// floor and ms->frame uses ceil. For frame rates not greater than 1000, this
// preserves integer frame counts on a frame -> ms -> frame round trip.
func ConvertFramesToMS(frames int64, frameRate int, rounding RoundingDirection) (int64, error) {
	if frames < 0 {
		return 0, newSchemaError("", "frame duration must be non-negative")
	}
	if frameRate <= 0 {
		return 0, newSchemaError("/frame_rate", "frame_rate must be positive")
	}
	if frames > math.MaxInt64/1000 {
		return 0, newOvfError("", "/time", "frames", frames)
	}
	product := frames * 1000
	quotient := product / int64(frameRate)
	remainder := product % int64(frameRate)
	if rounding == RoundCeil && remainder != 0 {
		if quotient == math.MaxInt64 {
			return 0, newOvfError("", "/time", "frames", frames)
		}
		quotient++
	}
	return quotient, nil
}

// ConvertMSToFrames converts milliseconds to frames without floating point.
func ConvertMSToFrames(ms int64, frameRate int, rounding RoundingDirection) (int64, error) {
	if ms < 0 {
		return 0, newSchemaError("", "millisecond duration must be non-negative")
	}
	if frameRate <= 0 {
		return 0, newSchemaError("/frame_rate", "frame_rate must be positive")
	}
	rate := int64(frameRate)
	if ms > math.MaxInt64/rate {
		return 0, newOvfError("", "/time", "ms", ms)
	}
	product := ms * rate
	quotient := product / 1000
	remainder := product % 1000
	if rounding == RoundCeil && remainder != 0 {
		if quotient == math.MaxInt64 {
			return 0, newOvfError("", "/time", "ms", ms)
		}
		quotient++
	}
	return quotient, nil
}

func framesToMSForBranch(branchID string, frames int64, rate int, rounding RoundingDirection, path string) (int64, error) {
	v, err := ConvertFramesToMS(frames, rate, rounding)
	if err != nil {
		if domainErr, ok := AsError(err); ok {
			domainErr.BranchID = branchID
			if domainErr.Path == "" || domainErr.Path == "/time" {
				domainErr.Path = path
			}
			return 0, domainErr
		}
	}
	return v, err
}

func msToFramesForBranch(branchID string, ms int64, rate int, rounding RoundingDirection, path string) (int64, error) {
	v, err := ConvertMSToFrames(ms, rate, rounding)
	if err != nil {
		if domainErr, ok := AsError(err); ok {
			domainErr.BranchID = branchID
			domainErr.Path = path
			return 0, domainErr
		}
	}
	return v, err
}

// RoundTripFrames verifies the supported integer frame -> target unit -> frame
// conversion. It is intentionally explicit so callers cannot mistake lossy
// arbitrary millisecond values for exact frame boundaries.
func RoundTripFrames(frames int64, rate int) (int64, int64, error) {
	ms, err := ConvertFramesToMS(frames, rate, RoundFloor)
	if err != nil {
		return 0, 0, err
	}
	back, err := ConvertMSToFrames(ms, rate, RoundCeil)
	if err != nil {
		return ms, 0, err
	}
	return ms, back, nil
}
