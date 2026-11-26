// Package quant implements the adaptive integer-only spectral quantizer.
package quant

import (
	"fmt"
	"sort"

	"msworkbench/internal/conv"
	"msworkbench/internal/domain"
	"msworkbench/internal/e"
	"msworkbench/internal/sample"
)

type Method string

const (
	MethodExact Method = "sparse_pixel_exact"
	MethodRow   Method = "medium_row_accum_recompute"
	MethodLUT   Method = "dense_block_lut"
)

type Thresholds struct {
	SparseMaxObservations int   `json:"sparse_max_observations"`
	MediumMaxObservations int   `json:"medium_max_observations"`
	LUTStepRadiance       int64 `json:"lut_step_radiance"`
}

func DefaultThresholds() Thresholds {
	return Thresholds{SparseMaxObservations: 20, MediumMaxObservations: 40, LUTStepRadiance: 4}
}

func (t Thresholds) Normalize() Thresholds {
	d := DefaultThresholds()
	if t.SparseMaxObservations <= 0 {
		t.SparseMaxObservations = d.SparseMaxObservations
	}
	if t.MediumMaxObservations <= 0 {
		t.MediumMaxObservations = d.MediumMaxObservations
	}
	if t.MediumMaxObservations < t.SparseMaxObservations {
		t.MediumMaxObservations = t.SparseMaxObservations
	}
	if t.LUTStepRadiance <= 0 {
		t.LUTStepRadiance = d.LUTStepRadiance
	}
	return t
}

type Decision struct {
	Block            string             `json:"block"`
	Profile          domain.SampleScale `json:"profile"`
	ObservationCount int                `json:"observation_count"`
	SparseThreshold  int                `json:"sparse_threshold"`
	MediumThreshold  int                `json:"medium_threshold"`
	Method           Method             `json:"method"`
	Reason           string             `json:"reason"`
	LUTStepRadiance  int64              `json:"lut_step_radiance,omitempty"`
}

func Select(profile domain.SampleScale, observationCount int, t Thresholds) Decision {
	t = t.Normalize()
	d := Decision{
		Profile: profile, ObservationCount: observationCount,
		SparseThreshold: t.SparseMaxObservations, MediumThreshold: t.MediumMaxObservations,
		LUTStepRadiance: t.LUTStepRadiance,
	}
	switch {
	case observationCount <= t.SparseMaxObservations:
		d.Method = MethodExact
		d.Reason = fmt.Sprintf("observations %d <= sparse threshold %d; full per-pixel integer division is affordable", observationCount, t.SparseMaxObservations)
	case observationCount <= t.MediumMaxObservations:
		d.Method = MethodRow
		d.Reason = fmt.Sprintf("sparse threshold exceeded but %d <= medium threshold %d; accumulate rows and recompute block root", observationCount, t.MediumMaxObservations)
	default:
		d.Method = MethodLUT
		d.Reason = fmt.Sprintf("observations %d > medium threshold %d; use block accumulation and bounded LUT", observationCount, t.MediumMaxObservations)
	}
	return d
}

type BandResult struct {
	BandID          string  `json:"band_id"`
	Values          []int64 `json:"values"`
	Remainders      []int64 `json:"remainders"`
	Saturated       []bool  `json:"saturated"`
	MaxRemainder    int64   `json:"max_remainder"`
	ResidualSum     int64   `json:"residual_sum"`
	ErrorBoundQ     int64   `json:"error_bound_q"`
	LUTHits         int     `json:"lut_hits,omitempty"`
	LUTBoundaryMiss int     `json:"lut_boundary_misses,omitempty"`
}

type RowAccumulation struct {
	LocalRow     int   `json:"local_row"`
	PixelCount   int   `json:"pixel_count"`
	RadianceSum  int64 `json:"radiance_sum"`
	RemainderSum int64 `json:"remainder_sum"`
}

type BoundaryCheck struct {
	RowCount               int   `json:"row_count"`
	RowRadianceSum         int64 `json:"row_radiance_sum"`
	RecomputedRadianceSum  int64 `json:"recomputed_radiance_sum"`
	RowRemainderSum        int64 `json:"row_remainder_sum"`
	RecomputedRemainderSum int64 `json:"recomputed_remainder_sum"`
	Match                  bool  `json:"match"`
}

type Result struct {
	Coord              domain.BlockCoord      `json:"coord"`
	Decision           Decision               `json:"decision"`
	Bands              map[string]*BandResult `json:"bands"`
	RowAccumulations   []RowAccumulation      `json:"row_accumulations,omitempty"`
	BoundaryCheck      *BoundaryCheck         `json:"boundary_check,omitempty"`
	BlockRadianceSum   int64                  `json:"block_radiance_sum"`
	BoundaryRecomputed bool                   `json:"boundary_recomputed"`
	InputHash          string                 `json:"input_hash"`
	QuantHash          string                 `json:"quant_hash"`
}

