package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"dialogueforge/dialogue"
)

type versionCheck struct {
	Version            string `json:"version"`
	Loaded             bool   `json:"loaded"`
	SemanticMismatches int    `json:"semantic_mismatches"`
	EnvelopeMaxError   int64  `json:"envelope_max_error"`
	EnvelopeTolerance  int64  `json:"envelope_tolerance"`
	CalibrationMaxErr  int64  `json:"calibration_max_error"`
	CalibratedOK       bool   `json:"calibration_reached_all"`
	Lossy              int    `json:"lossy_issues"`
	Truncations        int    `json:"truncations"`
}

type report struct {
	Seed              int64                        `json:"seed"`
	DialogueID        string                       `json:"dialogue_id"`
	Frames            int                          `json:"frames"`
	Segments          int                          `json:"segments"`
	DirectEnvelopeErr int64                        `json:"direct_envelope_max_error"`
	DirectCalibration bool                         `json:"direct_calibration_reached"`
	Versions          []versionCheck               `json:"versions"`
	ThreePaths        *dialogue.VerificationReport `json:"three_paths"`
	Result            string                       `json:"result"`
}

func main() {
	seed := flag.Int64("seed", 42, "generator seed")
	dir := flag.String("dir", "", "working directory for checkpoints (default temporary)")
	flag.Parse()
	if *dir == "" {
		temp, err := os.MkdirTemp("", "dialogue-verify-*")
		if err != nil {
			fail(err)
		}
		defer os.RemoveAll(temp)
		*dir = temp
	}
	report, err := verify(*seed, *dir)
	if err != nil {
		fail(err)
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(data))
	fmt.Printf("\nACCEPTANCE %s: seed=%d frames=%d segments=%d\n", report.Result, report.Seed, report.Frames, report.Segments)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "verify:", err)
	os.Exit(1)
}

func verify(seed int64, dir string) (*report, error) {
	project, err := dialogue.GenerateProject(seed)
	if err != nil {
		return nil, err
	}
	envelopes, err := dialogue.MeasureProjectEnvelopes(project)
	if err != nil {
		return nil, err
	}
	calibration, err := dialogue.CalibrateEnvelopes(project, envelopes)
	if err != nil {
		return nil, err
	}
	report := &report{Seed: seed, DialogueID: project.ID, Frames: len(project.Truth.Frames), Segments: len(project.Segments())}
	truthFrames := map[string]dialogue.FrameTruth{}
	for _, frame := range project.Truth.Frames {
		truthFrames[fmt.Sprintf("%s:%d", frame.SegmentID, frame.FrameIndex)] = frame
	}
	for _, envelope := range envelopes {
		for _, frame := range envelope.Frames {
			expected := truthFrames[fmt.Sprintf("%s:%d", frame.SegmentID, frame.FrameIndex)]
			if diff := abs(frame.Envelope - expected.ExpectedEnvelope); diff > report.DirectEnvelopeErr {
				report.DirectEnvelopeErr = diff
			}
		}
	}
	report.DirectCalibration = calibration.Reached
	for _, version := range []string{dialogue.Version1, dialogue.Version2, dialogue.Version3} {
		exported, err := dialogue.ExportProject(project, version)
		if err != nil {
			return nil, err
		}
		check := versionCheck{Version: version, Lossy: len(exported.Lossy), Truncations: len(exported.Truncations)}
		loaded, err := dialogue.LoadProject(exported.Data)
		if err != nil {
			return nil, err
		}
		check.Loaded = true
		check.SemanticMismatches = len(dialogue.CompareSemantic(project, loaded.Project, version))
		check.EnvelopeTolerance = dialogue.TruthTolerance(version)
		loadedEnvelopes, err := dialogue.MeasureProjectEnvelopes(loaded.Project)
		if err != nil {
			return nil, err
		}
		loadedTruthFrames := map[string]dialogue.FrameTruth{}
		for _, frame := range loaded.Project.Truth.Frames {
			loadedTruthFrames[fmt.Sprintf("%s:%d", frame.SegmentID, frame.FrameIndex)] = frame
		}
		for _, envelope := range loadedEnvelopes {
			for _, frame := range envelope.Frames {
				expected := loadedTruthFrames[fmt.Sprintf("%s:%d", frame.SegmentID, frame.FrameIndex)]
				if diff := abs(frame.Envelope - expected.ExpectedEnvelope); diff > check.EnvelopeMaxError {
					check.EnvelopeMaxError = diff
				}
			}
		}
		loadedCalibration, err := dialogue.CalibrateEnvelopes(loaded.Project, loadedEnvelopes)
		if err != nil {
			return nil, err
		}
		check.CalibratedOK = loadedCalibration.Reached
		truthSegments := map[string]dialogue.SegmentTruth{}
		for _, segment := range loaded.Project.Truth.Segments {
			truthSegments[segment.SegmentID] = segment
		}
		for _, segment := range loadedCalibration.Segments {
			truth := truthSegments[segment.SegmentID]
			if diff := abs(segment.GainAdjustment - truth.GainAdjustment); diff > check.CalibrationMaxErr {
				check.CalibrationMaxErr = diff
			}
			for i, frame := range segment.Frames {
				if i < len(truth.Calibrated) {
					if diff := abs(frame.Envelope - truth.Calibrated[i]); diff > check.CalibrationMaxErr {
						check.CalibrationMaxErr = diff
					}
				}
			}
		}
		report.Versions = append(report.Versions, check)
	}
	paths, err := dialogue.VerifyThreePaths(project, []dialogue.Edit{
		{Kind: dialogue.EditSegmentGain, SegmentID: "seg-02", Value: 9900},
		{Kind: dialogue.EditTargetLoudness, Value: 5700},
		{Kind: dialogue.EditSegmentGain, SegmentID: "seg-04", Value: 9000},
	}, dir)
	if err != nil {
		return nil, err
	}
	report.ThreePaths = paths
	report.Result = "PASS"
	for _, check := range report.Versions {
		if !check.Loaded || check.SemanticMismatches != 0 || check.EnvelopeMaxError > check.EnvelopeTolerance ||
			!check.CalibratedOK || check.CalibrationMaxErr > check.EnvelopeTolerance {
			report.Result = "FAIL"
		}
	}
	if report.DirectEnvelopeErr > dialogue.DefaultFrameTolerance || !report.DirectCalibration || !paths.Equal {
		report.Result = "FAIL"
	}
	if report.Result != "PASS" {
		return report, fmt.Errorf("acceptance checks failed")
	}
	return report, nil
}

func abs(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}
