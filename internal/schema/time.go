package schema

import "maintenanceorchestrator/internal/domain"

func resolveTick(absolute *int64, offset *int64, base domain.Tick, path string) (domain.Tick, error) {
	if absolute != nil && offset != nil {
		expected := int64(base) + *offset
		if *absolute != expected {
			return 0, domain.NewError(domain.ETimeBaseConflict, path,
				"logical tick %d conflicts with window offset %d (expected %d)",
				*absolute, *offset, expected).
				WithDetailsSafe(map[string]any{
					"absolute_tick":   *absolute,
					"relative_offset": *offset,
					"window_start":    int64(base),
					"expected_tick":   expected,
				})
		}
		return domain.Tick(*absolute), nil
	}
	if absolute != nil {
		return domain.Tick(*absolute), nil
	}
	if offset != nil {
		return base + domain.Tick(*offset), nil
	}
	return 0, domain.NewError(domain.ETimeBaseConflict, path, "either absolute tick or relative offset is required")
}