func QuantizeValue(radiance int64, band domain.Band, p *domain.Project, rec *conv.Recorder) (q int64, rem int64, saturated bool, err error) {
	field := fmt.Sprintf("project.bands[%s].gain_ppm", band.ID)
	adjusted, err := conv.ApplyGain(radiance, band.GainPPM, field, rec)
	if err != nil {
		return 0, 0, false, err
	}
	dose, err := conv.CheckedMul(adjusted, p.ReferenceExpUS, "quantization.dose")
	if err != nil {
		return 0, 0, false, err
	}
	raw, remainder, err := conv.MulDivFloor(dose, p.QuantScale, p.FullWellDose, "quantization.value", rec, "quant_floor")
	if err != nil {
		return 0, 0, false, err
	}
	if raw > p.QuantScale {
		return p.QuantScale, remainder, true, nil
	}
	return raw, remainder, false, nil
}

func QuantizeBlock(p *domain.Project, set *sample.SampleSet, coord domain.BlockCoord, decision Decision, thresholds Thresholds) (*Result, error) {
	thresholds = thresholds.Normalize()
	indices, _, localY := p.BlockPixels(coord)
	inputHash := set.BlockHashes[coord.String()]
	if inputHash == "" {
		t := set.Truth[coord.String()]
		if t == nil {
			return nil, fmt.Errorf("missing generated truth for block %s", coord)
		}
		inputHash = t.InputHash
	}
	out := &Result{
		Coord: coord, Decision: decision, Bands: map[string]*BandResult{}, InputHash: inputHash,
	}
	planeByBand := map[string][]int64{}
	for _, pl := range set.Radiance {
		planeByBand[pl.BandID] = pl.Values
	}

	for _, band := range p.Bands {
		plane := planeByBand[band.ID]
		if plane == nil {
			return nil, fmt.Errorf("sample plane for band %s not found", band.ID)
		}
		br := &BandResult{Values: make([]int64, len(indices)), Remainders: make([]int64, len(indices)), Saturated: make([]bool, len(indices))}
		lut := newBoundLUT(band, p, thresholds.LUTStepRadiance)
		for li, gi := range indices {
			r := plane[gi]
			out.BlockRadianceSum += r
			q, rem, sat, lutHit, boundary, err := quantizeOneAdaptive(r, band, p, decision.Method, lut)
			if err != nil {
				return nil, err
			}
			br.Values[li] = q
			br.Remainders[li] = rem
			br.Saturated[li] = sat
			br.ResidualSum += rem
			if rem > br.MaxRemainder {
				br.MaxRemainder = rem
			}
			if lutHit {
				br.LUTHits++
			}
			if boundary {
				br.LUTBoundaryMiss++
			}
		}
		bound, err := errorBound(band, p, decision.Method, thresholds)
		if err != nil {
			return nil, err
		}
		br.ErrorBoundQ = bound
		out.Bands[band.ID] = br
	}

	if decision.Method == MethodRow {
		rowMap := map[int]*RowAccumulation{}
		rowOrder := []int{}
		for li := range indices {
			ry := localY[li] - coord.BY*p.BlockHeight
			row, ok := rowMap[ry]
			if !ok {
				row = &RowAccumulation{LocalRow: ry}
				rowMap[ry] = row
				rowOrder = append(rowOrder, ry)
			}
			row.PixelCount++
			for _, br := range out.Bands {
				row.RemainderSum += br.Remainders[li]
			}
			for bi := range p.Bands {
				row.RadianceSum += set.Radiance[bi].Values[indices[li]]
			}
		}
		sort.Ints(rowOrder)
		check := &BoundaryCheck{RowCount: len(rowOrder)}
		for _, ry := range rowOrder {
			ra := *rowMap[ry]
			out.RowAccumulations = append(out.RowAccumulations, ra)
			check.RowRadianceSum += ra.RadianceSum
			check.RowRemainderSum += ra.RemainderSum
		}
		for bi, band := range p.Bands {
			for _, gi := range indices {
				check.RecomputedRadianceSum += set.Radiance[bi].Values[gi]
			}
			check.RecomputedRemainderSum += out.Bands[band.ID].ResidualSum
		}
		check.Match = check.RowRadianceSum == check.RecomputedRadianceSum &&
			check.RowRemainderSum == check.RecomputedRemainderSum
		if !check.Match {
			return nil, &e.Error{Code: e.Diff, Block: coord.String(), Field: "boundary_check", Expected: fmt.Sprintf("rad=%d rem=%d", check.RecomputedRadianceSum, check.RecomputedRemainderSum), Actual: fmt.Sprintf("rad=%d rem=%d", check.RowRadianceSum, check.RowRemainderSum), Message: "medium row accumulation disagrees with block boundary recomputation"}
		}
		out.BoundaryCheck = check
		out.BoundaryRecomputed = true
	}

	out.QuantHash = domain.HashJSON(map[string]any{"block": coord.String(), "method": decision.Method, "bands": out.Bands})
	return out, nil
}

