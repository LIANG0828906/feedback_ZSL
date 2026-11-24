package acoustics

import (
	"levelacoustic/internal/fixed"
	"levelacoustic/internal/model"
)

const (
	BaseSPLDBMicro       int64 = 94 * fixed.Scale
	DoorOcclusionDBMicro int64 = 1_500_000
)

type ChainInput struct {
	Rooms           []model.Room
	ListenerRoom    model.Room
	DoorCount       int
	PathLengthMicro int64
	CurveSamples    int
	CurveStepMicros int64
}

type Score struct {
	CurveDBMicro     []int64
	SPLDBMicro       int64
	OcclusionDBMicro int64
	Truncations      []model.Truncation
}

type Prepared struct {
	SPLDBMicro       int64
	OcclusionDBMicro int64
	RT60Micros       int64
	CurveSamples     int
	CurveStepMicros  int64
	Truncations      []model.Truncation
}

// AnalyticScore is the single documented formula used both by the generator's
// analytic truth and by the solver. No solver output is ever fed back into it.
//
//	gainDB    = average(20*log10(room.gain))
//	densityDB = average(10*log10(1+room.density))
//	distance  = 20*log10(max(path,reference)/reference)
//	occlusion = 1.5 dB * crossed doors
//	L(t)      = 94 + gainDB + densityDB - distance - occlusion - 60*t/RT60(listener room)
func AnalyticScore(input ChainInput) Score {
	prepared := Prepare(input)
	curve, curveTruncations := prepared.Render(0, prepared.CurveSamples)
	truncations := append(append([]model.Truncation(nil), prepared.Truncations...), curveTruncations...)
	return Score{
		CurveDBMicro:     curve,
		SPLDBMicro:       prepared.SPLDBMicro,
		OcclusionDBMicro: prepared.OcclusionDBMicro,
		Truncations:      truncations,
	}
}

func Prepare(input ChainInput) Prepared {
	truncations := make([]model.Truncation, 0, len(input.Rooms)*3+input.CurveSamples+3)
	gains := make([]int64, 0, len(input.Rooms))
	for i, room := range input.Rooms {
		v, rem := fixed.GainDBMicro(room.ReverbGainMilli)
		gains = append(gains, v)
		truncations = append(truncations, model.Truncation{Stage: "room_gain_log", Step: i, Remainder: rem, Fixed: v})
	}
	densities := make([]int64, 0, len(input.Rooms))
	for i, room := range input.Rooms {
		v, rem := fixed.DensityBoostDBMicro(room.EarlyDensityMilli)
		densities = append(densities, v)
		truncations = append(truncations, model.Truncation{Stage: "room_density_log", Step: i, Remainder: rem, Fixed: v})
	}
	avgGain, gainRem := fixed.Average(gains)
	truncations = append(truncations, model.Truncation{Stage: "gain_average", Step: len(gains), Remainder: gainRem, Fixed: avgGain})
	avgDensity, densityRem := fixed.Average(densities)
	truncations = append(truncations, model.Truncation{Stage: "density_average", Step: len(densities), Remainder: densityRem, Fixed: avgDensity})
	distanceLoss, distanceRem := fixed.DistanceLossDBMicro(input.PathLengthMicro)
	truncations = append(truncations, model.Truncation{Stage: "distance_log", Step: 0, Remainder: distanceRem, Fixed: distanceLoss})

	occlusion := int64(input.DoorCount) * DoorOcclusionDBMicro
	spl := BaseSPLDBMicro + avgGain + avgDensity - distanceLoss - occlusion
	rt60Micros := input.ListenerRoom.RT60Milliseconds * 1000
	return Prepared{
		SPLDBMicro:       spl,
		OcclusionDBMicro: occlusion,
		RT60Micros:       rt60Micros,
		CurveSamples:     input.CurveSamples,
		CurveStepMicros:  input.CurveStepMicros,
		Truncations:      truncations,
	}
}

func (p Prepared) Render(start, count int) ([]int64, []model.Truncation) {
	if start < 0 {
		start = 0
	}
	if count < 0 {
		count = 0
	}
	if start+count > p.CurveSamples {
		count = p.CurveSamples - start
	}
	if count <= 0 {
		return nil, nil
	}
	curve := make([]int64, count)
	truncations := make([]model.Truncation, 0, count)
	for i := 0; i < count; i++ {
		index := start + i
		sampleTime := int64(index) * p.CurveStepMicros
		drop, rem := fixed.DecayDropDBMicro(sampleTime, p.RT60Micros)
		curve[i] = p.SPLDBMicro - drop
		truncations = append(truncations, model.Truncation{Stage: "decay_drop", Step: index, Remainder: rem, Fixed: drop})
	}
	return curve, truncations
}

func (p Prepared) RenderInto(dst []int64, start int) []model.Truncation {
	if start < 0 || start >= p.CurveSamples || len(dst) == 0 {
		return nil
	}
	count := len(dst)
	if start+count > p.CurveSamples {
		count = p.CurveSamples - start
	}
	truncations := make([]model.Truncation, 0, count)
	for i := 0; i < count; i++ {
		index := start + i
		sampleTime := int64(index) * p.CurveStepMicros
		drop, rem := fixed.DecayDropDBMicro(sampleTime, p.RT60Micros)
		dst[i] = p.SPLDBMicro - drop
		truncations = append(truncations, model.Truncation{Stage: "decay_drop", Step: index, Remainder: rem, Fixed: drop})
	}
	return truncations
}

func ChainAverageParams(chain []string, rooms map[string]model.Room) ([]model.Room, error) {
	out := make([]model.Room, 0, len(chain))
	for _, id := range chain {
		room, ok := rooms[id]
		if !ok {
			return nil, model.GeometryError(id, "", "room_chain", "unknown room %s in chain", id)
		}
		out = append(out, room)
	}
	return out, nil
}
