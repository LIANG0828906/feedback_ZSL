package orchestrator

import (
	"math"
)

func millisecondsToTicks(ms uint64, tickMillis Ratio, path string) (Ratio, error) {
	if tickMillis.Num == 0 || tickMillis.Den == 0 {
		return Ratio{}, newError(CodeInvalidSchema, path, "tick duration must be positive")
	}
	if ms == 0 {
		return Ratio{Num: 0, Den: 1}, nil
	}
	num, ok := checkedMul(ms, tickMillis.Den)
	if !ok {
		return Ratio{}, newError(CodeNumericOverflow, path, "millisecond to tick conversion overflow")
	}
	result := Ratio{Num: num, Den: tickMillis.Num}
	if err := result.normalize(); err != nil {
		return Ratio{}, err
	}
	return result, nil
}

func requireWholeTicks(r Ratio, path string) (int64, error) {
	if r.Den != 1 {
		return 0, newError(CodeTimeNotAligned, path, "wire time %s does not land on an integral tick", r)
	}
	if r.Num > math.MaxInt64 {
		return 0, newError(CodeNumericOverflow, path, "tick exceeds int64")
	}
	return int64(r.Num), nil
}

func toBytes(value uint64, unit CapacityUnit, path string) (int64, error) {
	switch unit {
	case UnitByte:
		if value > math.MaxInt64 {
			return 0, newError(CodeNumericOverflow, path, "byte value exceeds int64")
		}
		return int64(value), nil
	case UnitKiB:
		bytes, ok := checkedMul(value, 1024)
		if !ok || bytes > math.MaxInt64 {
			return 0, newError(CodeNumericOverflow, path, "KiB value exceeds int64")
		}
		return int64(bytes), nil
	default:
		return 0, newError(CodeInvalidSchema, path, "unknown capacity unit %q", unit)
	}
}

func addZone(base int64, offset int64, path string) (int64, error) {
	if offset < 0 {
		return 0, newError(CodeInvalidSchema, path, "relative zone offset must be non-negative")
	}
	if base > 0 && offset > math.MaxInt64-base {
		return 0, newError(CodeNumericOverflow, path, "absolute zone id overflow")
	}
	if base < 0 && offset < math.MinInt64-base {
		return 0, newError(CodeNumericOverflow, path, "absolute zone id underflow")
	}
	return base + offset, nil
}

func normalizeTimeout(value uint64, unit TimeUnit, tickMillis Ratio, bits uint8, defaultValue *Timeout, path string) (Timeout, error) {
	var ticks Ratio
	var err error
	switch unit {
	case UnitTick:
		ticks = Ratio{Num: value, Den: 1}
	case UnitMS:
		ticks, err = millisecondsToTicks(value, tickMillis, path)
		if err != nil {
			return Timeout{}, err
		}
	default:
		return Timeout{}, newError(CodeInvalidSchema, path, "unknown time unit %q", unit)
	}
	if err := ticks.check(bits, path); err != nil {
		return Timeout{}, err
	}
	return Timeout{Set: true, Ticks: ticks}, nil
}

func defaultTimeout(project *Project) (Timeout, error) {
	if project.DefaultTimeout.Set {
		if project.DefaultTimeout.Unit == "" {
			project.DefaultTimeout.Unit = UnitTick
		}
		normalized, err := normalizeTimeout(project.DefaultTimeout.Wire, project.DefaultTimeout.Unit, project.TickMillis, project.TimeoutBits, nil, "$.default_timeout")
		if err != nil {
			return Timeout{}, err
		}
		normalized.Default = true
		return normalized, nil
	}
	// A deterministic non-zero default keeps missing optional timeout fields
	// meaningful while allowing all three schemas to omit them together.
	normalized, err := normalizeTimeout(10, UnitTick, project.TickMillis, project.TimeoutBits, nil, "$.default_timeout")
	if err != nil {
		return Timeout{}, err
	}
	normalized.Default = true
	return normalized, nil
}

func bindTimeout(project *Project, raw *uint64, unit TimeUnit, path string) (Timeout, error) {
	fallback, err := defaultTimeout(project)
	if err != nil {
		return Timeout{}, err
	}
	if raw == nil {
		return fallback, nil
	}
	normalized, err := normalizeTimeout(*raw, unit, project.TickMillis, project.TimeoutBits, &fallback, path)
	if err != nil {
		return Timeout{}, err
	}
	if normalized.Ticks == fallback.Ticks {
		normalized.Default = true
	}
	return normalized, nil
}
