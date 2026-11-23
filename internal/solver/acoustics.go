package solver

import (
	"roomreverb/internal/fixed"
	"roomreverb/internal/model"
)

type AcousticEstimate struct {
	Decay  fixed.Value
	Energy fixed.Value
}

// Estimate derives decay time and early-reflection energy entirely in Q16.16.
// Decay is sound travel time plus the arithmetic mean target T60 of the rooms
// on the portal chain. Early energy averages densities/gains and applies a
// distance attenuation of 1000/(1000+path_mm). Therefore tightening any T60 on
// the chain can never increase decay, and increasing path length can never
// increase early energy.
func Estimate(level model.Level, roomPath []string, path fixed.Value) AcousticEstimate {
	if len(roomPath) == 0 {
		return AcousticEstimate{}
	}
	var t60sum fixed.Value
	var densitySum fixed.Value
	var gainSum fixed.Value
	var found int64
	for _, id := range roomPath {
		r, ok := level.Room(id)
		if !ok {
			continue
		}
		t60sum += fixed.FromFloat(r.Params.T60MS)
		densitySum += fixed.FromFloat(r.Params.EarlyDensity)
		gainSum += fixed.FromFloat(r.Params.ReverbGain)
		found++
	}
	if found == 0 {
		return AcousticEstimate{}
	}
	avgT60 := fixed.DivInt(t60sum, found)
	avgDensity := fixed.DivInt(densitySum, found)
	avgGain := fixed.DivInt(gainSum, found)
	travelMS := fixed.DivInt(fixed.MulInt(path, 1000), model.SpeedOfSoundMMPS)
	decay := travelMS + avgT60

	unitEnergy := fixed.Mul(avgDensity, avgGain)
	numerator := fixed.MulInt(unitEnergy, 1000)
	denominator := fixed.FromInt(1000) + path
	energy := fixed.Div(numerator, denominator)
	return AcousticEstimate{Decay: decay, Energy: energy}
}
