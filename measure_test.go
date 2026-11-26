package loudness

import (
	"testing"
)

func TestTruthMatchesAllThreeVersions(t *testing.T) {
	cases, err := GenerateAll(20261127)
	if err != nil {
		t.Fatal(err)
	}
	for _, generated := range cases {
		mismatch, err := VerifyTruth(generated.Project, generated.Truth)
		if err != nil {
			t.Fatalf("%s verify truth: %v", generated.Version, err)
		}
		if mismatch != nil {
			t.Fatalf("%s truth mismatch: %v", generated.Version, mismatch)
		}
	}
}

func TestFrameEnvelopeTruncationsPeaksAndDeterminism(t *testing.T) {
	generated := mustGeneratedV3(t)
	report, err := MeasureAndCalibrate(generated.Project)
	if err != nil {
		t.Fatal(err)
	}
	for _, clip := range generated.Project.Clips {
		result := report.Clips[clip.ID]
		if len(result.Frames) == 0 {
			t.Fatalf("clip %d has no frames", clip.ID)
		}
		for _, frame := range result.Frames {
			if frame.StartSample < clip.StartSample || frame.EndSample > clip.EndSample || frame.EndSample <= frame.StartSample {
				t.Fatalf("clip %d frame %d interval outside clip", clip.ID, frame.FrameIndex)
			}
			if frame.PeakSample < frame.StartSample || frame.PeakSample >= frame.EndSample {
				t.Fatalf("clip %d frame %d peak outside interval", clip.ID, frame.FrameIndex)
			}
			if len(frame.Truncations) < 5 {
				t.Fatalf("clip %d frame %d has %d truncation traces", clip.ID, frame.FrameIndex, len(frame.Truncations))
			}
			foundMeasuredPath := false
			for _, trace := range frame.Truncations {
				if trace.Path != "" && trace.Operation != "" {
					foundMeasuredPath = true
				}
			}
			if !foundMeasuredPath {
				t.Fatalf("clip %d frame %d truncation path missing", clip.ID, frame.FrameIndex)
			}
		}
		if absInt64(result.CalibratedSegmentDBQ8-generated.Project.TargetLoudnessDBQ8) > generated.Project.LoudnessToleranceQ8+2 {
			t.Fatalf("clip %d calibrated %d target %d", clip.ID, result.CalibratedSegmentDBQ8, generated.Project.TargetLoudnessDBQ8)
		}
	}
	second, err := MeasureAndCalibrate(generated.Project)
	if err != nil {
		t.Fatal(err)
	}
	for _, clip := range generated.Project.Clips {
		if report.Clips[clip.ID].EnvelopeDigest != second.Clips[clip.ID].EnvelopeDigest ||
			report.Clips[clip.ID].GainAdjustmentQ12 != second.Clips[clip.ID].GainAdjustmentQ12 {
			t.Fatalf("clip %d is not deterministic", clip.ID)
		}
	}
}

func TestFrameParameterErrors(t *testing.T) {
	generated := mustGeneratedV3(t)
	project := CloneProject(generated.Project)
	project.FrameLength = 0
	_, err := MeasureClip(project, project.Clips[0])
	if !HasCode(err, EPara) {
		t.Fatalf("expected E_PARA for frame length, got %v", err)
	}
	domainErr, _ := AsDomainError(err)
	if domainErr.Field != "frameLength" || domainErr.LegalMin != 1 || domainErr.LegalMax != MaxFrameLength {
		t.Fatalf("frame-length diagnostics: %+v", domainErr)
	}
	project.FrameLength = 128
	project.FrameShift = 129
	_, err = MeasureClip(project, project.Clips[0])
	if !HasCode(err, EPara) {
		t.Fatalf("expected E_PARA for frame shift, got %v", err)
	}
	domainErr, _ = AsDomainError(err)
	if domainErr.Field != "frameShift" || domainErr.LegalMax != 128 {
		t.Fatalf("frame-shift diagnostics: %+v", domainErr)
	}
}

func TestLoudnessUnreachableError(t *testing.T) {
	generated := mustGeneratedV3(t)
	project := CloneProject(generated.Project)
	project.TargetLoudnessDBQ8 = 0
	measurements, err := MeasureProject(project)
	if err != nil {
		t.Fatal(err)
	}
	report, err := CalibrateProject(project, measurements)
	if !HasCode(err, ELoud) {
		t.Fatalf("expected E_LOUD, got %v", err)
	}
	if report == nil || len(report.Clips) != len(project.Clips) {
		t.Fatal("partial calibration report missing")
	}
	domainErr, _ := AsDomainError(err)
	if domainErr.Target != 0 || domainErr.Reachable >= 0 || len(domainErr.LimitedClips) == 0 {
		t.Fatalf("E_LOUD diagnostics: %+v", domainErr)
	}
}
