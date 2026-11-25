package dialogue

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type ErrorCaseView struct {
	Name         string `json:"name"`
	ExpectedCode string `json:"expected_code"`
	ReturnedCode string `json:"returned_code,omitempty"`
	OK           bool   `json:"ok"`
	Message      string `json:"message,omitempty"`
	Path         string `json:"path,omitempty"`
	ClipID       string `json:"clip_id,omitempty"`
	Error        *Error `json:"error,omitempty"`
}

type InvalidationView struct {
	GainPlan           *InvalidationPlan `json:"gain_plan"`
	GainThreePathEqual bool              `json:"gain_three_path_equal"`
	TargetPlan         *InvalidationPlan `json:"target_plan"`
	TargetPathEqual    bool              `json:"target_path_equal"`
	FPSPlan            *InvalidationPlan `json:"fps_plan"`
	FPSPathEqual       bool              `json:"fps_path_equal"`
}

type PathView struct {
	FullFrameCount                int             `json:"full_frame_count"`
	Recovery                      *RecoveryReport `json:"recovery"`
	RecoveryEqualsFull            bool            `json:"recovery_equals_full"`
	RecoveryIncrementalEqualsFull bool            `json:"recovery_incremental_equals_full"`
	IncrementalEditEqual          bool            `json:"incremental_edit_equal"`
}

type ToleranceView struct {
	EnergyTolerance int64   `json:"energy_tolerance"`
	PassRate        float64 `json:"pass_rate"`
}

// DiagnosticData is the complete local page/API payload.
type DiagnosticData struct {
	OK             bool                     `json:"ok"`
	Checks         []Check                  `json:"checks"`
	Seed           int64                    `json:"seed"`
	Project        *Project                 `json:"project"`
	Truth          *TruthManifest           `json:"truth"`
	Frames         []FrameResult            `json:"frames"`
	Graph          *DependencyGraph         `json:"graph"`
	Scheduler      *SchedulerReport         `json:"scheduler"`
	RoundTrips     map[int]*RoundTripReport `json:"round_trips"`
	Drift          *PathDriftReport         `json:"drift"`
	Invalidation   InvalidationView         `json:"invalidation"`
	Paths          PathView                 `json:"paths"`
	ToleranceRates []ToleranceView          `json:"tolerance_rates"`
	ErrorCases     []ErrorCaseView          `json:"error_cases"`
}

// BuildDiagnosticData runs the deterministic sample and every self-check.
func BuildDiagnosticData() (*DiagnosticData, error) {
	project, truth, err := GenerateProject(GeneratorOptions{})
	if err != nil {
		return nil, err
	}
	docs, _, err := GenerateDocuments(project)
	if err != nil {
		return nil, err
	}
	for version, data := range docs {
		if _, loadErr := LoadDocument(data, "diagnostic-v"+strconv.Itoa(version)+".json"); loadErr != nil {
			return nil, loadErr
		}
	}

	frames, pErr := FullBuild(project)
	if pErr != nil {
		return nil, pErr
	}
	schedulerReport, sErr := RunScheduled(CloneProject(project), DefaultSchedulerConfig(), nil)
	if sErr != nil {
		return nil, sErr
	}
	rt1, err := VerifyRoundTrip(project, 1)
	if err != nil {
		return nil, err
	}
	rt2, err := VerifyRoundTrip(project, 2)
	if err != nil {
		return nil, err
	}
	drift, err := VerifyVersionPathDrift(project)
	if err != nil {
		return nil, err
	}

	invalidView, err := buildInvalidationView(project)
	if err != nil {
		return nil, err
	}
	pathView, err := buildPathView(project, frames)
	if err != nil {
		return nil, err
	}

	truthMap := truthFeatureMap(truth)
	tolerances := []int64{1000, 100, 2, 0}
	rates := make([]ToleranceView, 0, len(tolerances))
	for _, tol := range tolerances {
		rates = append(rates, ToleranceView{EnergyTolerance: tol, PassRate: energyPassRate(frames, truthMap, tol)})
	}

	data := &DiagnosticData{
		Seed:           FixedSeed,
		Project:        project,
		Truth:          truth,
		Frames:         frames,
		Graph:          BuildDependencyGraph(project),
		Scheduler:      schedulerReport,
		RoundTrips:     map[int]*RoundTripReport{1: rt1, 2: rt2},
		Drift:          drift,
		Invalidation:   invalidView,
		Paths:          pathView,
		ToleranceRates: rates,
		ErrorCases:     buildErrorCases(project, truth),
	}
	data.Checks = buildChecks(data)
	data.OK = true
	for _, check := range data.Checks {
		if !check.OK {
			data.OK = false
			break
		}
	}
	if len(data.Checks) == 0 {
		data.OK = true
	}
	return data, nil
}

