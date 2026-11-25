package fog

import (
	"context"
	"path/filepath"
)

type InjectionCheck struct {
	ExpectedCode string     `json:"expected_code"`
	ActualCode   string     `json:"actual_code"`
	MediumID     string     `json:"medium_id,omitempty"`
	Tile         *TileCoord `json:"tile,omitempty"`
	RayID        string     `json:"ray_id,omitempty"`
	Passed       bool       `json:"passed"`
	Message      string     `json:"message,omitempty"`
}

type VerificationReport struct {
	Success        bool                      `json:"success"`
	Seed           int64                     `json:"seed"`
	Precision      *PrecisionReport          `json:"precision"`
	SchedulerTiles int                       `json:"scheduler_tiles"`
	QueueRecords   int                       `json:"queue_records"`
	Preview        *PreviewSummary           `json:"preview"`
	ThreeWay       *ThreeWaySummary          `json:"three_way"`
	Injections     map[string]InjectionCheck `json:"injections"`
	Failure        *Error                    `json:"failure,omitempty"`
}

func RunVerificationReport(ctx context.Context, artifactDir string) (*VerificationReport, error) {
	sample, err := GenerateSample()
	if err != nil {
		return nil, err
	}
	report := &VerificationReport{Seed: sample.Scene.Seed, Injections: map[string]InjectionCheck{}}
	report.Precision = VerifyPrecision(sample, []int{8, 12, 18, 32, 48, sample.Scene.Recipe.MaxStepsPerRay})

	scheduler := NewScheduler(SchedulerConfig{MaxParallelTiles: 2, RayBufferPerWorker: 32, ShadowBudgetTotal: 220_000})
	schedulerReport := scheduler.Run(ctx, &sample.Scene, TileGrid(sample.Scene.Viewport, sample.Scene.Recipe), IntegrationOptions{})
	report.SchedulerTiles = len(schedulerReport.Tiles)
	report.QueueRecords = len(schedulerReport.Queue)
	if len(schedulerReport.Failures) != 0 {
		report.Failure = firstSchedulerFailure(schedulerReport.Failures)
		return report, nil
	}

	preview, err := RenderPreview(&sample.Scene)
	if err != nil {
		return nil, err
	}
	exact, err := RenderExact(&sample.Scene)
	if err != nil {
		return nil, err
	}
	EvaluatePreview(&sample.Scene, preview, exact)
	report.Preview = previewSummary(preview)

	threeWay, err := RunThreeWayConsistency(ctx, &sample.Scene, filepath.Join(artifactDir, "checkpoints"), SchedulerConfig{MaxParallelTiles: 2, RayBufferPerWorker: 48, ShadowBudgetTotal: 220_000})
	if err != nil {
		return nil, err
	}
	report.ThreeWay = &ThreeWaySummary{
		Equal: threeWay.Equal, FirstDiff: threeWay.FirstDiff, Sequence: threeWay.Realtime.Sequence,
		ResumeReused: threeWay.Resume.ReusedTiles, ResumeRecomputed: threeWay.Resume.RecomputedTiles,
		Stale: threeWay.Resume.StaleCheckpoints,
	}

	report.Injections["E_SCHEMA"] = checkSchemaInjection(sample)
	report.Injections["E_SEG"] = checkSegInjection()
	report.Injections["E_ITER"] = checkIterInjection(sample)
	report.Injections["E_DECODE"] = checkDecodeInjection(sample)
	report.Injections["E_ORDER"] = checkOrderInjection(sample)
	report.Injections["E_DENS"] = checkDensityInjection(sample)
	report.Injections["E_BLOCK"] = checkBlockInjection(ctx, sample)

	report.Success = report.Precision.TolerancePassed && report.Precision.Monotonic &&
		report.Preview.WithinTolerance && report.Preview.WithinDelay &&
		report.ThreeWay.Equal && allInjectionsPass(report.Injections)
	if !report.Success && report.Failure == nil {
		if report.ThreeWay.FirstDiff != nil {
			report.Failure = report.ThreeWay.FirstDiff
		} else if report.Preview.Divergence != nil {
			report.Failure = report.Preview.Divergence
		}
	}
	return report, nil
}

