package dialogue

import "fmt"

func validateProject(p *Project) error {
	if p == nil {
		return newError(CodeSchema, "", "$", "project is nil")
	}
	if p.ID == "" {
		return newError(CodeSchema, "", "id", "dialogue id is empty")
	}
	if p.SampleRate <= 0 {
		return newError(CodeSchema, p.ID, "sample_rate", "must be positive")
	}
	if p.DeclaredTotalSamples < 0 {
		return newError(CodeSchema, p.ID, "declared_total_samples", "must be non-negative")
	}
	if int64(len(p.PCM)) != p.DeclaredTotalSamples {
		return newError(CodeSchema, p.ID, "pcm.samples", fmt.Sprintf("sample count %d does not match declared total %d", len(p.PCM), p.DeclaredTotalSamples))
	}
	if p.FrameLength <= 0 {
		return newError(CodeSchema, p.ID, "frame_length", "must be positive")
	}
	if p.Hop <= 0 {
		return newError(CodeSchema, p.ID, "hop", "must be positive")
	}
	if p.GainLimit < 0 {
		return newError(CodeSchema, p.ID, "gain_limit", "must be non-negative")
	}
	if p.TargetLoudness < 0 {
		return newError(CodeSchema, p.ID, "target_loudness", "must be non-negative")
	}
	if p.GainMax < p.GainMin {
		return newError(CodeSchema, p.ID, "session_gain_range", "maximum is lower than minimum")
	}
	if len(p.Scenes) == 0 {
		return newError(CodeSchema, p.ID, "scenes", "at least one scene is required")
	}
	sceneSeen := map[string]bool{}
	segmentSeen := map[string]bool{}
	var previousEnd int64 = 0
	segments := p.Segments()
	if len(segments) == 0 {
		return newError(CodeSchema, p.ID, "segments", "at least one segment is required")
	}
	for sceneIndex, scene := range p.Scenes {
		if scene.ID == "" {
			return newError(CodeSchema, p.ID, fmt.Sprintf("scenes[%d].id", sceneIndex), "scene id is empty")
		}
		if sceneSeen[scene.ID] {
			return newError(CodeSchema, p.ID, fmt.Sprintf("scenes.%s", scene.ID), "duplicate scene id")
		}
		sceneSeen[scene.ID] = true
		if len(scene.Segments) == 0 {
			return newError(CodeSchema, p.ID, fmt.Sprintf("scenes[%d].segments", sceneIndex), "scene has no segments")
		}
		for segmentIndex, segment := range scene.Segments {
			path := fmt.Sprintf("scenes[%d].segments[%d]", sceneIndex, segmentIndex)
			if err := validateSegment(p, segment, path, previousEnd); err != nil {
				return err
			}
			if segmentSeen[segment.ID] {
				return newError(CodeSchema, p.ID, path+".id", "duplicate segment id")
			}
			segmentSeen[segment.ID] = true
			previousEnd = segment.EndSample
		}
	}
	return validateTruth(p)
}

func validateSegment(p *Project, segment Segment, path string, previousEnd int64) error {
	if segment.ID == "" {
		return newError(CodeSchema, p.ID, path+".id", "segment id is empty")
	}
	if segment.StartSample < 0 || segment.EndSample <= segment.StartSample || segment.EndSample > p.DeclaredTotalSamples {
		return newError(CodeSchema, p.ID, path+".end_sample", "invalid segment sample range")
	}
	if segment.StartSample < previousEnd {
		return newError(CodeSchema, p.ID, path+".start_sample", "segments overlap or are not strictly ordered")
	}
	if segment.Length() < p.FrameLength {
		return newError(CodeSchema, p.ID, path+".end_sample", "segment is shorter than one frame")
	}
	if segment.SessionGain < p.GainMin || segment.SessionGain > p.GainMax {
		return newError(CodeSchema, p.ID, path+".session_gain", fmt.Sprintf("gain %d outside declared range [%d,%d]", segment.SessionGain, p.GainMin, p.GainMax))
	}
	if len(segment.Phonemes) == 0 {
		return newError(CodeSchema, p.ID, path+".phonemes", "at least one phoneme is required")
	}
	phonemeSeen := map[string]bool{}
	var previousPhonemeEnd int64 = segment.StartSample - 1
	var previousPhonemeStart int64 = segment.StartSample - 1
	for i, phoneme := range segment.Phonemes {
		phonemePath := fmt.Sprintf("%s.phonemes[%d]", path, i)
		if phoneme.ID == "" {
			return newError(CodeSchema, p.ID, phonemePath+".id", "phoneme id is empty")
		}
		if phonemeSeen[phoneme.ID] {
			return newError(CodeSchema, p.ID, phonemePath+".id", "duplicate phoneme id")
		}
		phonemeSeen[phoneme.ID] = true
		if !phoneme.Kind.Valid() {
			return newError(CodeSchema, p.ID, phonemePath+".kind", "invalid phoneme category "+string(phoneme.Kind))
		}
		if phoneme.StartSample < segment.StartSample || phoneme.EndSample > segment.EndSample || phoneme.EndSample <= phoneme.StartSample {
			return newError(CodeSchema, p.ID, phonemePath+".start_sample", "phoneme range is outside its segment")
		}
		if phoneme.StartSample <= previousPhonemeStart {
			return newError(CodeSchema, p.ID, phonemePath+".start_sample", "phoneme starts are not strictly increasing")
		}
		if phoneme.StartSample < previousPhonemeEnd {
			return newError(CodeSchema, p.ID, phonemePath+".start_sample", "phoneme intervals overlap")
		}
		if phoneme.Weight < 0 {
			return newError(CodeSchema, p.ID, phonemePath+".weight", "loudness weight must be non-negative")
		}
		if phoneme.BaseF0MilliHz <= 0 {
			return newError(CodeSchema, p.ID, phonemePath+".base_f0_milli_hz", "base F0 must be positive")
		}
		var previousPoint int64 = phoneme.StartSample - 1
		for j, point := range phoneme.F0Points {
			if point.AtSample < phoneme.StartSample || point.AtSample >= phoneme.EndSample {
				return newError(CodeSchema, p.ID, fmt.Sprintf("%s.f0_points[%d].at_sample", phonemePath, j), "F0 point is outside the phoneme")
			}
			if point.AtSample <= previousPoint {
				return newError(CodeSchema, p.ID, fmt.Sprintf("%s.f0_points[%d].at_sample", phonemePath, j), "F0 points must be strictly increasing")
			}
			if point.HertzMilli <= 0 {
				return newError(CodeSchema, p.ID, fmt.Sprintf("%s.f0_points[%d].hertz_milli", phonemePath, j), "F0 value must be positive")
			}
			previousPoint = point.AtSample
		}
		previousPhonemeStart = phoneme.StartSample
		previousPhonemeEnd = phoneme.EndSample
	}
	return nil
}