func buildInvalidationView(base *Project) (InvalidationView, error) {
	view := InvalidationView{}

	gainProject := CloneProject(base)
	gainEngine := NewEngine(gainProject)
	if _, err := gainEngine.BuildAll(); err != nil {
		return view, err
	}
	plan, eErr := gainEngine.ApplyClipGainEdit("clip-01", gainProject.Clips[0].GainBP+200)
	if eErr != nil {
		return view, eErr
	}
	inc, pErr := gainEngine.RebuildPlan(plan)
	if pErr != nil {
		return view, pErr
	}
	fullGain, pErr := FullBuild(CloneProject(gainProject))
	if pErr != nil {
		return view, pErr
	}
	view.GainPlan = plan
	view.GainThreePathEqual = CompareFrames(inc, fullGain).Equal

	targetProject := CloneProject(base)
	targetEngine := NewEngine(targetProject)
	if _, err := targetEngine.BuildAll(); err != nil {
		return view, err
	}
	tplan, eErr := targetEngine.ApplyTargetEdit(68_000)
	if eErr != nil {
		return view, eErr
	}
	targetInc, pErr := targetEngine.RebuildPlan(tplan)
	if pErr != nil {
		return view, pErr
	}
	fullTarget, pErr := FullBuild(CloneProject(targetProject))
	if pErr != nil {
		return view, pErr
	}
	view.TargetPlan = tplan
	view.TargetPathEqual = CompareFrames(targetInc, fullTarget).Equal

	fpsFullProject := CloneProject(base)
	fpsFullProject.FrameRate = 30
	fpsFull, pErr := FullBuild(fpsFullProject)
	if pErr != nil {
		return view, pErr
	}
	fpsProject := CloneProject(base)
	fpsEngine := NewEngine(fpsProject)
	if _, err := fpsEngine.BuildAll(); err != nil {
		return view, err
	}
	fplan, eErr := fpsEngine.ApplyFrameRateEdit(30)
	if eErr != nil {
		return view, eErr
	}
	fpsInc, pErr := fpsEngine.RebuildPlan(fplan)
	if pErr != nil {
		return view, pErr
	}
	view.FPSPlan = fplan
	view.FPSPathEqual = CompareFrames(fpsInc, fpsFull).Equal
	return view, nil
}

func buildPathView(base *Project, full []FrameResult) (PathView, error) {
	project := CloneProject(base)
	dir, err := os.MkdirTemp("", "dialogue-checkpoint-*")
	if err != nil {
		return PathView{}, err
	}
	path := filepath.Join(dir, "checkpoint.json")
	store, err := NewFileCheckpointStore(path, project)
	if err != nil {
		return PathView{}, err
	}
	for i := 0; i < 2; i++ {
		out, pErr := ProcessClip(project, project.Clips[i], nil)
		if pErr != nil {
			return PathView{}, pErr
		}
		if err := store.SaveClip(out); err != nil {
			return PathView{}, err
		}
	}
	engine, report, rErr := RecoverOrBuild(project, path)
	if rErr != nil {
		return PathView{}, rErr
	}
	recoveryEqual := CompareFrames(full, engine.Results()).Equal

	plan, eErr := engine.ApplyClipGainEdit("clip-03", project.Clips[2].GainBP+160)
	if eErr != nil {
		return PathView{}, eErr
	}
	recoveryIncremental, pErr := engine.RebuildPlan(plan)
	if pErr != nil {
		return PathView{}, pErr
	}
	fullAfterEdit, pErr := FullBuild(CloneProject(project))
	if pErr != nil {
		return PathView{}, pErr
	}
	return PathView{
		FullFrameCount:                len(full),
		Recovery:                      report,
		RecoveryEqualsFull:            recoveryEqual,
		RecoveryIncrementalEqualsFull: CompareFrames(recoveryIncremental, fullAfterEdit).Equal,
	}, nil
}

