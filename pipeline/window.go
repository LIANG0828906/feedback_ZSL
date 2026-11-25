package pipeline

import (
	"fmt"
	"sort"
)

// FeatureWindowPlan is the deterministic set of window starts for a feature.
type FeatureWindowPlan struct {
	FeatureID     string      `json:"feature_id"`
	AlignmentBase int64       `json:"alignment_base_ms"`
	FirstStartMs  int64       `json:"first_start_ms"`
	LastStartMs   int64       `json:"last_start_ms"`
	Windows       []WindowKey `json:"windows"`
}

// WindowPlans enumerates every stride-aligned window between a feature's first
// and last observation. Epoch alignment uses Unix epoch base zero; first-data
// alignment uses the earliest observation in the project as base. For the
// generated fixture the origin is divisible by every step, so both encodings
// resolve to identical absolute boundaries.
func WindowPlans(p *Project) (map[string]FeatureWindowPlan, error) {
	if p == nil {
		return nil, NewBusinessError(CodeSchemaUnsupported, "nil project")
	}
	var base int64
	switch p.Alignment {
	case AlignEpoch:
		base = 0
	case AlignFirstData:
		if len(p.Events) == 0 {
			base = p.TimeOriginMs
		} else {
			base = p.Events[0].ObservedAtMs
			for _, e := range p.Events[1:] {
				if e.ObservedAtMs < base {
					base = e.ObservedAtMs
				}
			}
		}
	default:
		return nil, NewBusinessError(CodeSchemaAmbiguous, "unsupported alignment %q", p.Alignment).
			WithContext("alignment", string(p.Alignment))
	}
	byFeature := make(map[string][]ObservationEvent, len(p.Features))
	for _, e := range p.Events {
		byFeature[e.FeatureID] = append(byFeature[e.FeatureID], e)
	}
	plans := make(map[string]FeatureWindowPlan, len(p.Features))
	for _, f := range p.Features {
		if f.WindowWidthMs <= 0 {
			return nil, NewBusinessError(CodeFeatureWindowWidth, "window width must be positive").
				WithContext("feature_id", f.ID).WithContext("value", fmt.Sprint(f.WindowWidthMs))
		}
		if f.WindowStepMs <= 0 {
			return nil, NewBusinessError(CodeFeatureStepZero, "window step must be positive").
				WithContext("feature_id", f.ID).WithContext("value", fmt.Sprint(f.WindowStepMs))
		}
		plan := FeatureWindowPlan{FeatureID: f.ID, AlignmentBase: base}
		events := byFeature[f.ID]
		if len(events) > 0 {
			minTS, maxTS := events[0].ObservedAtMs, events[0].ObservedAtMs
			for _, e := range events[1:] {
				if e.ObservedAtMs < minTS {
					minTS = e.ObservedAtMs
				}
				if e.ObservedAtMs > maxTS {
					maxTS = e.ObservedAtMs
				}
			}
			first := floorToStep(minTS, base, f.WindowStepMs)
			last := floorToStep(maxTS, base, f.WindowStepMs)
			plan.FirstStartMs, plan.LastStartMs = first, last
			count := (last-first)/f.WindowStepMs + 1
			plan.Windows = make([]WindowKey, 0, count)
			for start := first; start <= last; start += f.WindowStepMs {
				plan.Windows = append(plan.Windows, WindowKey{
					FeatureID: f.ID, StartMs: start, EndMs: start + f.WindowWidthMs,
				})
			}
		}
		plans[f.ID] = plan
	}
	return plans, nil
}

func floorToStep(ts, base, step int64) int64 {
	delta := ts - base
	q := delta / step
	if delta < 0 && delta%step != 0 {
		q--
	}
	return base + q*step
}

func candidateStarts(plan FeatureWindowPlan, width, observed int64) []WindowKey {
	if len(plan.Windows) == 0 {
		return nil
	}
	lo := sort.Search(len(plan.Windows), func(i int) bool {
		return plan.Windows[i].StartMs >= observed-width+1
	})
	hi := sort.Search(len(plan.Windows), func(i int) bool {
		return plan.Windows[i].StartMs > observed
	})
	if lo >= hi {
		return nil
	}
	return plan.Windows[lo:hi]
}
