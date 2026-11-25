package dialogue

import "fmt"

// Declared round-trip tolerances.
const (
	RoundTripGainToleranceBP   int64 = 10
	RoundTripAmpToleranceMicro int64 = 1_000
	RoundTripLoudToleranceMDB  int64 = 100
	RoundTripSampleTolerance   int64 = 1
)

// FieldResidual records one field's downgrade/upgrade residual.
type FieldResidual struct {
	Path      string `json:"path"`
	Original  int64  `json:"original"`
	RoundTrip int64  `json:"round_trip"`
	Delta     int64  `json:"delta"`
	Tolerance int64  `json:"tolerance"`
	OK        bool   `json:"ok"`
}

// RoundTripReport is the field-by-field export/load residual record.
type RoundTripReport struct {
	TargetVersion int             `json:"target_version"`
	SourceID      string          `json:"source_id"`
	OK            bool            `json:"ok"`
	Loss          *LossReport     `json:"loss,omitempty"`
	Residuals     []FieldResidual `json:"residuals"`
}

func (r *RoundTripReport) add(path string, original, roundTrip, tolerance int64) {
	delta := abs64(original - roundTrip)
	res := FieldResidual{
		Path:      path,
		Original:  original,
		RoundTrip: roundTrip,
		Delta:     delta,
		Tolerance: tolerance,
		OK:        delta <= tolerance,
	}
	r.Residuals = append(r.Residuals, res)
	if !res.OK {
		r.OK = false
	}
}

// VerifyRoundTrip exports to version, loads that artifact and compares fields.
func VerifyRoundTrip(p *Project, version int) (*RoundTripReport, error) {
	sourceID := fmt.Sprintf("roundtrip-dialogue-v%d.json", version)
	data, loss, err := ExportProject(p, version)
	if err != nil {
		return nil, err
	}
	loaded, err := LoadDocument(data, sourceID)
	if err != nil {
		return nil, err
	}
	report := &RoundTripReport{
		TargetVersion: version,
		SourceID:      sourceID,
		OK:            true,
		Loss:          loss,
		Residuals:     []FieldResidual{},
	}
	report.add("$.session_gain_bp", p.SessionGainBP, loaded.SessionGainBP, 0)
	report.add("$.target_amp_micro", p.TargetAmpMicro, loaded.TargetAmpMicro, RoundTripAmpToleranceMicro)
	report.add("$.target_loud_mdb", p.TargetLoudMDB, loaded.TargetLoudMDB, RoundTripLoudToleranceMDB)
	report.add("$.frame_rate", int64(p.FrameRate), int64(loaded.FrameRate), 0)
	report.add("$.sample_rate", int64(p.SampleRate), int64(loaded.SampleRate), 0)
	if len(p.Clips) != len(loaded.Clips) {
		report.OK = false
		report.Residuals = append(report.Residuals, FieldResidual{
			Path: "$.clips", Original: int64(len(p.Clips)), RoundTrip: int64(len(loaded.Clips)),
			Delta: int64(abs(len(p.Clips) - len(loaded.Clips))), Tolerance: 0, OK: false,
		})
		return report, nil
	}
	for i, originalClip := range p.Clips {
		loadedClip := loaded.Clips[i]
		path := fmt.Sprintf("$.clips[%d]", i)
		if originalClip.ID != loadedClip.ID {
			report.OK = false
			report.Residuals = append(report.Residuals, FieldResidual{
				Path: path + ".id", OK: false,
			})
		}
		report.add(path+".gain_bp", originalClip.GainBP, loadedClip.GainBP, RoundTripGainToleranceBP)
		report.add(path+".start_sample", originalClip.StartSample, loadedClip.StartSample, RoundTripSampleTolerance)
		report.add(path+".sample_count", int64(len(originalClip.Samples)), int64(len(loadedClip.Samples)), 0)
		if originalClip.AudioHash != loadedClip.AudioHash {
			report.OK = false
			report.Residuals = append(report.Residuals, FieldResidual{
				Path: path + ".audio_hash", Delta: 1, Tolerance: 0, OK: false,
			})
		}
	}
	return report, nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// PathDriftReport compares two downgrade/upgrade paths back to schema v3.
type PathDriftReport struct {
	OK    bool            `json:"ok"`
	PathA []FieldResidual `json:"path_a"`
	PathB []FieldResidual `json:"path_b"`
}

// VerifyVersionPathDrift ensures conversion does not depend on version path.
func VerifyVersionPathDrift(p *Project) (*PathDriftReport, error) {
	pathA, err := RoundTripThrough(p, []int{1, 3})
	if err != nil {
		return nil, err
	}
	pathB, err := RoundTripThrough(p, []int{2, 1, 3})
	if err != nil {
		return nil, err
	}
	report := &PathDriftReport{OK: true, PathA: []FieldResidual{}, PathB: []FieldResidual{}}
	addDrift := func(collection *[]FieldResidual, path string, a, b, tol int64) {
		r := FieldResidual{Path: path, Original: a, RoundTrip: b, Delta: abs64(a - b), Tolerance: tol}
		r.OK = r.Delta <= tol
		if !r.OK {
			report.OK = false
		}
		*collection = append(*collection, r)
	}
	addDrift(&report.PathA, "session_gain", p.SessionGainBP, pathA.SessionGainBP, 0)
	addDrift(&report.PathB, "session_gain", pathA.SessionGainBP, pathB.SessionGainBP, 0)
	addDrift(&report.PathA, "target_amp", p.TargetAmpMicro, pathA.TargetAmpMicro, RoundTripAmpToleranceMicro)
	addDrift(&report.PathB, "target_amp_between_paths", pathA.TargetAmpMicro, pathB.TargetAmpMicro, RoundTripAmpToleranceMicro)
	for i := range p.Clips {
		addDrift(&report.PathA, fmt.Sprintf("clip_%s.gain", p.Clips[i].ID),
			p.Clips[i].GainBP, pathA.Clips[i].GainBP, RoundTripGainToleranceBP)
		addDrift(&report.PathB, fmt.Sprintf("clip_%s.gain_between_paths", p.Clips[i].ID),
			pathA.Clips[i].GainBP, pathB.Clips[i].GainBP, 0)
	}
	return report, nil
}

// RoundTripThrough exports/loads through a sequence of schema versions.
func RoundTripThrough(p *Project, versions []int) (*Project, error) {
	current := p
	for _, version := range versions {
		data, _, err := ExportProject(current, version)
		if err != nil {
			return nil, err
		}
		next, err := LoadDocument(data, fmt.Sprintf("path-v%d.json", version))
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}