func allInjectionsPass(checks map[string]InjectionCheck) bool {
	for _, check := range checks {
		if !check.Passed {
			return false
		}
	}
	return true
}

func checkSchemaInjection(sample *GeneratedSample) InjectionCheck {
	sample.Scene.Recipe.MaxStepsPerRay = 0
	err := ValidateRecipe(&sample.Scene.Recipe)
	fe := AsError(err)
	return injectionResult(CodeSchema, fe, fe != nil && fe.Field == "recipe.max_steps_per_ray")
}

func checkSegInjection() InjectionCheck {
	err := ValidateSegmentOrder("verify-ray", []RaySegment{{MediumIDs: []string{"a", "b"}, PriorityKey: "b+a", Near: SegmentEndpoint{Fixed: 1}, Far: SegmentEndpoint{Fixed: 2}}})
	fe := AsError(err)
	check := injectionResult(CodeSeg, fe, fe != nil && fe.RayID == "verify-ray")
	if fe != nil && len(fe.MediumPair) == 2 {
		check.MediumID = fe.MediumPair[0]
	}
	return check
}

func checkIterInjection(sample *GeneratedSample) InjectionCheck {
	var fe *Error
	for _, truth := range sample.Truth.Rays {
		err := AssertRayTruthWithinTolerance(&sample.Scene, truth, IntegrationOptions{StepBudget: 8, ShadowBudget: sample.Scene.Recipe.ShadowBudgetPerRay})
		if candidate := AsError(err); candidate != nil && candidate.Code == CodeIter {
			fe = candidate
			break
		}
	}
	return injectionResult(CodeIter, fe, fe != nil && fe.RayID != "" && fe.Deviation > 0)
}

func checkDecodeInjection(sample *GeneratedSample) InjectionCheck {
	document, _ := EncodeMedium(sample.Scene.Media[0])
	corrupt, _, _ := CorruptEncodedMedium(document)
	_, err := DecodeMedium(corrupt)
	fe := AsError(err)
	check := injectionResult(CodeDecode, fe, fe != nil && fe.MediumID == sample.Scene.Media[0].ID)
	if fe != nil {
		check.MediumID = fe.MediumID
	}
	return check
}

func checkOrderInjection(sample *GeneratedSample) InjectionCheck {
	bad := sample.Scene
	bad.CameraTrack.Samples = append([]CameraSample(nil), sample.Scene.CameraTrack.Samples...)
	bad.CameraTrack.Samples[1].LogicalTime = bad.CameraTrack.Samples[0].LogicalTime - 1
	err := ValidateScene(&bad)
	return injectionResult(CodeOrder, AsError(err), AsError(err) != nil)
}

func checkDensityInjection(sample *GeneratedSample) InjectionCheck {
	graph := BuildDependencyGraph(&sample.Scene)
	_, err := PlanVoxelEdit(&sample.Scene, graph, "fog-a", [3]int{0, 0, 0}, 999)
	fe := AsError(err)
	check := injectionResult(CodeDensity, fe, fe != nil && fe.MediumID == "fog-a")
	tile := TileCoord{X: 1, Y: 1}
	check.Tile = &tile
	return check
}

func checkBlockInjection(ctx context.Context, sample *GeneratedSample) InjectionCheck {
	scheduler := NewScheduler(SchedulerConfig{MaxParallelTiles: 1, RayBufferPerWorker: 64, ShadowBudgetTotal: 220_000})
	tile := TileCoord{X: 1, Y: 1}
	scheduler.Faults.FailNext(tile, 1)
	report := scheduler.Run(ctx, &sample.Scene, []TileCoord{tile}, IntegrationOptions{})
	fe := report.Failures[tileKey(tile)]
	check := injectionResult(CodeBlock, fe, fe != nil && fe.Tile != nil && *fe.Tile == tile)
	check.Tile = &tile
	return check
}

func injectionResult(expected string, fe *Error, passed bool) InjectionCheck {
	check := InjectionCheck{ExpectedCode: expected, Passed: passed}
	if fe != nil {
		check.ActualCode = fe.Code
		check.Message = fe.Message
		check.MediumID = fe.MediumID
		check.RayID = fe.RayID
		if fe.Tile != nil {
			tile := *fe.Tile
			check.Tile = &tile
		}
	}
	return check
}
