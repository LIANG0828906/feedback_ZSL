package stage

import (
	"context"
	"fmt"
	"reflect"
	"sort"
)

type RunOptions struct {
	Workers  int
	Limit    int
	Sampling SamplingConfig
	Store    *CheckpointStore
	Previous *Project
}

type windowComputation struct {
	result   WindowResult
	window   *WindowStateMachine
	fixtures map[string]*FixtureStateMachine
	err      error
}

type poseRecord struct {
	pose     FixturePose
	cacheKey string
}

// RunProject computes windows in parallel but commits results in stable window
// order. Existing checkpoints are reused only when their sampling config and
// complete dependency keys match the current project.
func RunProject(ctx context.Context, p *Project, opts RunOptions) (RunResult, error) {
	if p == nil {
		return RunResult{}, domainError(CodeValue, "nil project", "project")
	}
	if err := ValidateProject(p); err != nil {
		return RunResult{}, err
	}
	cfg := opts.Sampling
	if len(cfg.DistancesMM) == 0 && cfg.Density == 0 && cfg.CellMM == 0 {
		cfg = ExactSampling()
	}
	if err := validateSampling(cfg, ""); err != nil {
		if de := AsError(err); de != nil && de.FixtureID == "" && len(p.Fixtures) > 0 {
			ids := sortedFixtureIDs(p)
			de.FixtureID = ids[0]
		}
		return RunResult{}, err
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = 1
	}
	windows := append([]SampleWindow(nil), p.Timeline.Windows...)
	sort.Slice(windows, func(i, j int) bool { return windows[i].Number < windows[j].Number })
	if opts.Limit > 0 && opts.Limit < len(windows) {
		windows = windows[:opts.Limit]
	}
	limit := len(windows)
	if workers > limit {
		workers = limit
	}
	if workers < 1 {
		workers = 1
	}

	storedByWindow := make(map[int]WindowResult)
	if opts.Store != nil {
		stored, err := opts.Store.LoadAll(p.ParameterVersion, AlgorithmVersion)
		if err != nil {
			return RunResult{}, err
		}
		for _, w := range stored {
			storedByWindow[w.WindowNumber] = w
		}
	}

	type slot struct {
		index   int
		window  SampleWindow
		stored  *WindowResult
		compute bool
	}
	slots := make([]slot, limit)
	var invalidations []Invalidation
	for i, w := range windows {
		slots[i] = slot{index: i, window: w}
		if stored, ok := storedByWindow[w.Number]; ok {
			expected, err := expectedWindowDependencies(p, w)
			if err != nil {
				return RunResult{}, err
			}
			if checkpointCompatible(stored, w, cfg, p.ParameterVersion, expected) {
				copy := stored
				slots[i].stored = &copy
			} else {
				slots[i].compute = true
				_, byFixture, err := expectedFixtureCacheKeys(p, w)
				if err != nil {
					return RunResult{}, err
				}
				invalidations = append(invalidations, invalidationFromStored(stored, expected, byFixture, w.Number))
			}
		} else {
			slots[i].compute = true
		}
	}

	computed := make([]windowComputation, limit)
	jobs := make(chan int)
	results := make(chan struct {
		index int
		comp  windowComputation
	}, limit)
	for w := 0; w < workers; w++ {
		go func() {
			for idx := range jobs {
				comp := computeWindow(ctx, p, slots[idx].window, cfg)
				results <- struct {
					index int
					comp  windowComputation
				}{idx, comp}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := range slots {
			if slots[i].compute {
				select {
				case jobs <- i:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	remaining := 0
	for i := range slots {
		if slots[i].compute {
			remaining++
		}
	}
	for i := 0; i < remaining; i++ {
		select {
		case item := <-results:
			computed[item.index] = item.comp
		case <-ctx.Done():
			for j := range computed {
				if computed[j].err == nil && computed[j].window != nil {
					_ = computed[j].window.Transition(WindowFailed)
					for _, fm := range computed[j].fixtures {
						_ = fm.Transition(FixtureFailed)
					}
				}
			}
			return RunResult{}, ctx.Err()
		}
	}
	for i := range computed {
		if slots[i].compute && computed[i].err != nil {
			return RunResult{}, computed[i].err
		}
	}

	run := RunResult{Project: p.ProjectID, Invalidated: invalidations}
	seq := 0
	for i := range slots {
		seq++
		if slots[i].stored != nil {
			run.Windows = append(run.Windows, *slots[i].stored)
			run.Reused = append(run.Reused, slots[i].window.Number)
			run.CommitSequence = append(run.CommitSequence, slots[i].window.Number)
			continue
		}
		comp := computed[i]
		if opts.Store != nil {
			if err := opts.Store.Commit(comp.result, seq); err != nil {
				_ = comp.window.Transition(WindowFailed)
				for _, fm := range comp.fixtures {
					_ = fm.Transition(FixtureFailed)
				}
				return RunResult{}, err
			}
		}
		for _, fixtureID := range sortedFixtureIDs(p) {
			if fm := comp.fixtures[fixtureID]; fm != nil {
				if err := fm.Transition(FixtureCommitted); err != nil {
					return RunResult{}, err
				}
			}
		}
		if err := comp.window.Transition(WindowCommitted); err != nil {
			return RunResult{}, err
		}
		run.Windows = append(run.Windows, comp.result)
		run.Committed = append(run.Committed, slots[i].window.Number)
		run.CommitSequence = append(run.CommitSequence, slots[i].window.Number)
	}
	if opts.Previous != nil {
		report, err := CompareProjects(opts.Previous, p)
		if err != nil {
			return RunResult{}, err
		}
		selected := make(map[int]struct{}, len(windows))
		for _, w := range windows {
			selected[w.Number] = struct{}{}
		}
		run.Invalidated = run.Invalidated[:0]
		for _, inv := range report.Invalidated {
			if _, ok := selected[inv.WindowNumber]; ok {
				run.Invalidated = append(run.Invalidated, inv)
			}
		}
	}
	return run, nil
}

func computeWindow(ctx context.Context, p *Project, window SampleWindow, cfg SamplingConfig) windowComputation {
	windowSM := NewWindowStateMachine()
	fixtures := sortedFixtures(p)
	fixtureSMs := make(map[string]*FixtureStateMachine, len(fixtures))
	for _, f := range fixtures {
		fixtureSMs[f.ID] = NewFixtureStateMachine(f.ID)
	}
	fail := func(err error) windowComputation {
		_ = windowSM.Transition(WindowFailed)
		for _, fm := range fixtureSMs {
			_ = fm.Transition(FixtureFailed)
		}
		return windowComputation{window: windowSM, fixtures: fixtureSMs, err: err}
	}
	if err := windowSM.Transition(WindowPoseEvaluating); err != nil {
		return fail(err)
	}
	samples := append([]int64(nil), window.Samples...)
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	records := make([]poseRecord, 0, len(fixtures)*len(samples))
	for _, t := range samples {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		default:
		}
		for _, f := range fixtures {
			pose, err := EvaluateFixture(p, f, t)
			if err != nil {
				return fail(err)
			}
			cacheKey := FixtureCacheKey(pose.FixtureHash, t, p.ParameterVersion, pose.UpstreamKeys...)
			records = append(records, poseRecord{pose: pose, cacheKey: cacheKey})
		}
	}
	for _, f := range fixtures {
		if err := fixtureSMs[f.ID].Transition(FixtureEvaluated); err != nil {
			return fail(err)
		}
	}
	if err := windowSM.Transition(WindowConeSampling); err != nil {
		return fail(err)
	}

	fixtureSamples := make([]FixtureSample, 0, len(records))
	for _, rec := range records {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		default:
		}
		points, err := SampleFixture(rec.pose, rec.pose.LevelPermyriad, rec.pose.RGBPermyriad, cfg)
		if err != nil {
			return fail(err)
		}
		sample := FixtureSample{
			FixtureID: rec.pose.FixtureID, Pose: rec.pose, CacheKey: rec.cacheKey,
			Points: points,
		}
		for _, pt := range points {
			sample.TotalIntensity += pt.Intensity
			for ch := 0; ch < 3; ch++ {
				sample.TotalColor[ch] += pt.ColorContribution[ch]
			}
			if pt.Uncertain {
				sample.UncertainCount++
			}
		}
		fixtureSamples = append(fixtureSamples, sample)
	}
	for _, f := range fixtures {
		if err := fixtureSMs[f.ID].Transition(FixtureSampled); err != nil {
			return fail(err)
		}
	}
	aggregates := aggregatePoints(fixtureSamples, cfg)
	cacheKeys := make([]string, 0, len(records))
	nodes := make([]string, 0, len(records))
	fixtureIDs := make([]string, 0, len(records))
	uncertain := 0
	for _, rec := range records {
		cacheKeys = append(cacheKeys, rec.cacheKey)
		nodes = append(nodes, dependencyNodeID(rec.pose.FixtureID, window.Number, rec.pose.TimeMS))
		fixtureIDs = append(fixtureIDs, rec.pose.FixtureID)
	}
	for _, sample := range fixtureSamples {
		uncertain += sample.UncertainCount
	}
	deps := WindowDependency{
		FixtureIDs: sortedUnique(fixtureIDs),
		CacheKeys:  sortedUnique(cacheKeys),
		NodeIDs:    sortedUnique(nodes),
		WindowKey:  WindowCacheKey(cacheKeys, samples, p.ParameterVersion),
	}
	result := WindowResult{
		WindowNumber: window.Number, SampleTimes: samples,
		ParameterVersion: p.ParameterVersion, AlgorithmVersion: AlgorithmVersion,
		Sampling: cfg, Fixtures: fixtureSamples, Aggregates: aggregates,
		UncertainCount: uncertain, Dependencies: deps,
	}
	result, err := FinalizeWindow(result)
	if err != nil {
		return fail(err)
	}
	return windowComputation{result: result, window: windowSM, fixtures: fixtureSMs}
}

func expectedWindowDependencies(p *Project, window SampleWindow) (WindowDependency, error) {
	keys, _, err := expectedFixtureCacheKeys(p, window)
	if err != nil {
		return WindowDependency{}, err
	}
	samples := append([]int64(nil), window.Samples...)
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	fixtures := sortedFixtures(p)
	nodes := make([]string, 0, len(samples)*len(fixtures))
	fixtureIDs := make([]string, 0, len(samples)*len(fixtures))
	for _, t := range samples {
		for _, f := range fixtures {
			nodes = append(nodes, dependencyNodeID(f.ID, window.Number, t))
			fixtureIDs = append(fixtureIDs, f.ID)
		}
	}
	return WindowDependency{
		FixtureIDs: sortedUnique(fixtureIDs),
		CacheKeys:  sortedUnique(keys),
		NodeIDs:    sortedUnique(nodes),
		WindowKey:  WindowCacheKey(keys, samples, p.ParameterVersion),
	}, nil
}

func expectedFixtureCacheKeys(p *Project, window SampleWindow) ([]string, map[string][]string, error) {
	samples := append([]int64(nil), window.Samples...)
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	keys := make([]string, 0, len(samples)*len(p.Fixtures))
	byFixture := make(map[string][]string)
	for _, t := range samples {
		for _, f := range sortedFixtures(p) {
			pose, err := EvaluateFixture(p, f, t)
			if err != nil {
				return nil, nil, err
			}
			key := FixtureCacheKey(pose.FixtureHash, t, p.ParameterVersion, pose.UpstreamKeys...)
			keys = append(keys, key)
			byFixture[f.ID] = append(byFixture[f.ID], key)
		}
	}
	return keys, byFixture, nil
}

func checkpointCompatible(stored WindowResult, window SampleWindow, cfg SamplingConfig, parameterVersion string, expected WindowDependency) bool {
	if stored.WindowNumber != window.Number ||
		stored.ParameterVersion != parameterVersion ||
		stored.AlgorithmVersion != AlgorithmVersion {
		return false
	}
	if !reflect.DeepEqual(stored.Sampling, cfg) {
		return false
	}
	expectedTimes := append([]int64(nil), window.Samples...)
	sort.Slice(expectedTimes, func(i, j int) bool { return expectedTimes[i] < expectedTimes[j] })
	if !reflect.DeepEqual(stored.SampleTimes, expectedTimes) {
		return false
	}
	return reflect.DeepEqual(stored.Dependencies, expected)
}

func invalidationFromStored(stored WindowResult, expected WindowDependency, newByFixture map[string][]string, windowNumber int) Invalidation {
	changed := make([]string, 0)
	oldByFixture := make(map[string][]string)
	for _, sample := range stored.Fixtures {
		oldByFixture[sample.FixtureID] = append(oldByFixture[sample.FixtureID], sample.CacheKey)
	}
	for _, fixtureID := range expected.FixtureIDs {
		if !reflect.DeepEqual(sortedUnique(oldByFixture[fixtureID]), sortedUnique(newByFixture[fixtureID])) {
			changed = append(changed, fixtureID)
		}
	}
	return Invalidation{
		WindowNumber: windowNumber,
		FixtureIDs:   sortedUnique(changed),
		Reasons:      []string{"checkpoint dependency cache key or sampling configuration changed"},
		Downstream: []string{
			fmt.Sprintf("window:%d:summary", windowNumber),
			fmt.Sprintf("window:%d:aggregates", windowNumber),
		},
	}
}

func sortedFixtureIDs(p *Project) []string {
	out := make([]string, 0, len(p.Fixtures))
	for _, f := range p.Fixtures {
		out = append(out, f.ID)
	}
	sort.Strings(out)
	return out
}
