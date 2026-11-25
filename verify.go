package dialogue

// TruthFeatureMap indexes generator truth by concrete frame range ID.
func TruthFeatureMap(truth *TruthManifest) map[string]FrameFeature {
	return truthFeatureMap(truth)
}

func truthFeatureMap(truth *TruthManifest) map[string]FrameFeature {
	out := make(map[string]FrameFeature)
	if truth == nil {
		return out
	}
	for _, clip := range truth.Clips {
		for _, feature := range clip.Features {
			out[feature.RangeID] = feature
		}
	}
	return out
}

// EnergyPassRate returns the fraction of frames within energy tolerance.
func EnergyPassRate(rows []FrameResult, truth map[string]FrameFeature, tolerance int64) float64 {
	return energyPassRate(rows, truth, tolerance)
}

func energyPassRate(rows []FrameResult, truth map[string]FrameFeature, tolerance int64) float64 {
	if len(rows) == 0 {
		return 0
	}
	pass := 0
	for _, row := range rows {
		want := truth[row.RangeID]
		if abs64(row.Feature.EnergyFixed-want.EnergyFixed) <= tolerance {
			pass++
		}
	}
	return float64(pass) / float64(len(rows))
}

// CalibrationPassRate scores calibrated after-loudness against truth.
func CalibrationPassRate(rows []FrameResult, truth *TruthManifest, toleranceMDB int64) float64 {
	expected := make(map[string]CalibratedFrame)
	for _, clip := range truth.Clips {
		for _, cal := range clip.Calibration {
			expected[cal.RangeID] = cal
		}
	}
	pass := 0
	for _, row := range rows {
		want := expected[row.RangeID]
		if abs64(row.Calibration.LoudAfterMDB-want.LoudAfterMDB) <= toleranceMDB {
			pass++
		}
	}
	return float64(pass) / float64(len(rows))
}
