package engine

import (
	"sort"

	"fixedsampler/internal/fixedpoint"
	"fixedsampler/internal/model"
)

type Observation struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

type BoundReport struct {
	StageSeq        int           `json:"stage_seq"`
	Category        string        `json:"category"`
	LocalBoundFX    int64         `json:"local_bound_fixed"`
	GainNumerator   int64         `json:"gain_numerator"`
	GainDenominator int64         `json:"gain_denominator"`
	CumulativeFX    int64         `json:"cumulative_fixed"`
	Dependencies    []Observation `json:"dependencies"`
}

type CumulativePoint struct {
	Seq             int   `json:"seq"`
	WeightNormFX    int64 `json:"weight_norm_fixed"`
	AccumTruncFX    int64 `json:"accum_trunc_fixed"`
	ColorQuantFX    int64 `json:"color_quant_fixed"`
	BitDepthErrorFX int64 `json:"bit_depth_error_fixed"`
}

type ErrorReport struct {
	ProjectID           string                     `json:"project_id"`
	ConfigFingerprint   string                     `json:"config_fingerprint"`
	AlgorithmVersion    string                     `json:"algorithm_version"`
	ParamVersion        string                     `json:"param_version"`
	Workers             int                        `json:"workers"`
	PerThreadBuffer     int                        `json:"per_thread_buffer"`
	ChunkSize           int                        `json:"chunk_size"`
	ChunkCount          int                        `json:"chunk_count"`
	RecomputedNodeCount int                        `json:"recomputed_node_count"`
	ReusedNodeCount     int                        `json:"reused_node_count"`
	QueueWaitCount      int                        `json:"queue_wait_count"`
	QueueEvents         []model.QueueEvent         `json:"queue_events"`
	CommitSequence      []int                      `json:"commit_sequence"`
	Invalidations       []model.InvalidationRecord `json:"invalidations"`
	ResidualStats       []model.ResidualStat       `json:"residual_stats"`
	Truncations         []model.TruncRecord        `json:"truncations"`
	Bounds              []BoundReport              `json:"bounds"`
	Cumulative          []CumulativePoint          `json:"cumulative_bounds"`
}

