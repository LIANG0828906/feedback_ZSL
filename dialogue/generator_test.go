package dialogue

import "testing"

func TestGeneratedSampleEnvelopeMatchesTruth(t *testing.T) {
	project, err := GenerateProject(42)
	if err != nil {
		t.Fatalf("GenerateProject: %v", err)
	}
	envelopes, err := MeasureProjectEnvelopes(project)
	if err != nil {
		t.Fatalf("MeasureProjectEnvelopes: %v", err)
	}
	for _, envelope := range envelopes {
		for _, frame := range envelope.Frames {
			truth := findFrameTruth(t, project.Truth, frame.SegmentID, frame.FrameIndex)
			if abs64(frame.Envelope-truth.ExpectedEnvelope) > DefaultFrameTolerance {
				t.Fatalf("%s frame %d: envelope=%d truth=%d", frame.SegmentID, frame.FrameIndex, frame.Envelope, truth.ExpectedEnvelope)
			}
			if frame.Energy != truth.Energy {
				t.Fatalf("%s frame %d: energy=%d truth=%d", frame.SegmentID, frame.FrameIndex, frame.Energy, truth.Energy)
			}
		}
	}
	calibration, err := CalibrateEnvelopes(project, envelopes)
	if err != nil {
		t.Fatalf("CalibrateEnvelopes: %v", err)
	}
	for _, segment := range calibration.Segments {
		truth := findSegmentTruth(t, project.Truth, segment.SegmentID)
		if segment.GainAdjustment != truth.GainAdjustment {
			t.Fatalf("%s adjustment=%d truth=%d", segment.SegmentID, segment.GainAdjustment, truth.GainAdjustment)
		}
		for i, frame := range segment.Frames {
			if frame.Envelope != truth.Calibrated[i] {
				t.Fatalf("%s frame %d calibrated=%d truth=%d", segment.SegmentID, i, frame.Envelope, truth.Calibrated[i])
			}
		}
		if !segment.Reached {
			t.Fatalf("%s did not reach target: got %d want %d (adjustment %d)", segment.SegmentID, segment.CalibratedLoudness, project.TargetLoudness, segment.GainAdjustment)
		}
	}
}

func TestExportLoadAllVersions(t *testing.T) {
	project, err := GenerateProject(42)
	if err != nil {
		t.Fatalf("GenerateProject: %v", err)
	}
	for _, version := range []string{Version1, Version2, Version3} {
		exported, err := ExportProject(project, version)
		if err != nil {
			t.Fatalf("export %s: %v", version, err)
		}
		loaded, err := LoadProject(exported.Data)
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		if loaded.Project.ID != project.ID || len(loaded.Project.PCM) != len(project.PCM) {
			t.Fatalf("%s core mismatch", version)
		}
		if mismatches := CompareSemantic(project, loaded.Project, version); len(mismatches) > 0 {
			t.Fatalf("%s semantic roundtrip mismatches: %+v", version, mismatches)
		}
		envelopes, err := MeasureProjectEnvelopes(loaded.Project)
		if err != nil {
			t.Fatalf("measure %s: %v", version, err)
		}
		for _, envelope := range envelopes {
			for _, frame := range envelope.Frames {
				truth := findFrameTruth(t, loaded.Project.Truth, frame.SegmentID, frame.FrameIndex)
				if abs64(frame.Envelope-truth.ExpectedEnvelope) > TruthTolerance(version) {
					t.Fatalf("%s %s/%d envelope=%d truth=%d", version, frame.SegmentID, frame.FrameIndex, frame.Envelope, truth.ExpectedEnvelope)
				}
			}
		}
		if _, err := CalibrateEnvelopes(loaded.Project, envelopes); err != nil {
			t.Fatalf("calibrate %s: %v", version, err)
		}
	}
}

func findFrameTruth(t *testing.T, truth *Truth, segmentID string, frameIndex int) FrameTruth {
	t.Helper()
	for _, frame := range truth.Frames {
		if frame.SegmentID == segmentID && frame.FrameIndex == frameIndex {
			return frame
		}
	}
	t.Fatalf("missing truth frame %s/%d", segmentID, frameIndex)
	return FrameTruth{}
}

func findSegmentTruth(t *testing.T, truth *Truth, segmentID string) SegmentTruth {
	t.Helper()
	for _, segment := range truth.Segments {
		if segment.SegmentID == segmentID {
			return segment
		}
	}
	t.Fatalf("missing truth segment %s", segmentID)
	return SegmentTruth{}
}