func buildErrorCases(base *Project, truth *TruthManifest) []ErrorCaseView {
	cases := []ErrorCaseView{}
	collect := func(name, expected string, fn func() error) {
		view := ErrorCaseView{Name: name, ExpectedCode: expected}
		err := fn()
		var be *Error
		if errors.As(err, &be) {
			view.ReturnedCode = be.Code
			view.Message = be.Message
			view.Path = be.Path
			view.ClipID = be.ClipID
			view.Error = be
			view.OK = be.Code == expected
		} else if err != nil {
			view.Message = err.Error()
		}
		cases = append(cases, view)
	}

	collect("unknown schema version", CodeSchema, func() error {
		_, err := LoadDocument([]byte(`{"schema_version":404}`), "error-schema.json")
		return err
	})
	collect("integer overflow", CodeOvf, func() error {
		_, _, err := SamplesToMillis(1<<62, 24000, RoundFloor, "$.large_sample")
		return err
	})
	collect("unreachable loudness", CodeLoud, func() error {
		loud := CloneProject(base)
		loud.TargetAmpMicro = LinearScale
		loud.TargetLoudMDB = 0
		loud.MaxAdjustBP = 0
		features := map[string][]FrameFeature{}
		for _, clip := range truth.Clips {
			features[clip.ClipID] = clip.Features
		}
		_, err := CalibrateProject(loud, features)
		return err
	})
	collect("illegal state transition", CodeState, func() error {
		m := NewMachine([]string{"c"})
		return m.Transition("c", StateCommitted)
	})
	collect("decode truncation", CodeDecode, func() error {
		bad := CloneProject(base)
		bad.Clips[0].DeclaredSampleCount = len(bad.Clips[0].Samples) + 1
		_, err := ProcessClip(bad, bad.Clips[0], nil)
		return err
	})
	collect("interval overlap", CodeOver, func() error {
		bad := CloneProject(base)
		bad.Clips[1].StartSample = bad.Clips[0].StartSample
		return ValidateOverlap(bad.Clips)
	})
	collect("gain scale contradiction", CodePara, func() error {
		_, err := LoadDocument([]byte(`{"schema_version":3,"dialogue_id":"p","calibration":{"target_loud_mdb":-20000},"scenes":[{"name":"s","clips":[{"id":"c","start_sample":0,"gain_linear_micro":1000000,"gain_bp":12000,"pcm_s16":[0,1]}]}]}`), "para.json")
		return err
	})
	collect("single stage failure", CodeStage, func() error {
		bad := CloneProject(base)
		bad.Clips[2].ForceStageFailure = true
		_, err := ProcessClip(bad, bad.Clips[2], nil)
		return err
	})
	rt, rtErr := VerifyRoundTrip(base, 1)
	lossOK := rtErr == nil && rt != nil && rt.Loss != nil && rt.Loss.HasLoss()
	lossView := ErrorCaseView{
		Name:         "downgrade lossy report",
		ExpectedCode: CodeLossy,
		OK:           lossOK,
	}
	if lossOK {
		lossView.ReturnedCode = CodeLossy
		lossView.Message = "nested scenes and optional markers recorded as non-fatal loss items"
		lossView.Error = &Error{Code: CodeLossy, Message: lossView.Message, Details: rt.Loss.Losses}
	}
	cases = append(cases, lossView)
	return cases
}

func buildChecks(d *DiagnosticData) []Check {
	checks := []Check{}
	add := func(name string, ok bool, detail string) {
		checks = append(checks, Check{Name: name, OK: ok, Detail: detail})
	}

	add("three schema documents load", len(d.Project.Clips) == 4, "v1/v2/v3 mapped to unified model")
	add("v1 round trip", d.RoundTrips[1] != nil && d.RoundTrips[1].OK, "field residuals within tolerance")
	add("v2 round trip", d.RoundTrips[2] != nil && d.RoundTrips[2].OK, "field residuals within tolerance")
	add("version path independent", d.Drift != nil && d.Drift.OK, "v3->v1->v3 equals v3->v2->v1->v3")
	add("scheduler stable commits", d.Scheduler.State.CommittedClips == 4 && len(d.Scheduler.CommitOrder) == 4,
		"parallel extraction/calibration with ordered commits")
	add("recovery equals full", d.Paths.RecoveryEqualsFull, "committed intervals restored, remainder rebuilt")
	add("recovery incremental equals full", d.Paths.RecoveryIncrementalEqualsFull,
		"restored checkpoint plus precise edit matches one-shot rebuild")
	add("gain incremental equals full", d.Invalidation.GainThreePathEqual, "only clip calibration invalidated")
	add("target incremental equals full", d.Invalidation.TargetPathEqual, "all uncommitted calibration invalidated")
	add("fps incremental equals full", d.Invalidation.FPSPathEqual, "precise requantized ranges rebuilt")
	allErrors := true
	for _, ec := range d.ErrorCases {
		if !ec.OK {
			allErrors = false
		}
	}
	add("all business error codes located", allErrors, "E_SCHEMA/E_LOSSY/E_OVF/E_LOUD/E_STATE/E_DECODE/E_OVER/E_PARA/E_STAGE")
	pass := d.ToleranceRates[len(d.ToleranceRates)-1].PassRate
	add("exact integer truth match", pass == 1.0, "zero-tolerance energy pass rate")
	return checks
}
