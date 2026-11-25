package dialoguecal

import "math"

type CriticalCaseReport struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	Results     []FrameResult     `json:"results"`
	Comparisons []FrameComparison `json:"comparisons"`
	AllWithin   bool              `json:"all_within"`
}

type OverflowCheck struct {
	Name         string `json:"name"`
	ExpectedCode Code   `json:"expected_code"`
	GotCode      Code   `json:"got_code,omitempty"`
	Saturated    bool   `json:"saturated"`
	OK           bool   `json:"ok"`
	Detail       string `json:"detail,omitempty"`
}

type CriticalSuite struct {
	Cases          []CriticalCaseReport `json:"cases"`
	OverflowChecks []OverflowCheck      `json:"overflow_checks"`
}

func syntheticProject(id string, pcm []int16, gainPermille, targetPermille, capPermille int) *Project {
	p := &Project{
		ProjectID: id, Seed: 99, SampleRateHz: 16000, PCM: pcm,
		FramesPerSecond: 50, FrameShiftSamples: 320, FrameWindowSamples: 320,
		EnergyCapPermille: capPermille,
		Segments:          []Segment{{ID: "seg-c", Index: 0, StartSample: 0, EndSample: len(pcm), GainPermille: gainPermille, TargetLevelPermille: targetPermille}},
		Phonemes:          []Phoneme{{ID: "ph-c", SegmentID: "seg-c", SyllableID: "syl-c", StartSample: 0, EndSample: len(pcm), Class: ClassVowel, F0StartHzQ8: 140 * 256, F0EndHzQ8: 140 * 256, WeightPermille: 1000}},
	}
	return p
}

func runCriticalProject(id, description string, p *Project) (CriticalCaseReport, error) {
	engine, err := NewEngine(p)
	if err != nil {
		return CriticalCaseReport{}, err
	}
	report, err := engine.ComputeAll()
	if err != nil {
		return CriticalCaseReport{}, err
	}
	truth := GenerateTruth(p, engine.Frames())
	cmps := CompareResults(report.Results, truth)
	allWithin := true
	for _, cmp := range cmps {
		if !cmp.AllWithin {
			allWithin = false
		}
	}
	return CriticalCaseReport{ID: id, Description: description, Results: report.Results, Comparisons: cmps, AllWithin: allWithin}, nil
}

func GenerateCriticalSuite() *CriticalSuite {
	suite := &CriticalSuite{}

	loud := make([]int16, 1600)
	for i := range loud {
		loud[i] = int16(22000 * math.Sin(2*math.Pi*140*float64(i)/16000))
	}
	if report, err := runCriticalProject("continuous_limiting", "five consecutive loud frames hit the per-frame energy cap", syntheticProject("critical-limit", loud, 1000, 100, 200)); err == nil {
		suite.Cases = append(suite.Cases, report)
	}

	alternating := make([]int16, 3200)
	for frame := 0; frame < 10; frame++ {
		amplitude := 3500.0
		if frame%2 == 0 {
			amplitude = 13000
		}
		for i := 0; i < 320; i++ {
			idx := frame*320 + i
			alternating[idx] = int16(amplitude * math.Sin(2*math.Pi*135*float64(i)/16000))
		}
	}
	if report, err := runCriticalProject("repeated_recalibration", "alternating frame levels force repeated recalibration transitions", syntheticProject("critical-jumps", alternating, 1000, 110, 500)); err == nil {
		suite.Cases = append(suite.Cases, report)
	}

	_, strictErr := CheckedMulQ15(Q15(math.MaxInt32), Q15(math.MaxInt32), "critical.overflow")
	be, ok := AsBusinessError(strictErr)
	check := OverflowCheck{Name: "checked_mul_int32_max_squared", ExpectedCode: CodeFixedOverflow}
	if ok {
		check.GotCode = be.Code
		check.OK = be.Code == CodeFixedOverflow
		check.Detail = be.Detail
	}
	_, sat := MulQ15(Q15(math.MaxInt32), Q15(math.MaxInt32))
	check.Saturated = sat != nil
	check.OK = check.OK && check.Saturated
	suite.OverflowChecks = append(suite.OverflowChecks, check)

	return suite
}
