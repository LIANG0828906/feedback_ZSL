package engine

import (
	"strconv"
	"time"

	"featurekernel/internal/kernel"
)

const CodeLateBeyondAllowed = "E_LATE_BEYOND_ALLOWED"

type WindowState struct {
	Coord      kernel.WindowCoord `json:"coord"`
	Acc        kernel.Accumulator `json:"acc"`
	MemberSeqs []int64            `json:"member_seqs"`
	Submitted  bool               `json:"submitted"`
	DirtySeq   int64              `json:"dirty_seq"`
}

type FeatureOutput struct {
	Results        []kernel.WindowResult
	Recomputations []kernel.Recomputation
	Discarded      []kernel.DiscardedRecord
	Diagnostic     kernel.FeatureDiagnostic
	States         map[int64]*WindowState
	Decision       kernel.PathDecision
}

type Runtime struct {
	Project    *kernel.Project
	Feature    kernel.FeatureDef
	ActualPath string
	Decision   kernel.PathDecision

	States         map[int64]*WindowState
	Recomputations []kernel.Recomputation
	Discarded      []kernel.DiscardedRecord
	AppliedSeq     int64

	started     time.Time
	eventCount  int
	watermark   *int64
	discardKeys map[string]bool
}

func ProcessFeature(project *kernel.Project, feature kernel.FeatureDef, forcePath string) (*FeatureOutput, error) {
	runtime, err := NewRuntime(project, feature, forcePath)
	if err != nil {
		return nil, err
	}
	for _, record := range project.Records {
		if err := runtime.ApplyRecord(record); err != nil {
			return nil, err
		}
	}
	return runtime.Output()
}

func NewRuntime(project *kernel.Project, feature kernel.FeatureDef, forcePath string) (*Runtime, error) {
	if err := kernel.ValidateFeature(feature); err != nil {
		return nil, err
	}
	if err := kernel.ValidateBudget(project.Budget); err != nil {
		return nil, err
	}
	decision := kernel.SelectPath(feature.WidthMs, feature.SlideMs, project.PathPolicy)
	actualPath := decision.Path
	if forcePath == kernel.PathIncremental || forcePath == kernel.PathRescan {
		actualPath = forcePath
	} else if forcePath != "" {
		return nil, kernel.NewError(kernel.CodeInvalidArgument, "unknown forced path", map[string]any{"path": forcePath})
	}
	return &Runtime{
		Project:        project,
		Feature:        feature,
		ActualPath:     actualPath,
		Decision:       decision,
		States:         map[int64]*WindowState{},
		Recomputations: []kernel.Recomputation{},
		Discarded:      []kernel.DiscardedRecord{},
		started:        time.Now(),
		discardKeys:    map[string]bool{},
	}, nil
}

func (r *Runtime) SetWatermark(watermark *int64) { r.watermark = watermark }

func (r *Runtime) Watermark() *int64 { return r.watermark }

func (r *Runtime) HydrateState(state WindowState) {
	copyState := state
	r.States[copyState.Coord.Index] = &copyState
}

func DiscardKey(arrivalSeq, windowIndex int64) string {
	return strconv.FormatInt(arrivalSeq, 10) + "#" + strconv.FormatInt(windowIndex, 10)
}

func (r *Runtime) HydrateDiscarded(records []kernel.DiscardedRecord) {
	for _, record := range records {
		r.Discarded = append(r.Discarded, record)
		r.discardKeys[DiscardKey(record.ArrivalSeq, record.WindowIndex)] = true
	}
}

func (r *Runtime) HydrateRecomputations(records []kernel.Recomputation) {
	r.Recomputations = append(r.Recomputations, records...)
}

func (r *Runtime) ApplyRecord(record kernel.Record) error {
	r.AppliedSeq = record.Seq
	switch record.Kind {
	case kernel.RecordWatermark:
		if record.Watermark == nil {
			return kernel.NewError(kernel.CodeSchemaInvalidCombination, "watermark record has no payload", map[string]any{"seq": record.Seq})
		}
		if err := r.applyWatermark(record.Watermark.TimeMs); err != nil {
			return err
		}
		watermark := record.Watermark.TimeMs
		r.watermark = &watermark
	case kernel.RecordEvent:
		if record.Event == nil || record.Event.FeatureID != r.Feature.ID {
			return nil
		}
		r.eventCount++
		return r.applyEvent(record.Event)
	}
	return nil
}