func quantizeOneAdaptive(r int64, band domain.Band, p *domain.Project, method Method, lut *boundLUT) (q, rem int64, sat bool, lutHit bool, boundary bool, err error) {
	switch method {
	case MethodExact, MethodRow:
		q, rem, sat, err = QuantizeValue(r, band, p, nil)
		return q, rem, sat, false, false, err
	case MethodLUT:
		entry, same, lookupOK := lut.lookup(r)
		if lookupOK && same {
			if entry.LowQ >= p.QuantScale {
				q, rem, sat, err = QuantizeValue(r, band, p, nil)
				return q, rem, sat, true, true, err
			}
			rem, err := remainderForQ(r, band, p, entry.LowQ)
			if err != nil {
				q, rem, sat, qerr := QuantizeValue(r, band, p, nil)
				return q, rem, sat, true, true, qerr
			}
			return entry.LowQ, rem, false, true, false, nil
		}
		q, rem, sat, err = QuantizeValue(r, band, p, nil)
		return q, rem, sat, false, lookupOK, err
	default:
		return 0, 0, false, false, false, fmt.Errorf("unknown quantization method %q", method)
	}
}

func remainderForQ(r int64, band domain.Band, p *domain.Project, q int64) (int64, error) {
	adjusted, _, err := conv.MulDivFloor(r, band.GainPPM, conv.GainPPMUnity, "quantization.gain", nil, "gain")
	if err != nil {
		return 0, err
	}
	dose, err := conv.CheckedMul(adjusted, p.ReferenceExpUS, "quantization.dose")
	if err != nil {
		return 0, err
	}
	number, err := conv.CheckedMul(dose, p.QuantScale, "quantization.numerator")
	if err != nil {
		return 0, err
	}
	qWell, err := conv.CheckedMul(q, p.FullWellDose, "quantization.qwell")
	if err != nil {
		return 0, err
	}
	if number < qWell {
		return 0, fmt.Errorf("invalid LUT quotient")
	}
	return number - qWell, nil
}

func errorBound(band domain.Band, p *domain.Project, method Method, t Thresholds) (int64, error) {
	switch method {
	case MethodExact:
		return 0, nil
	case MethodRow:
		return 1, nil
	case MethodLUT:
		// Conservative integer bound: one coarse radiance step can change the
		// normalized quantizer by floor(step * exposure * scale / fullWell),
		// plus one unit for endpoint/floor uncertainty.
		stepDose, err := conv.CheckedMul(t.LUTStepRadiance, p.ReferenceExpUS, "error_bound.step_dose")
		if err != nil {
			return 0, err
		}
		delta, _, err := conv.MulDivFloor(stepDose, p.QuantScale, p.FullWellDose, "error_bound.quant", nil, "bound")
		if err != nil {
			return 0, err
		}
		gainDelta, _, err := conv.MulDivFloor(delta, band.GainPPM, conv.GainPPMUnity, "error_bound.gain", nil, "bound_gain")
		if err != nil {
			return 0, err
		}
		return gainDelta + 1, nil
	default:
		return 0, fmt.Errorf("unknown method %q", method)
	}
}

type lutEntry struct {
	Bucket int64
	LowQ   int64
	HighQ  int64
}

type boundLUT struct {
	step    int64
	entries map[int64]lutEntry
	project *domain.Project
	band    domain.Band
}

func newBoundLUT(band domain.Band, p *domain.Project, step int64) *boundLUT {
	return &boundLUT{step: step, entries: map[int64]lutEntry{}, project: p, band: band}
}

func (l *boundLUT) lookup(r int64) (lutEntry, bool, bool) {
	bucket := r / l.step
	if e, ok := l.entries[bucket]; ok {
		return e, e.LowQ == e.HighQ, true
	}
	low := bucket * l.step
	high := low + l.step - 1
	lowQ, _, lowSat, err := QuantizeValue(low, l.band, l.project, nil)
	if err != nil {
		return lutEntry{}, false, false
	}
	highQ, _, highSat, err := QuantizeValue(high, l.band, l.project, nil)
	if err != nil {
		return lutEntry{}, false, false
	}
	if lowSat {
		lowQ = l.project.QuantScale
	}
	if highSat {
		highQ = l.project.QuantScale
	}
	e := lutEntry{Bucket: bucket, LowQ: lowQ, HighQ: highQ}
	l.entries[bucket] = e
	return e, lowQ == highQ, true
}
