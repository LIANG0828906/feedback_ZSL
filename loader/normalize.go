package loader

import "chaosdiag/model"

func normalizeMSOrTicks(ms *int64, ticks *int64, tickMS int64, field string) (int64, error) {
	if ms != nil && ticks != nil {
		fromTicks := *ticks * tickMS
		if *ms != fromTicks {
			err := model.NewError(model.CodeFieldConflict, field, "absolute ms=%d conflicts with ticks=%d => %dms", *ms, *ticks, fromTicks)
			model.WithContext(err, "ms", *ms)
			model.WithContext(err, "ticks", *ticks)
			return 0, err
		}
		return *ms, nil
	}
	if ms != nil {
		return *ms, nil
	}
	if ticks != nil {
		return *ticks * tickMS, nil
	}
	return 0, nil
}

func normalizeWindow(startAbs, endAbs, startAlt, duration *int, field string) (int, int, error) {
	start := 0
	haveStart := false
	if startAbs != nil {
		start, haveStart = *startAbs, true
	}
	if startAlt != nil {
		if haveStart && start != *startAlt {
			return 0, 0, model.NewError(model.CodeFieldConflict, field+".start_tick", "conflicting absolute starts %d and %d", start, *startAlt)
		}
		start, haveStart = *startAlt, true
	}
	if !haveStart {
		return 0, 0, model.NewError(model.CodeFieldRequired, field+".start_tick", "effective start is required")
	}

	end := 0
	haveEnd := false
	if endAbs != nil {
		end, haveEnd = *endAbs, true
	}
	if duration != nil {
		if *duration <= 0 {
			return 0, 0, model.NewError(model.CodeFieldInvalid, field+".duration_ticks", "duration must be positive")
		}
		relativeEnd := start + *duration - 1
		if haveEnd && end != relativeEnd {
			err := model.NewError(model.CodeFieldConflict, field+".end_tick", "absolute end=%d conflicts with start=%d duration=%d => end=%d", end, start, *duration, relativeEnd)
			model.WithContext(err, "absolute_end", end)
			model.WithContext(err, "relative_end", relativeEnd)
			return 0, 0, err
		}
		end, haveEnd = relativeEnd, true
	}
	if !haveEnd {
		return 0, 0, model.NewError(model.CodeFieldRequired, field+".end_tick", "absolute end or relative duration is required")
	}
	return start, end, nil
}

func quantileValue(values []quantilePoint, selected string, field string) (int64, error) {
	if selected == "" {
		selected = "P90"
	}
	for _, q := range values {
		if q.Quantile == selected {
			return q.ValueMS, nil
		}
	}
	return 0, model.NewError(model.CodeFieldConflict, field+".quantile", "quantile %q is not present in amplitude manifest", selected)
}

type quantilePoint struct {
	Quantile string
	ValueMS  int64
}
