package engine

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/generator"
)

func TestEvaluationIndependentOfInputOrderAndGoroutine(t *testing.T) {
	base := generator.GenerateProjects(generator.DefaultSeed)[0]
	params := domain.DefaultParameters()
	expected, err := EvaluateProject(base, params)
	if err != nil {
		t.Fatal(err)
	}
	shuffled := domain.CloneProject(base)
	for i := range shuffled.Segments {
		seg := &shuffled.Segments[i]
		reverseChannels(seg.Channels)
		reverseRules(seg.Priorities)
		for j := range seg.Channels {
			reverseLayers(seg.Channels[j].Layers)
		}
	}
	shuffledResult, err := EvaluateProject(shuffled, params)
	if err != nil {
		t.Fatal(err)
	}
	if cmp, err := CompareResultSets(expected, shuffledResult); err != nil || !cmp.Equal {
		t.Fatalf("input order changed result: %v cmp=%+v", err, cmp)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			result, err := EvaluateProject(base, params)
			if err != nil {
				errs <- err
				return
			}
			cmp, cmpErr := CompareResultSets(expected, result)
			if cmpErr != nil || !cmp.Equal {
				errs <- fmt.Errorf("worker %d result differs: %v %+v", worker, cmpErr, cmp)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestConflictDecisionsAreDeterministic(t *testing.T) {
	base := generator.GenerateProjects(generator.DefaultSeed)[0]
	result, err := EvaluateProject(base, domain.DefaultParameters())
	if err != nil {
		t.Fatal(err)
	}
	decisionCount := 0
	for _, seg := range result.Segments {
		for _, sample := range seg.Samples {
			decisionCount += len(sample.Decisions)
		}
	}
	if decisionCount == 0 {
		t.Fatal("sample must exercise channel override decisions")
	}
	second, err := EvaluateProject(domain.CloneProject(base), domain.DefaultParameters())
	if err != nil {
		t.Fatal(err)
	}
	if cmp, err := CompareResultSets(result, second); err != nil || !cmp.Equal {
		t.Fatalf("decision sequence not deterministic: %v %+v", err, cmp)
	}
	first := result.Segments[0].Samples[0].Decisions[0]
	if first.Channel == "" || first.Overridden == "" || first.Basis == "" || first.OverriddenValue == 0 {
		t.Fatalf("incomplete decision detail: %+v", first)
	}
}

func TestCompareLocatesFirstDifferenceWithBothValues(t *testing.T) {
	base := generator.GenerateProjects(generator.DefaultSeed)[0]
	left, err := EvaluateProject(base, domain.DefaultParameters())
	if err != nil {
		t.Fatal(err)
	}
	right := cloneResultForTest(left)
	right.Segments[0].Samples[3].Values[domain.TargetVelocity] += 0.25
	cmp, cmpErr := CompareResultSets(left, right)
	if cmpErr == nil || domain.CodeOf(cmpErr) != domain.CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", cmpErr)
	}
	if cmp.First == nil || cmp.First.Tick != left.Segments[0].Samples[3].Tick || cmp.First.Field != "value.velocity" {
		t.Fatalf("wrong first difference: %+v", cmp.First)
	}
	if cmp.First.Left == cmp.First.Right {
		t.Fatalf("E_DIFF must include both values: %+v", cmp.First)
	}
}

func TestPlaybackAtIsStatelessBySampleTime(t *testing.T) {
	project := generator.GenerateProjects(generator.DefaultSeed)[0]
	params := domain.DefaultParameters()
	result, err := EvaluateProject(project, params)
	if err != nil {
		t.Fatal(err)
	}
	for _, seg := range result.Segments {
		for _, sample := range seg.Samples {
			playback, err := PlaybackAt(project, params, seg.SegmentID, sample.Beat)
			if err != nil {
				t.Fatal(err)
			}
			for target, expected := range sample.Values {
				if playback.Values[target] != expected {
					t.Fatalf("playback drifted for %s tick %d: %v != %v", target, sample.Tick, playback.Values[target], expected)
				}
				if playback.Sources[target].Channel != sample.Sources[target].Channel {
					t.Fatalf("playback source drifted at tick %d", sample.Tick)
				}
			}
		}
	}
}

func TestPrecisionTolerancePassingSetIsMonotone(t *testing.T) {
	project := generator.GenerateProjects(generator.DefaultSeed)[0]
	result, err := EvaluateProject(project, domain.DefaultParameters())
	if err != nil {
		t.Fatal(err)
	}
	levels := []float64{1e-6, 1e-9, 1e-12}
	var previous map[PointKey]struct{}
	for _, level := range levels {
		report, _ := ValidatePrecision(project, result, level)
		set := PassingPointSet(report)
		if previous != nil && !SetIsSubset(set, previous) {
			t.Fatalf("passing set grew when tolerance tightened to %g", level)
		}
		previous = set
	}
}

func TestPrecisionReportsToleranceAndSourceMismatchSeparately(t *testing.T) {
	project := generator.GenerateProjects(generator.DefaultSeed)[0]
	params := domain.DefaultParameters()
	result, err := EvaluateProject(project, params)
	if err != nil {
		t.Fatal(err)
	}
	badValue := cloneResultForTest(result)
	badValue.Segments[0].Samples[2].Values[domain.TargetVibrato] += 0.5
	report, err := ValidatePrecision(project, badValue, 1e-9)
	if domain.CodeOf(err) != domain.CodePrecision {
		t.Fatalf("expected E_PREC, got %v", err)
	}
	if report.FailedPoints == 0 {
		t.Fatal("precision report did not include failed point")
	}
	appErr, _ := domain.AsAppError(err)
	if appErr == nil || appErr.SegmentID == "" || appErr.Tick == nil {
		t.Fatalf("E_PREC missing segment/tick: %+v", appErr)
	}

	badSource := cloneResultForTest(result)
	sample := &badSource.Segments[0].Samples[2]
	source := sample.Sources[domain.TargetVelocity]
	source.Channel = "wrong.source"
	sample.Sources[domain.TargetVelocity] = source
	sourceReport, sourceErr := ValidatePrecision(project, badSource, 1e-9)
	if domain.CodeOf(sourceErr) != domain.CodePrecision {
		t.Fatalf("expected E_PREC for source mismatch, got %v", sourceErr)
	}
	if sourceReport.SourceMismatches == 0 || sourceReport.FailedPoints != 0 {
		t.Fatalf("source mismatch was not isolated: %+v", sourceReport)
	}
}

func reverseChannels(values []domain.Channel) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}

func reverseRules(values []domain.PriorityRule) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}

func reverseLayers(values []domain.Layer) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}

func cloneResultForTest(in domain.ResultSet) domain.ResultSet {
	b, _ := domain.CanonicalJSON(in)
	var out domain.ResultSet
	_ = json.Unmarshal(b, &out)
	return out
}