func BuildErrorReport(p *model.Project, res *RunResult, workers, buffer int) *ErrorReport {
	report := &ErrorReport{
		ProjectID: p.ProjectID, ConfigFingerprint: res.ConfigFP,
		AlgorithmVersion: model.AlgoVersion, ParamVersion: model.ParamVersion,
		Workers: workers, PerThreadBuffer: buffer, ChunkSize: res.Grid.ChunkSize,
		ChunkCount: res.Grid.Count(), QueueEvents: append([]model.QueueEvent(nil), res.QueueEvents...),
		QueueWaitCount: len(res.QueueEvents), CommitSequence: append([]int(nil), res.CommitSequence...),
		Invalidations: append([]model.InvalidationRecord(nil), res.Invalidations...),
	}
	report.collectTruncations(res)
	report.ResidualStats = aggregateResiduals(report.Truncations, p.ValueScale)

	report.RecomputedNodeCount = len(res.Invalidations)
	totalNodes := len(p.Config) * res.Grid.Count()
	report.ReusedNodeCount = totalNodes - report.RecomputedNodeCount

	bits := map[string]int{}
	for _, pl := range p.Planes {
		bits[pl.Name] = pl.BitDepth
	}
	cum := [4]int64{}
	for seq, stage := range p.Config {
		seq++
		gainNum, gainDen := stageGain(stage)
		for i := range cum {
			cum[i] = ceilMul(cum[i], gainNum, gainDen)
		}
		switch stage.Type {
		case "resample":
			maxBits := 0
			for _, name := range stage.Planes {
				maxBits = max(maxBits, bits[name])
			}
			taps := int64(stage.Taps)
			if taps == 0 {
				if stage.Kernel == "nearest" {
					taps = 1
				} else {
					taps = int64(stage.FactorX * stage.FactorY)
				}
			}
			vMax := fixedpoint.MaxCode(maxBits) * p.ValueScale
			weightLocal := ceilDiv(vMax*taps, stage.FixedScale)
			accumLocal := int64(1)
			normObs := maxObservedNormResidual(report.ResidualStats, seq)
			cum[0] += weightLocal
			cum[1] += accumLocal
			report.Bounds = append(report.Bounds,
				BoundReport{StageSeq: seq, Category: "resample_weight_normalization", LocalBoundFX: weightLocal, GainNumerator: gainNum, GainDenominator: gainDen, CumulativeFX: cum[0],
					Dependencies: []Observation{
						{Name: "value_scale", Value: p.ValueScale},
						{Name: "weight_fixed_scale", Value: stage.FixedScale},
						{Name: "factor_x", Value: stage.FactorX},
						{Name: "factor_y", Value: stage.FactorY},
						{Name: "tap_count", Value: taps},
						{Name: "input_bit_depth_max", Value: maxBits},
						{Name: "input_max_fixed", Value: vMax},
						{Name: "observed_norm_residual_max", Value: normObs},
					}},
				BoundReport{StageSeq: seq, Category: "downsample_accumulation_truncation", LocalBoundFX: accumLocal, GainNumerator: gainNum, GainDenominator: gainDen, CumulativeFX: cum[1],
					Dependencies: []Observation{
						{Name: "weight_fixed_scale", Value: stage.FixedScale},
						{Name: "factor_x", Value: stage.FactorX},
						{Name: "factor_y", Value: stage.FactorY},
						{Name: "tap_count", Value: taps},
						{Name: "truncation_divisor", Value: stage.FixedScale},
					}})
		case "color":
			local := int64(1)
			cum[2] += local
			rowL1 := matrixRowL1(stage)
			report.Bounds = append(report.Bounds, BoundReport{
				StageSeq: seq, Category: "color_conversion_quantization", LocalBoundFX: local,
				GainNumerator: gainNum, GainDenominator: gainDen, CumulativeFX: cum[2],
				Dependencies: []Observation{
					{Name: "matrix_fixed_scale", Value: stage.Matrix.Scale},
					{Name: "max_row_abs_sum", Value: rowL1},
					{Name: "source_space", Value: stage.SourceSpace},
					{Name: "target_space", Value: stage.TargetSpace},
					{Name: "component_divisions", Value: 3 * res.Grid.Count()},
				},
			})
			firstBits := bits[stage.InputNames[0]]
			for _, out := range stage.OutputNames {
				bits[out] = firstBits
			}
		case "quantize":
			local := (p.ValueScale + 1) / 2
			maxFromBits := 0
			for _, name := range stage.Planes {
				maxFromBits = max(maxFromBits, bits[name])
				bits[name] = stage.BitDepth
			}
			cum[3] += local
			report.Bounds = append(report.Bounds, BoundReport{
				StageSeq: seq, Category: "bit_depth_rounding", LocalBoundFX: local,
				GainNumerator: gainNum, GainDenominator: gainDen, CumulativeFX: cum[3],
				Dependencies: []Observation{
					{Name: "value_scale", Value: p.ValueScale},
					{Name: "input_bit_depth_max", Value: maxFromBits},
					{Name: "output_bit_depth", Value: stage.BitDepth},
					{Name: "input_max_code", Value: fixedpoint.MaxCode(maxFromBits)},
					{Name: "output_max_code", Value: fixedpoint.MaxCode(stage.BitDepth)},
				},
			})
		}
		report.Cumulative = append(report.Cumulative, CumulativePoint{
			Seq: seq, WeightNormFX: cum[0], AccumTruncFX: cum[1], ColorQuantFX: cum[2], BitDepthErrorFX: cum[3],
		})
	}
	return report
}

func (r *ErrorReport) collectTruncations(res *RunResult) {
	for idx := 0; idx < res.Grid.Count(); idx++ {
		chunk := res.Artifacts[idx]
		if chunk == nil {
			continue
		}
		for _, level := range chunk.Levels {
			r.Truncations = append(r.Truncations, level.Truncations...)
		}
	}
}

type statKey struct {
	seq      int
	category string
	plane    string
}

