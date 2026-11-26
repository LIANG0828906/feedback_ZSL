package dialogue

import "fmt"

type SemanticMismatch struct {
	Path      string `json:"path"`
	Expected  string `json:"expected"`
	Actual    string `json:"actual"`
	Tolerance int64  `json:"tolerance"`
}

// CompareSemantic verifies that a reloaded export retains every field that the
// target schema can express. Values are compared at the target schema's declared
// quantization residual, not bit-for-bit.
func CompareSemantic(original, loaded *Project, version string) []SemanticMismatch {
	var mismatches []SemanticMismatch
	valueTolerance := int64(0)
	timeTolerance := int64(0)
	if version == Version1 || version == Version3 {
		valueTolerance = 5
	}
	if version == Version2 {
		timeTolerance = 8
	}
	compareInt := func(path string, expected, actual, tolerance int64) {
		if abs64(expected-actual) > tolerance {
			mismatches = append(mismatches, SemanticMismatch{Path: path, Expected: fmt.Sprint(expected), Actual: fmt.Sprint(actual), Tolerance: tolerance})
		}
	}
	if original.ID != loaded.ID {
		mismatches = append(mismatches, SemanticMismatch{Path: "id", Expected: original.ID, Actual: loaded.ID})
	}
	if original.SampleRate != loaded.SampleRate {
		mismatches = append(mismatches, SemanticMismatch{Path: "sample_rate", Expected: fmt.Sprint(original.SampleRate), Actual: fmt.Sprint(loaded.SampleRate)})
	}
	if original.DeclaredTotalSamples != loaded.DeclaredTotalSamples || len(original.PCM) != len(loaded.PCM) {
		mismatches = append(mismatches, SemanticMismatch{Path: "declared_total_samples", Expected: fmt.Sprint(original.DeclaredTotalSamples), Actual: fmt.Sprint(loaded.DeclaredTotalSamples)})
	} else {
		for i := range original.PCM {
			if original.PCM[i] != loaded.PCM[i] {
				mismatches = append(mismatches, SemanticMismatch{Path: fmt.Sprintf("pcm.samples[%d]", i), Expected: fmt.Sprint(original.PCM[i]), Actual: fmt.Sprint(loaded.PCM[i])})
				break
			}
		}
	}
	compareInt("target_loudness", original.TargetLoudness, loaded.TargetLoudness, valueTolerance)
	compareInt("gain_limit", original.GainLimit, loaded.GainLimit, valueTolerance)
	compareInt("gain_minimum", original.GainMin, loaded.GainMin, valueTolerance)
	compareInt("gain_maximum", original.GainMax, loaded.GainMax, valueTolerance)
	compareInt("frame_length", original.FrameLength, loaded.FrameLength, timeTolerance)
	compareInt("hop", original.Hop, loaded.Hop, timeTolerance)
	if version != Version1 {
		if len(original.Scenes) != len(loaded.Scenes) {
			mismatches = append(mismatches, SemanticMismatch{Path: "scenes", Expected: fmt.Sprint(len(original.Scenes)), Actual: fmt.Sprint(len(loaded.Scenes))})
		} else {
			for i := range original.Scenes {
				if original.Scenes[i].ID != loaded.Scenes[i].ID {
					mismatches = append(mismatches, SemanticMismatch{Path: fmt.Sprintf("scenes[%d].id", i), Expected: original.Scenes[i].ID, Actual: loaded.Scenes[i].ID})
				}
			}
		}
	}
	originalSegments := original.Segments()
	loadedSegments := loaded.Segments()
	if len(originalSegments) != len(loadedSegments) {
		mismatches = append(mismatches, SemanticMismatch{Path: "segments", Expected: fmt.Sprint(len(originalSegments)), Actual: fmt.Sprint(len(loadedSegments))})
		return mismatches
	}
	for i := range originalSegments {
		expected := originalSegments[i]
		actual := loadedSegments[i]
		path := "segments." + expected.ID
		if expected.ID != actual.ID {
			mismatches = append(mismatches, SemanticMismatch{Path: path + ".id", Expected: expected.ID, Actual: actual.ID})
			continue
		}
		compareInt(path+".start_sample", expected.StartSample, actual.StartSample, timeTolerance)
		compareInt(path+".end_sample", expected.EndSample, actual.EndSample, timeTolerance)
		compareInt(path+".session_gain", expected.SessionGain, actual.SessionGain, valueTolerance)
		if version != Version1 && expected.SceneID != actual.SceneID {
			mismatches = append(mismatches, SemanticMismatch{Path: path + ".scene_id", Expected: expected.SceneID, Actual: actual.SceneID})
		}
		if len(expected.Phonemes) != len(actual.Phonemes) {
			mismatches = append(mismatches, SemanticMismatch{Path: path + ".phonemes", Expected: fmt.Sprint(len(expected.Phonemes)), Actual: fmt.Sprint(len(actual.Phonemes))})
			continue
		}
		for j := range expected.Phonemes {
			expectedPhoneme := expected.Phonemes[j]
			actualPhoneme := actual.Phonemes[j]
			phonemePath := fmt.Sprintf("%s.phonemes[%d]", path, j)
			if expectedPhoneme.ID != actualPhoneme.ID || expectedPhoneme.Kind != actualPhoneme.Kind {
				mismatches = append(mismatches, SemanticMismatch{Path: phonemePath, Expected: string(expectedPhoneme.Kind), Actual: string(actualPhoneme.Kind)})
			}
			compareInt(phonemePath+".start_sample", expectedPhoneme.StartSample, actualPhoneme.StartSample, timeTolerance)
			compareInt(phonemePath+".end_sample", expectedPhoneme.EndSample, actualPhoneme.EndSample, timeTolerance)
			compareInt(phonemePath+".weight", expectedPhoneme.Weight, actualPhoneme.Weight, valueTolerance)
			compareInt(phonemePath+".base_f0", expectedPhoneme.BaseF0MilliHz, actualPhoneme.BaseF0MilliHz, 0)
			if version != Version1 {
				if len(expectedPhoneme.F0Points) != len(actualPhoneme.F0Points) {
					mismatches = append(mismatches, SemanticMismatch{Path: phonemePath + ".f0_points", Expected: fmt.Sprint(len(expectedPhoneme.F0Points)), Actual: fmt.Sprint(len(actualPhoneme.F0Points))})
				} else {
					for k := range expectedPhoneme.F0Points {
						compareInt(fmt.Sprintf("%s.f0_points[%d].at_sample", phonemePath, k), expectedPhoneme.F0Points[k].AtSample, actualPhoneme.F0Points[k].AtSample, timeTolerance)
						compareInt(fmt.Sprintf("%s.f0_points[%d].hertz_milli", phonemePath, k), expectedPhoneme.F0Points[k].HertzMilli, actualPhoneme.F0Points[k].HertzMilli, 0)
					}
				}
			}
		}
	}
	if original.Truth != nil && loaded.Truth != nil {
		if len(original.Truth.Frames) != len(loaded.Truth.Frames) {
			mismatches = append(mismatches, SemanticMismatch{Path: "truth.frames", Expected: fmt.Sprint(len(original.Truth.Frames)), Actual: fmt.Sprint(len(loaded.Truth.Frames))})
		} else {
			for i := range original.Truth.Frames {
				expected := original.Truth.Frames[i]
				actual := loaded.Truth.Frames[i]
				path := fmt.Sprintf("truth.frames[%s:%d]", expected.SegmentID, expected.FrameIndex)
				compareInt(path+".expected_envelope", expected.ExpectedEnvelope, actual.ExpectedEnvelope, TruthTolerance(version))
				compareInt(path+".calibrated_envelope", expected.CalibratedEnvelope, actual.CalibratedEnvelope, TruthTolerance(version))
			}
		}
	}
	return mismatches
}
