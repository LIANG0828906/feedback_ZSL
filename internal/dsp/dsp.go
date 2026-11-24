package dsp

import (
	"math"
	"math/big"

	"roomreverb/internal/config"
	"roomreverb/internal/fixedpoint"
	"roomreverb/internal/model"
)

type FormulaInput struct {
	SourceLevelDB      float64
	Length             float64
	UnitsPerMeter      float64
	ReverbGain         float64
	EarlyDensity       float64
	RT60Seconds        float64
	OcclusionPerDoorDB float64
	DoorCount          int
	SampleDT           float64
	SampleCount        int
}

func distanceMeters(length, unitsPerMeter float64) float64 {
	if unitsPerMeter <= 0 {
		unitsPerMeter = 20
	}
	d := length / unitsPerMeter
	return math.Max(d, 1.0)
}

func AnalyticSPL(in FormulaInput) float64 {
	distance := distanceMeters(in.Length, in.UnitsPerMeter)
	gain := math.Max(in.ReverbGain, 1e-9)
	density := math.Max(in.EarlyDensity, 0)
	occlusion := in.OcclusionPerDoorDB * float64(in.DoorCount)
	return in.SourceLevelDB +
		20*math.Log10(gain) -
		20*math.Log10(distance) -
		occlusion +
		3*math.Log10(1+density)
}

func AnalyticCurve(in FormulaInput) []float64 {
	spl := AnalyticSPL(in)
	out := make([]float64, in.SampleCount)
	den := math.Max(float64(in.SampleCount-1), 1)
	for k := 0; k < in.SampleCount; k++ {
		early := 2 * in.EarlyDensity * (1 - float64(k)/den)
		decay := 60 * float64(k) * in.SampleDT / math.Max(in.RT60Seconds, 1e-6)
		out[k] = spl + early - decay
	}
	return out
}

type Synthesis struct {
	CurveDB []float64
	SPLDB   float64
	Traces  []model.Truncation
}

func Synthesize(in FormulaInput, profile config.PrecisionProfile, roomID, pathKey string) (*Synthesis, error) {
	traces := make([]model.Truncation, 0, in.SampleCount*8)
	engine := fixedpoint.New(profile.Decimals, &traces, roomID, pathKey)

	lengthQ := engine.FromFloat(in.Length, "length_quantize", -1)
	unitsQ := engine.FromFloat(in.UnitsPerMeter, "units_per_meter_quantize", -1)
	distanceQ := engine.Div(lengthQ, unitsQ, "distance_meters", -1)
	oneQ := engine.Q()
	if distanceQ.Cmp(oneQ) < 0 {
		distanceQ = oneQ
	}
	distanceLog := engine.Log10(distanceQ, "distance_log10", -1)
	distanceDB := engine.MulInt(distanceLog, 20)

	gainQ := engine.FromFloat(math.Max(in.ReverbGain, 1e-9), "gain_quantize", -1)
	gainLog := engine.Log10(gainQ, "gain_log10", -1)
	gainDB := engine.MulInt(gainLog, 20)

	densityQ := engine.FromFloat(math.Max(in.EarlyDensity, 0), "density_quantize", -1)
	onePlusDensity := new(big.Int).Add(engine.Q(), densityQ)
	densityLog := engine.Log10(onePlusDensity, "density_log10", -1)
	densityDB := engine.MulInt(densityLog, 3)

	occPerDoorQ := engine.FromFloat(in.OcclusionPerDoorDB, "occlusion_quantize", -1)
	occlusionDB := engine.MulInt(occPerDoorQ, int64(in.DoorCount))
	sourceLevelQ := engine.FromFloat(in.SourceLevelDB, "source_level_quantize", -1)

	base := new(big.Int).Set(sourceLevelQ)
	base.Add(base, gainDB)
	base.Sub(base, distanceDB)
	base.Sub(base, occlusionDB)
	base.Add(base, densityDB)
	spl := engine.Float(base)

	rt60Q := engine.FromFloat(in.RT60Seconds, "rt60_quantize", -1)
	dtQ := engine.FromFloat(in.SampleDT, "dt_quantize", -1)
	earlyPeakDB := engine.MulInt(densityQ, 2)

	curve := make([]float64, in.SampleCount)
	den := int64(in.SampleCount - 1)
	if den <= 0 {
		den = 1
	}
	for k := 0; k < in.SampleCount; k++ {
		kdT := engine.MulInt(dtQ, int64(k))
		sixtyKDt := engine.MulInt(kdT, 60)
		decayDB := engine.Div(sixtyKDt, rt60Q, "rt60_decay_division", k)

		remaining := big.NewInt(den - int64(k))
		if in.SampleCount <= 1 {
			remaining = big.NewInt(1)
		}
		ratioRaw := new(big.Int).Mul(remaining, engine.Scale)
		ratioQ, rem := new(big.Int).QuoRem(ratioRaw, big.NewInt(den), new(big.Int))
		if rem.Sign() != 0 {
			traces = append(traces, model.Truncation{
				Step:      "early_ratio",
				RoomID:    roomID,
				PathKey:   pathKey,
				SampleIdx: k,
				At:        "ratio",
				Raw:       ratioRaw.String(),
				Truncated: ratioQ.String(),
			})
		}
		earlyDB := engine.Mul(earlyPeakDB, ratioQ, "early_density_shape", k)

		valueQ := new(big.Int).Set(base)
		valueQ.Add(valueQ, earlyDB)
		valueQ.Sub(valueQ, decayDB)
		curve[k] = engine.Float(valueQ)
	}

	return &Synthesis{CurveDB: curve, SPLDB: spl, Traces: traces}, nil
}

