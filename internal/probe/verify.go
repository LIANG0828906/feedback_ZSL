package probe

import (
	"lightprobe/internal/diag"
	"lightprobe/internal/scene"
)

type PrecisionCheck struct {
	ID        string     `json:"id"`
	RoomID    string     `json:"room_id,omitempty"`
	Kind      string     `json:"kind"`
	Expected  scene.Vec3 `json:"expected"`
	Actual    scene.Vec3 `json:"actual"`
	MaxError  float64    `json:"max_error"`
	Tolerance float64    `json:"tolerance"`
	Pass      bool       `json:"pass"`
}

type PrecisionReport struct {
	Pass          bool             `json:"pass"`
	ProbeChecks   int              `json:"probe_checks"`
	PixelChecks   int              `json:"pixel_checks"`
	JumpChecks    int              `json:"jump_checks"`
	MaxProbeError float64          `json:"max_probe_error"`
	MaxPixelError float64          `json:"max_pixel_error"`
	MaxJumpError  float64          `json:"max_jump_error"`
	Checks        []PrecisionCheck `json:"checks"`
	FirstMismatch *diag.Error      `json:"first_mismatch,omitempty"`
}

func (e *Engine) VerifyTruth() (*PrecisionReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	report := &PrecisionReport{Pass: true}
	addFailure := func(check PrecisionCheck, err *diag.Error) (*PrecisionReport, error) {
		check.Pass = false
		report.Checks = append(report.Checks, check)
		report.Pass = false
		if report.FirstMismatch == nil {
			report.FirstMismatch = err
		}
		return report, err
	}

	for _, expected := range e.scene.Truth.Probes {
		record, ok := e.records[expected.ProbeID]
		if !ok {
			err := diag.New(diag.CodeDiff, "truth probe missing: %s", expected.ProbeID).WithID(expected.ProbeID)
			return addFailure(PrecisionCheck{ID: expected.ProbeID, Kind: "probe", Pass: false}, err)
		}
		diff := MaxVecAbsDiff(record.Radiance, expected.Radiance)
		report.ProbeChecks++
		report.MaxProbeError = maxFloat(report.MaxProbeError, diff)
		check := PrecisionCheck{
			ID: expected.ProbeID, RoomID: record.RoomID, Kind: "probe",
			Expected: expected.Radiance, Actual: record.Radiance, MaxError: diff,
			Tolerance: e.scene.Tolerance.Radiance, Pass: diff <= e.scene.Tolerance.Radiance,
		}
		report.Checks = append(report.Checks, check)
		if !check.Pass {
			err := diffError(record.RoomID, expected.ProbeID, "radiance",
				"probe radiance error %.3g exceeds tolerance %.3g", diff, e.scene.Tolerance.Radiance)
			return addFailure(check, err)
		}
	}

	for _, expected := range e.scene.Truth.Pixels {
		actual, ok := e.pixelResults[expected.PixelID]
		if !ok {
			err := diag.New(diag.CodeDiff, "pixel result missing: %s", expected.PixelID).WithID(expected.PixelID)
			return addFailure(PrecisionCheck{ID: expected.PixelID, Kind: "pixel", Pass: false}, err)
		}
		if actual.Error != "" {
			err := diag.New(diag.CodeState, "pixel evaluation failed: %s", actual.Error).WithID(expected.PixelID)
			return addFailure(PrecisionCheck{ID: expected.PixelID, Kind: "pixel", Actual: actual.Color, Pass: false}, err)
		}
		diff := MaxVecAbsDiff(actual.Color, expected.Color)
		report.PixelChecks++
		report.MaxPixelError = maxFloat(report.MaxPixelError, diff)
		check := PrecisionCheck{
			ID: expected.PixelID, RoomID: actual.RoomID, Kind: "pixel_color",
			Expected: expected.Color, Actual: actual.Color, MaxError: diff,
			Tolerance: e.scene.Tolerance.Color, Pass: diff <= e.scene.Tolerance.Color,
		}
		report.Checks = append(report.Checks, check)
		if !check.Pass {
			err := diffError(actual.RoomID, expected.PixelID, "pixel_color",
				"pixel color error %.3g exceeds tolerance %.3g", diff, e.scene.Tolerance.Color)
			return addFailure(check, err)
		}
	}

	for _, expectedJump := range e.scene.Truth.Jumps {
		boundary, ok := findBoundary(e.scene, expectedJump.BoundaryID)
		if !ok {
			continue
		}
		ra, _, _, err := e.interpolateRawLocked(boundary.RoomA, expectedJump.Position)
		if err != nil {
			return report, err
		}
		positionB := expectedJump.Position
		positionB[boundary.Axis] = boundary.Position
		rb, _, _, err := e.interpolateRawLocked(boundary.RoomB, positionB)
		if err != nil {
			return report, err
		}
		var actualJump scene.Vec3
		for c := 0; c < 3; c++ {
			actualJump[c] = rb[c] - ra[c]
		}
		diff := MaxVecAbsDiff(actualJump, expectedJump.Jump)
		report.JumpChecks++
		report.MaxJumpError = maxFloat(report.MaxJumpError, diff)
		check := PrecisionCheck{
			ID: expectedJump.BoundaryID, RoomID: boundary.RoomA, Kind: "boundary_jump",
			Expected: expectedJump.Jump, Actual: actualJump, MaxError: diff,
			Tolerance: e.scene.Tolerance.Jump, Pass: diff <= e.scene.Tolerance.Jump,
		}
		report.Checks = append(report.Checks, check)
		if !check.Pass {
			err := diffError(boundary.RoomA, expectedJump.BoundaryID, "boundary_jump",
				"jump error %.3g exceeds tolerance %.3g", diff, e.scene.Tolerance.Jump)
			return addFailure(check, err)
		}
	}
	return report, nil
}

func findBoundary(s *scene.Scene, id string) (scene.BoundaryRule, bool) {
	for _, boundary := range s.Boundaries {
		if boundary.ID == id {
			return boundary, true
		}
	}
	return scene.BoundaryRule{}, false
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