func aggregateResiduals(records []model.TruncRecord, valueScale int64) []model.ResidualStat {
	type agg struct {
		stat     model.ResidualStat
		milliSum int64
		milliMax int64
		fixedSum int64
	}
	groups := map[statKey]*agg{}
	var keys []statKey
	for _, rec := range records {
		category := categoryForKind(rec.Kind)
		key := statKey{seq: rec.StageSeq, category: category, plane: rec.Plane}
		g := groups[key]
		if g == nil {
			g = &agg{stat: model.ResidualStat{StageSeq: rec.StageSeq, Category: category, Plane: rec.Plane}}
			groups[key] = g
			keys = append(keys, key)
		}
		g.stat.Count++
		switch rec.Kind {
		case "weight_normalization":
			g.stat.Unit = "integer_weight_scale_unit"
			g.stat.MaxAbs = max(g.stat.MaxAbs, fixedpoint.Abs(rec.Remainder))
			g.stat.SumAbs += fixedpoint.Abs(rec.Remainder)
			g.stat.MeanMilli = safeMilliWeight(g.stat.SumAbs, rec.Denominator, g.stat.Count)
		case "bitdepth_round":
			g.stat.Unit = "fixed_unit"
			d := rec.Remainder
			if 2*d >= rec.Denominator {
				d = rec.Denominator - d
			}
			errFX := ceilDiv(d*valueScale, rec.Denominator)
			milli := ceilDiv(d*valueScale*1000, rec.Denominator)
			g.fixedSum += errFX
			g.milliSum += milli
			g.milliMax = max(g.milliMax, milli)
			g.stat.MaxAbs = max(g.stat.MaxAbs, errFX)
			g.stat.SumAbs = g.fixedSum
			g.stat.MeanMilli = g.milliSum / int64(g.stat.Count)
			g.stat.MaxAbsMilli = g.milliMax
		default:
			g.stat.Unit = "milli_fixed_unit"
			milli := ceilDiv(rec.Remainder*1000, rec.Denominator)
			g.milliSum += milli
			g.milliMax = max(g.milliMax, milli)
			g.stat.MaxAbs = g.milliMax
			g.stat.SumAbs = g.milliSum
			g.stat.MeanMilli = g.milliSum / int64(g.stat.Count)
			g.stat.MaxAbsMilli = g.milliMax
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].seq != keys[j].seq {
			return keys[i].seq < keys[j].seq
		}
		if keys[i].category != keys[j].category {
			return keys[i].category < keys[j].category
		}
		return keys[i].plane < keys[j].plane
	})
	out := make([]model.ResidualStat, 0, len(keys))
	for _, key := range keys {
		out = append(out, groups[key].stat)
	}
	return out
}

func categoryForKind(kind string) string {
	switch kind {
	case "weight_normalization":
		return "resample_weight_normalization"
	case "downsample_accum":
		return "downsample_accumulation_truncation"
	case "color_quant":
		return "color_conversion_quantization"
	case "bitdepth_round":
		return "bit_depth_rounding"
	default:
		return kind
	}
}

func safeMilliWeight(sumResidual, denominator int64, count int) int64 {
	if count == 0 || denominator == 0 {
		return 0
	}
	return sumResidual * 1000 / (denominator * int64(count))
}

func maxObservedNormResidual(stats []model.ResidualStat, seq int) int64 {
	var m int64
	for _, st := range stats {
		if st.StageSeq == seq && st.Category == "resample_weight_normalization" {
			m = max(m, st.MaxAbs)
		}
	}
	return m
}

func stageGain(stage model.StageConfig) (int64, int64) {
	switch stage.Type {
	case "resample":
		return 1, 1
	case "color":
		l1 := matrixRowL1(stage)
		if l1 < stage.Matrix.Scale {
			l1 = stage.Matrix.Scale
		}
		return l1, stage.Matrix.Scale
	default:
		return 1, 1
	}
}

func matrixRowL1(stage model.StageConfig) int64 {
	if stage.Matrix == nil {
		return 1
	}
	var worst int64
	for _, row := range stage.Matrix.Rows {
		var sum int64
		for _, v := range row {
			sum += fixedpoint.Abs(v)
		}
		worst = max(worst, sum)
	}
	return worst
}

func ceilDiv(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func ceilMul(v, num, den int64) int64 {
	if v == 0 || num == 0 {
		return 0
	}
	return ceilDiv(v*num, den)
}