func validateTruth(p *Project) error {
	if p.Truth == nil {
		return newError(CodeSchema, p.ID, "truth", "truth manifest is required")
	}
	segments := map[string]Segment{}
	for _, segment := range p.Segments() {
		segments[segment.ID] = segment
	}
	expectedFrames := 0
	for _, segment := range p.Segments() {
		expectedFrames += frameCount(segment, p.FrameLength, p.Hop)
	}
	if len(p.Truth.Frames) != expectedFrames {
		return newError(CodeSchema, p.ID, "truth.frames", fmt.Sprintf("truth has %d frames, expected %d from frame geometry", len(p.Truth.Frames), expectedFrames))
	}
	frameSeen := map[string]bool{}
	truthSegments := map[string]bool{}
	for i, frame := range p.Truth.Frames {
		path := fmt.Sprintf("truth.frames[%d]", i)
		segment, ok := segments[frame.SegmentID]
		if !ok {
			return newError(CodeSchema, p.ID, path+".segment_id", "unknown segment id")
		}
		if frame.FrameIndex < 0 || frame.FrameIndex >= frameCount(segment, p.FrameLength, p.Hop) {
			return newError(CodeSchema, p.ID, path+".frame_index", "frame index outside segment frame range")
		}
		key := fmt.Sprintf("%s:%d", frame.SegmentID, frame.FrameIndex)
		if frameSeen[key] {
			return newError(CodeSchema, p.ID, path+".frame_index", "duplicate truth frame")
		}
		frameSeen[key] = true
		start := segment.StartSample + int64(frame.FrameIndex)*p.Hop
		end := start + p.FrameLength
		if frame.StartSample != start || frame.EndSample != end {
			return newError(CodeSchema, p.ID, path+".start_sample", fmt.Sprintf("truth frame geometry [%d,%d) does not match [%d,%d)", frame.StartSample, frame.EndSample, start, end))
		}
		if frame.Energy < 0 || frame.PeakPosition < 0 || frame.PeakPosition >= int(p.FrameLength) {
			return newError(CodeSchema, p.ID, path, "invalid energy or peak position")
		}
	}
	for i, segment := range p.Truth.Segments {
		path := fmt.Sprintf("truth.segments[%d]", i)
		projectSegment, ok := segments[segment.SegmentID]
		if !ok {
			return newError(CodeSchema, p.ID, path+".segment_id", "unknown segment id")
		}
		if truthSegments[segment.SegmentID] {
			return newError(CodeSchema, p.ID, path+".segment_id", "duplicate truth segment")
		}
		truthSegments[segment.SegmentID] = true
		count := frameCount(projectSegment, p.FrameLength, p.Hop)
		if len(segment.Calibrated) != count {
			return newError(CodeSchema, p.ID, path+".calibrated", fmt.Sprintf("has %d calibrated frames, expected %d", len(segment.Calibrated), count))
		}
	}
	if len(truthSegments) != len(segments) {
		return newError(CodeSchema, p.ID, "truth.segments", "truth does not cover every segment")
	}
	return nil
}

func frameCount(segment Segment, frameLength, hop int64) int {
	if segment.Length() < frameLength || hop <= 0 {
		return 0
	}
	return int((segment.Length()-frameLength)/hop) + 1
}