func CheckTruth(result model.PathResult, truth model.PathTruth, profile config.PrecisionProfile) *model.Error {
	for i := 0; i < len(result.CurveDB) && i < len(truth.CurveDB); i++ {
		deviation := math.Abs(result.CurveDB[i] - truth.CurveDB[i])
		if deviation > profile.CurveTolDB {
			return &model.Error{
				Code:    model.CodePrec,
				Ref:     result.Key,
				Message: "decay curve sample exceeds precision tolerance",
				Details: map[string]any{
					"sample_index":       i,
					"observed_deviation": deviation,
					"tolerance":          profile.CurveTolDB,
				},
			}
		}
	}
	if len(result.CurveDB) != len(truth.CurveDB) {
		return &model.Error{
			Code:    model.CodePrec,
			Ref:     result.Key,
			Message: "curve sample count mismatch",
			Details: map[string]any{"sample_index": min(len(result.CurveDB), len(truth.CurveDB)), "observed_deviation": math.Inf(1)},
		}
	}
	splDeviation := math.Abs(result.SPLDB - truth.SPLDB)
	if splDeviation > profile.SPLTolDB {
		return &model.Error{
			Code:    model.CodePrec,
			Ref:     result.Key,
			Message: "sound pressure level exceeds precision tolerance",
			Details: map[string]any{
				"sample_index":       -1,
				"field":              "spl_db",
				"observed_deviation": splDeviation,
				"tolerance":          profile.SPLTolDB,
			},
		}
	}
	lengthDeviation := math.Abs(result.Length - truth.Length)
	if lengthDeviation > profile.LengthTol {
		return &model.Error{
			Code:    model.CodeDiff,
			Ref:     result.Key,
			Message: "path length differs from truth beyond tolerance",
			Details: map[string]any{"field": "length", "observed_deviation": lengthDeviation, "tolerance": profile.LengthTol},
		}
	}
	if !equalStrings(result.RoomChain, truth.RoomChain) {
		return &model.Error{
			Code:    model.CodeDiff,
			Ref:     result.Key,
			Message: "room chain differs from truth",
			Details: map[string]any{"field": "room_chain", "expected": truth.RoomChain, "actual": result.RoomChain},
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