func (r *Runtime) applyWatermark(watermark int64) error {
	if r.watermark != nil && watermark < *r.watermark {
		for _, state := range r.States {
			if state.Submitted && watermark < state.Coord.End {
				return kernel.NewError(kernel.CodeWatermarkBeforeCommitted, "watermark is earlier than a submitted window", map[string]any{
					"feature_id": r.Feature.ID,
					"watermark":  watermark,
					"previous":   *r.watermark,
					"window_id":  kernel.WindowID(r.Feature.ID, state.Coord.Index),
					"window_end": state.Coord.End,
				})
			}
		}
		return kernel.NewError(kernel.CodeWatermarkBeforeCommitted, "watermark regresses before the previous observed watermark", map[string]any{
			"feature_id": r.Feature.ID, "watermark": watermark, "previous": *r.watermark,
		})
	}
	for _, state := range r.States {
		if !state.Submitted && watermark >= state.Coord.End+r.Feature.AllowedLatenessMs {
			state.Submitted = true
		}
	}
	return nil
}

func (r *Runtime) applyEvent(event *kernel.Event) error {
	coords, err := kernel.WindowsForTime(r.Feature.AnchorMs, r.Feature.WidthMs, r.Feature.SlideMs, event.TimeMs)
	if err != nil {
		return err
	}
	if len(coords) == 0 {
		return kernel.NewError(kernel.CodeEventInvalidTime, "event is before window anchor", map[string]any{
			"feature_id": r.Feature.ID, "arrival_seq": event.ArrivalSeq, "time_ms": event.TimeMs, "anchor_ms": r.Feature.AnchorMs,
		})
	}

	lateMs := int64(0)
	isLate := false
	if r.watermark != nil && event.TimeMs < *r.watermark {
		lateMs = *r.watermark - event.TimeMs
		isLate = true
	}

	if lateMs > r.Feature.AllowedLatenessMs {
		for _, coord := range coords {
			discard := kernel.DiscardedRecord{
				ArrivalSeq: event.ArrivalSeq, FeatureID: r.Feature.ID,
				WindowID: kernel.WindowID(r.Feature.ID, coord.Index), WindowIndex: coord.Index,
				StartMs: coord.Start, EndMs: coord.End, TimeMs: event.TimeMs,
				Channel: event.Channel, Value: event.Value, LateMs: lateMs, Code: CodeLateBeyondAllowed,
			}
			key := DiscardKey(event.ArrivalSeq, coord.Index)
			if !r.discardKeys[key] {
				r.discardKeys[key] = true
				r.Discarded = append(r.Discarded, discard)
			}
		}
		return nil
	}

	missing := 0
	for _, coord := range coords {
		if _, ok := r.States[coord.Index]; !ok {
			missing++
		}
	}
	if len(r.States)+missing > r.Project.Budget.MaxWindowsPerFeature {
		return kernel.NewError(kernel.CodeBackpressureBuffer, "per-feature window buffer budget exhausted", map[string]any{
			"feature_id":           r.Feature.ID,
			"existing_windows":     len(r.States),
			"required_new_windows": missing,
			"projected_windows":    len(r.States) + missing,
			"limit":                r.Project.Budget.MaxWindowsPerFeature,
			"arrival_seq":          event.ArrivalSeq,
		})
	}

	weight := int64(1_000_000)
	if configured, ok := r.Feature.ChannelWeightsPPM[event.Channel]; ok {
		weight = configured
	}

	for _, coord := range coords {
		state, existed := r.States[coord.Index]
		if !existed {
			state = &WindowState{Coord: coord, Acc: *kernel.NewAccumulator()}
			r.States[coord.Index] = state
		}
		hadMembers := existed && len(state.MemberSeqs) > 0
		recompute := isLate && lateMs > 0 && hadMembers
		if recompute {
			r.Recomputations = append(r.Recomputations, kernel.Recomputation{
				FeatureID: r.Feature.ID, WindowID: kernel.WindowID(r.Feature.ID, coord.Index), WindowIndex: coord.Index,
				TriggerSeq: event.ArrivalSeq, EventArrivalSeq: event.ArrivalSeq, LateMs: lateMs,
				Reason: "late_event_within_allowed_lateness",
			})
		}
		state.MemberSeqs = appendUniqueInt64(state.MemberSeqs, event.ArrivalSeq)
		state.DirtySeq = event.ArrivalSeq
		if r.ActualPath != kernel.PathRescan {
			state.Acc.Add(event.Value, weight)
		}
	}
	return nil
}

func appendUniqueInt64(values []int64, value int64) []int64 {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
