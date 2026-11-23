package kernel

import (
	"sort"

	"inputkernel/internal/fixed"
	"inputkernel/internal/model"
)

type AxisErrorMetric struct {
	BindingID      string         `json:"binding_id"`
	AxisID         string         `json:"axis_id"`
	Raw            int64          `json:"raw"`
	Normalized     int64          `json:"normalized"`
	Residual       model.Fraction `json:"quantization_residual"`
	ResidualBound  int64          `json:"quantization_error_bound"`
	DeadzoneInside bool           `json:"deadzone_inside"`
	DeadzoneMargin int64          `json:"deadzone_margin"`
	CurveError     int64          `json:"curve_linearization_error"`
}

type TimingErrorMetric struct {
	BindingID string `json:"binding_id"`
	Kind      string `json:"kind"`
	MarginMS  int64  `json:"margin_ms"`
	WindowMS  int64  `json:"window_ms"`
	EventGap  int64  `json:"event_gap_ms"`
}

type BoundSnapshot struct {
	QuantizationFixed int64 `json:"quantization_fixed"`
	DeadzoneFixed     int64 `json:"deadzone_fixed"`
	CurveFixed        int64 `json:"curve_fixed"`
	TimingMS          int64 `json:"timing_ms"`
}

type FlipRisk struct {
	EventSeq int    `json:"event_seq"`
	Field    string `json:"field"`
	Bound    int64  `json:"bound"`
	Margin   int64  `json:"margin"`
	Detail   string `json:"detail"`
}

type EventErrorRecord struct {
	EventSeq       int                 `json:"event_seq"`
	TimeMS         int64               `json:"time_ms"`
	Axis           []AxisErrorMetric   `json:"axis"`
	Timing         []TimingErrorMetric `json:"timing"`
	Cumulative     BoundSnapshot       `json:"cumulative_upper_bounds"`
	PotentialFlips []FlipRisk          `json:"potential_flips,omitempty"`
}

type ErrorPropagationReport struct {
	Events              []EventErrorRecord `json:"events"`
	FinalBounds         BoundSnapshot      `json:"final_upper_bounds"`
	PotentialFlipEvents []int              `json:"potential_flip_events,omitempty"`
	QuantizationRule    string             `json:"quantization_rule"`
	DeadzoneRule        string             `json:"deadzone_rule"`
	CurveRule           string             `json:"curve_rule"`
	TimingRule          string             `json:"timing_rule"`
}

func (rt *Runtime) ErrorReport() ErrorPropagationReport {
	s := rt.Scenario()
	samples := rt.AxisSamples()
	timings := rt.TimingChecks()
	samplesBySeq := map[int][]AxisSample{}
	for _, sample := range samples {
		samplesBySeq[sample.EventSeq] = append(samplesBySeq[sample.EventSeq], sample)
	}
	timingsBySeq := map[int][]TimingCheck{}
	for _, check := range timings {
		timingsBySeq[check.EventSeq] = append(timingsBySeq[check.EventSeq], check)
	}
	report := ErrorPropagationReport{
		QuantizationRule: "each normalized axis sample contributes at most one fixed unit (half-quantum rounded upward)",
		DeadzoneRule:     "deadzone classification uncertainty is bounded by cumulative axis quantization error",
		CurveRule:        "curve chord error uses declared curvature_ppm and the containing segment width; propagated quantization is added",
		TimingRule:       "timing uncertainty accumulates min(window, event_gap) for observed timing checks",
	}
	bounds := BoundSnapshot{}
	flipSet := map[int]bool{}
	for _, event := range s.Events {
		record := EventErrorRecord{
			EventSeq: event.Seq, TimeMS: event.TimeMS,
			Axis: []AxisErrorMetric{}, Timing: []TimingErrorMetric{},
		}
		for _, sample := range samplesBySeq[event.Seq] {
			resAbs := fixed.FractionAbs(sample.Residual)
			record.Axis = append(record.Axis, AxisErrorMetric{
				BindingID: sample.BindingID, AxisID: sample.AxisID, Raw: sample.Raw,
				Normalized: sample.Normalized, Residual: resAbs,
				ResidualBound: sample.ResidualBound, DeadzoneInside: sample.DeadzoneInside,
				DeadzoneMargin: sample.DeadzoneMargin, CurveError: sample.CurveError,
			})
			bounds.QuantizationFixed += sample.ResidualBound
			bounds.DeadzoneFixed = bounds.QuantizationFixed
			bounds.CurveFixed += sample.CurveError + sample.ResidualBound
			if bounds.DeadzoneFixed >= sample.DeadzoneMargin {
				record.PotentialFlips = append(record.PotentialFlips, FlipRisk{
					EventSeq: event.Seq, Field: "deadzone_inside",
					Bound: bounds.DeadzoneFixed, Margin: sample.DeadzoneMargin,
					Detail: "cumulative quantization error reaches the deadzone boundary margin",
				})
			}
			if bounds.CurveFixed >= sample.DeadzoneMargin {
				record.PotentialFlips = append(record.PotentialFlips, FlipRisk{
					EventSeq: event.Seq, Field: "curve_zero_crossing",
					Bound: bounds.CurveFixed, Margin: sample.DeadzoneMargin,
					Detail: "curve linearization and propagated quantization reach the deadzone boundary margin",
				})
			}
		}
		for _, check := range timingsBySeq[event.Seq] {
			gap := check.EventGap
			window := check.WindowMS
			local := gap
			if window > 0 && local > window {
				local = window
			}
			if local < 0 {
				local = 0
			}
			bounds.TimingMS += local
			record.Timing = append(record.Timing, TimingErrorMetric{
				BindingID: check.BindingID, Kind: check.Kind, MarginMS: check.MarginMS,
				WindowMS: check.WindowMS, EventGap: check.EventGap,
			})
			if bounds.TimingMS >= abs64(check.MarginMS) {
				record.PotentialFlips = append(record.PotentialFlips, FlipRisk{
					EventSeq: event.Seq, Field: "timing_boundary",
					Bound: bounds.TimingMS, Margin: abs64(check.MarginMS),
					Detail: "cumulative timing uncertainty reaches the observed timing decision margin",
				})
			}
		}
		sort.SliceStable(record.Timing, func(i, j int) bool {
			if record.Timing[i].BindingID != record.Timing[j].BindingID {
				return record.Timing[i].BindingID < record.Timing[j].BindingID
			}
			return record.Timing[i].Kind < record.Timing[j].Kind
		})
		if len(record.PotentialFlips) > 0 {
			flipSet[event.Seq] = true
			seen := map[string]bool{}
			filtered := record.PotentialFlips[:0]
			for _, risk := range record.PotentialFlips {
				key := risk.Field + "|" + risk.Detail
				if !seen[key] {
					seen[key] = true
					filtered = append(filtered, risk)
				}
			}
			record.PotentialFlips = filtered
		}
		record.Cumulative = bounds
		report.Events = append(report.Events, record)
	}
	report.FinalBounds = bounds
	for seq := range flipSet {
		report.PotentialFlipEvents = append(report.PotentialFlipEvents, seq)
	}
	sort.Ints(report.PotentialFlipEvents)
	return report
}
