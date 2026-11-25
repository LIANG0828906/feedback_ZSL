package acceptance_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"sre-timeline/internal/bizerr"
	"sre-timeline/internal/checkpoint"
	"sre-timeline/internal/concurrency"
	"sre-timeline/internal/engine"
	"sre-timeline/internal/fixedpoint"
	"sre-timeline/internal/intervals"
	"sre-timeline/internal/model"
	"sre-timeline/internal/sample"
	"sre-timeline/internal/schemaio"
	"sre-timeline/internal/scoring"
	"sre-timeline/internal/timeline"
	"sre-timeline/internal/topology"
)

func fixture(t *testing.T) *model.Project {
	t.Helper()
	p := sample.Generate(sample.DefaultSeed)
	if _, err := sample.BuildTruth(p); err != nil {
		t.Fatalf("build truth: %v", err)
	}
	return p
}

func loadedVariants(t *testing.T, p *model.Project) map[string]*model.Project {
	t.Helper()
	out := map[string]*model.Project{}
	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		data, err := schemaio.Encode(p, version)
		if err != nil {
			t.Fatalf("encode %s: %v", version, err)
		}
		loaded, err := schemaio.Load(data)
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		out[version] = loaded
	}
	return out
}

func TestSchemaVariantsNormalizeToSameProject(t *testing.T) {
	p := fixture(t)
	variants := loadedVariants(t, p)
	wantServices := p.ServiceIDs()
	wantEdges := p.EdgesSorted()
	wantHash := engine.ContentHash(p)

	for version, loaded := range variants {
		if gotHash := engine.ContentHash(loaded); gotHash != wantHash {
			t.Fatalf("%s normalized hash = %s, want %s", version, gotHash, wantHash)
		}
		if got := loaded.ServiceIDs(); !reflect.DeepEqual(got, wantServices) {
			t.Fatalf("%s services = %v, want %v", version, got, wantServices)
		}
		if got := loaded.EdgesSorted(); !reflect.DeepEqual(got, wantEdges) {
			t.Fatalf("%s edges = %+v, want %+v", version, got, wantEdges)
		}
		wantEvents := p.SortedEvents()
		gotEvents := loaded.SortedEvents()
		if len(gotEvents) != len(wantEvents) {
			t.Fatalf("%s event count = %d, want %d", version, len(gotEvents), len(wantEvents))
		}
		for i := range wantEvents {
			w, g := wantEvents[i], gotEvents[i]
			if w.ID != g.ID || w.Seq != g.Seq || w.ServiceID != g.ServiceID || w.Source != g.Source || w.OccurredMS != g.OccurredMS || w.IngestLagMS != g.IngestLagMS || w.AliveMS != g.AliveMS {
				t.Fatalf("%s event %d mismatch:\nwant %+v\ngot  %+v", version, i, w, g)
			}
		}
	}
}

func TestAlignmentIntervalsAndScoresMatchTruth(t *testing.T) {
	p := fixture(t)
	for version, loaded := range loadedVariants(t, p) {
		aligned, err := timeline.Align(loaded)
		if err != nil {
			t.Fatalf("%s align: %v", version, err)
		}
		truthByID := map[string]model.TruthEvent{}
		for _, te := range p.Truth.AlignedEvents {
			truthByID[te.ID] = te
		}
		if len(aligned.All) != len(p.Truth.AlignedEvents) {
			t.Fatalf("%s aligned all count %d want %d", version, len(aligned.All), len(p.Truth.AlignedEvents))
		}
		for _, got := range aligned.All {
			want := truthByID[got.EventID]
			if got.EffectiveMS != want.EffectiveMS || got.ServiceID != want.ServiceID {
				t.Fatalf("%s alignment %s = (%d,%s), want (%d,%s)", version, got.EventID, got.EffectiveMS, got.ServiceID, want.EffectiveMS, want.ServiceID)
			}
			if got.Kept && got.DuplicatesRemoved != want.DedupedCount {
				t.Fatalf("%s dedup count %s = %d want %d", version, got.EventID, got.DuplicatesRemoved, want.DedupedCount)
			}
		}

		results, err := intervals.Derive(loaded, aligned)
		if err != nil {
			t.Fatalf("%s derive: %v", version, err)
		}
		targets := scoring.NumberTargets(results)
		if len(targets) != len(p.Truth.Intervals) {
			t.Fatalf("%s interval count = %d want %d", version, len(targets), len(p.Truth.Intervals))
		}
		for i, target := range targets {
			want := p.Truth.Intervals[i]
			got := target.Data
			if target.Ordinal != want.Ordinal || got.ServiceID != want.ServiceID || got.StartMS != want.StartMS || got.EndMS != want.EndMS || got.Metric != want.Metric || got.ThresholdBP != want.ThresholdBP {
				t.Fatalf("%s interval %d mismatch got=%+v want=%+v", version, i, got, want)
			}
			if !reflect.DeepEqual(got.EvidenceIDs, want.EvidenceIDs) {
				t.Fatalf("%s interval %s evidence = %v want %v", version, target.ID, got.EvidenceIDs, want.EvidenceIDs)
			}
		}

		topo, err := topology.New(loaded.Services, loaded.Edges)
		if err != nil {
			t.Fatalf("%s topology: %v", version, err)
		}
		kept := []model.Event{}
		byID := map[string]model.Event{}
		for _, ev := range loaded.Events {
			byID[ev.ID] = ev
		}
		for _, item := range aligned.Kept {
			kept = append(kept, byID[item.EventID])
		}
		report, err := scoring.Score(loaded, topo, targets, kept)
		if err != nil {
			t.Fatalf("%s score: %v", version, err)
		}
		for i, candidate := range report.Candidates {
			want := p.Truth.Candidates[i]
			if candidate.EventID != want.EventID || candidate.Rank != want.Rank {
				t.Fatalf("%s rank %d got %s/#%d want %s/#%d", version, i, candidate.EventID, candidate.Rank, want.EventID, want.Rank)
			}
			if !within(candidate.TotalRaw, want.ScoreRaw, p.Policy.ScoreTolerance) {
				t.Fatalf("%s score %s = %d want %d tol %d", version, candidate.EventID, candidate.TotalRaw, want.ScoreRaw, p.Policy.ScoreTolerance)
			}
		}
		if report.Candidates[0].EventID != "chg-pay-deploy" {
			t.Fatalf("top candidate = %s, want chg-pay-deploy", report.Candidates[0].EventID)
		}
	}
}

func TestPropagationFlatAndNestedTreeMatch(t *testing.T) {
	p := fixture(t)
	flat, err := topology.New(p.Services, p.Edges)
	if err != nil {
		t.Fatal(err)
	}
	flatResult, err := flat.Propagate("payment", p.Policy.PropagationGen, topology.DefaultDecayBP)
	if err != nil {
		t.Fatal(err)
	}
	wantClosure := []string{"gateway", "order", "payment"}
	if !reflect.DeepEqual(flatResult.Closure, wantClosure) {
		t.Fatalf("closure = %v want %v", flatResult.Closure, wantClosure)
	}

	v2, err := schemaio.Encode(p, model.SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := schemaio.Load(v2)
	if err != nil {
		t.Fatal(err)
	}
	nested, err := topology.New(loaded.Services, loaded.Edges)
	if err != nil {
		t.Fatal(err)
	}
	nestedResult, err := nested.Propagate("payment", p.Policy.PropagationGen, topology.DefaultDecayBP)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(flatResult, nestedResult) {
		t.Fatalf("flat/nested propagation mismatch:\nflat=%+v\nnested=%+v", flatResult, nestedResult)
	}

	cycleEdges := append(append([]model.Edge{}, p.Edges...), model.Edge{Caller: "inventory", Callee: "order"})
	cycTopo, err := topology.New(p.Services, cycleEdges)
	if err != nil {
		t.Fatal(err)
	}
	if err := cycTopo.ValidateAcyclic(); !bizerr.IsCode(err, bizerr.CodeTopologyCycle) {
		t.Fatalf("cycle error = %v, want %s", err, bizerr.CodeTopologyCycle)
	}
	bounded, err := cycTopo.Propagate("payment", 2, topology.DefaultDecayBP)
	if err != nil {
		t.Fatalf("bounded propagation should tolerate cycle with generation cap: %v", err)
	}
	if !bounded.CycleDetected {
		t.Fatal("bounded result should report cycle_detected")
	}
}

func TestIncrementalEditsMatchFullRecompute(t *testing.T) {
	ctx := context.Background()
	p := fixture(t)
	cfg := engine.Options{Concurrency: concurrency.Config{Workers: 3, BufferLimit: 4096}}
	eng, err := engine.New(p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	full, err := eng.Analyze(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Targets) != 5 {
		t.Fatalf("target count = %d, want 5", len(full.Targets))
	}

	if _, err := eng.EditThreshold("payment", 900); err != nil {
		t.Fatal(err)
	}
	incremental, err := eng.Analyze(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(incremental.Diagnostics.RecomputedServices, "payment") {
		t.Fatalf("payment should be recomputed: %+v", incremental.Diagnostics)
	}
	if contains(incremental.Diagnostics.RecomputedServices, "inventory") {
		t.Fatalf("inventory interval should be reused: %+v", incremental.Diagnostics)
	}
	if len(incremental.Diagnostics.ReusedScoreKeys) != 2 {
		t.Fatalf("inventory score keys reused = %v, want 2", incremental.Diagnostics.ReusedScoreKeys)
	}
	assertEquivalentToFresh(t, eng.Project(), cfg, incremental)

	if _, err := eng.CorrectEventTime("alert-gw-01", p.Window.StartMS+160_000); err != nil {
		t.Fatal(err)
	}
	incremental, err = eng.Analyze(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertEquivalentToFresh(t, eng.Project(), cfg, incremental)

	newEvent := model.Event{
		ID: "chg-payment-config-extra", Source: model.SourceChange, ServiceID: "payment",
		Kind: "config_change", Fingerprint: "config-extra", OccurredMS: p.Window.StartMS + 75_000,
		ChangeType: "config_change", TypeWeightBP: 8500, AliveMS: 60_000,
	}
	if _, err := eng.AddEvent(newEvent); err != nil {
		t.Fatal(err)
	}
	incremental, err = eng.Analyze(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertEquivalentToFresh(t, eng.Project(), cfg, incremental)

	if _, err := eng.AddEdge("payment", "inventory"); err != nil {
		t.Fatal(err)
	}
	incremental, err = eng.Analyze(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(incremental.Diagnostics.RecomputedServices) != 0 {
		t.Fatalf("edge edit must reuse all interval derivations, recomputed=%v", incremental.Diagnostics.RecomputedServices)
	}
	assertEquivalentToFresh(t, eng.Project(), cfg, incremental)
}

func assertEquivalentToFresh(t *testing.T, edited *model.Project, cfg engine.Options, incremental *engine.Analysis) {
	t.Helper()
	freshEngine, err := engine.New(edited, cfg)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := freshEngine.Analyze(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidateSummaries(incremental), candidateSummaries(fresh)) {
		t.Fatalf("incremental candidates differ from full recompute:\ninc=%+v\nfull=%+v", candidateSummaries(incremental), candidateSummaries(fresh))
	}
	if !reflect.DeepEqual(intervalSummaries(incremental), intervalSummaries(fresh)) {
		t.Fatalf("incremental intervals differ from full recompute:\ninc=%+v\nfull=%+v", intervalSummaries(incremental), intervalSummaries(fresh))
	}
}

type candidateSummary struct {
	Rank  int
	Event string
	Raw   int64
}

func candidateSummaries(a *engine.Analysis) []candidateSummary {
	out := make([]candidateSummary, 0, len(a.Scoring.Candidates))
	for _, c := range a.Scoring.Candidates {
		out = append(out, candidateSummary{c.Rank, c.EventID, c.TotalRaw})
	}
	return out
}

type intervalSummary struct {
	Service string
	Start   int64
	End     int64
}

func intervalSummaries(a *engine.Analysis) []intervalSummary {
	out := make([]intervalSummary, 0, len(a.Targets))
	for _, target := range a.Targets {
		out = append(out, intervalSummary{target.Data.ServiceID, target.Data.StartMS, target.Data.EndMS})
	}
	return out
}

func TestCheckpointVersionAndTruncation(t *testing.T) {
	ctx := context.Background()
	p := fixture(t)
	eng, err := engine.New(p, engine.Options{Concurrency: concurrency.Config{Workers: 2, BufferLimit: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := eng.Analyze(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := checkpoint.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	round := checkpoint.Round{RoundNo: 1, ProjectID: p.ID, ProjectVersion: eng.Hash(), SchemaVariant: model.SchemaV1, CommittedAtMS: time.Now().UnixMilli()}
	for serviceID, result := range analysis.Intervals {
		payload, _ := json.Marshal(result)
		round.ServiceStates = append(round.ServiceStates, checkpoint.ServiceState{ServiceID: serviceID, StateJSON: string(payload)})
	}
	contribsByKey := map[string][]scoring.Contribution{}
	for _, contribution := range analysis.Scoring.Contributions {
		contribsByKey[contribution.IntervalKey] = append(contribsByKey[contribution.IntervalKey], contribution)
	}
	for _, target := range analysis.Targets {
		payload, _ := json.Marshal(contribsByKey[target.Data.Key])
		round.ContributionCaches = append(round.ContributionCaches, checkpoint.ContributionCache{IntervalKey: target.Data.Key, IntervalID: target.ID, PayloadJSON: string(payload)})
	}
	if err := store.SaveRound(ctx, round); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadLatest(ctx, eng.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RoundNo != 1 || len(loaded.ServiceStates) != len(p.Services) || len(loaded.ContributionCaches) != len(analysis.Targets) {
		t.Fatalf("loaded checkpoint = %+v", loaded)
	}
	var persistedContribs []scoring.Contribution
	if err := json.Unmarshal([]byte(loaded.ContributionCaches[0].PayloadJSON), &persistedContribs); err != nil {
		t.Fatalf("persisted contribution payload: %v", err)
	}
	if len(persistedContribs) == 0 {
		t.Fatal("contribution cache payload is empty")
	}
	if _, err := store.LoadLatest(ctx, "wrong-version"); !bizerr.IsCode(err, bizerr.CodeCheckpointVersion) {
		t.Fatalf("version mismatch err=%v want %s", err, bizerr.CodeCheckpointVersion)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(dbPath, 32); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.Open(ctx, dbPath); !bizerr.IsCode(err, bizerr.CodeCheckpointTrunc) {
		t.Fatalf("truncated checkpoint err=%v want %s", err, bizerr.CodeCheckpointTrunc)
	}
}

func TestBackpressureAndDeterministicConcurrency(t *testing.T) {
	ctx := context.Background()
	p := fixture(t)
	tiny, err := engine.New(p, engine.Options{Concurrency: concurrency.Config{Workers: 2, BufferLimit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tiny.Analyze(ctx); !bizerr.IsCode(err, bizerr.CodeBackpressureBuffer) {
		t.Fatalf("tiny buffer err=%v want %s", err, bizerr.CodeBackpressureBuffer)
	}

	block := make(chan struct{})
	started := make(chan struct{})
	pool, err := concurrency.NewPool(concurrency.Config{Workers: 1, BufferLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Submit(ctx, func() {
		close(started)
		<-block
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := pool.TrySubmit(func() {}); err != nil {
		t.Fatalf("first queue slot should accept: %v", err)
	}
	err = pool.TrySubmit(func() {})
	if !bizerr.IsCode(err, bizerr.CodeBackpressureWorker) {
		t.Fatalf("saturated pool err=%v want %s", err, bizerr.CodeBackpressureWorker)
	}
	close(block)
	pool.Close()

	one, err := engine.New(p, engine.Options{Concurrency: concurrency.Config{Workers: 1, BufferLimit: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	four, err := engine.New(p, engine.Options{Concurrency: concurrency.Config{Workers: 4, BufferLimit: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := one.Analyze(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := four.Analyze(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidateSummaries(a), candidateSummaries(b)) {
		t.Fatalf("worker counts changed candidates: %+v vs %+v", candidateSummaries(a), candidateSummaries(b))
	}
}

func TestStableInputErrorCodes(t *testing.T) {
	p := fixture(t)
	v1, err := schemaio.Encode(p, model.SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(v1, &doc); err != nil {
		t.Fatal(err)
	}
	doc["unexpected_field"] = "boom"
	bad, _ := json.Marshal(doc)
	if _, err := schemaio.Load(bad); !bizerr.IsCode(err, bizerr.CodeSchemaUnknownField) {
		t.Fatalf("unknown field err=%v", err)
	}

	reversed, err := engine.CloneProject(p)
	if err != nil {
		t.Fatal(err)
	}
	reversed.Window.StartMS = reversed.Window.EndMS + 1
	if err := schemaio.ValidateCanonical(reversed); !bizerr.IsCode(err, bizerr.CodeTimeOrder) {
		t.Fatalf("reversed window err=%v", err)
	}

	monotonic, err := engine.CloneProject(p)
	if err != nil {
		t.Fatal(err)
	}
	events := monotonic.SortedEvents()
	events[20].OccurredMS = events[19].OccurredMS - 1
	monotonic.Events = events
	if err := schemaio.CheckMonotonicLogicalStream(monotonic); !bizerr.IsCode(err, bizerr.CodeTimeOrder) {
		t.Fatalf("monotonic err=%v", err)
	}

	negative, err := engine.CloneProject(p)
	if err != nil {
		t.Fatal(err)
	}
	negative.Services[0].ErrorThresholdBP = -3
	if err := schemaio.ValidateCanonical(negative); !bizerr.IsCode(err, bizerr.CodeThresholdNegative) {
		t.Fatalf("negative threshold err=%v", err)
	}

	cycle, err := engine.CloneProject(p)
	if err != nil {
		t.Fatal(err)
	}
	cycle.Edges = append(cycle.Edges, model.Edge{Caller: "inventory", Callee: "order"})
	cycTopo, err := topology.New(cycle.Services, cycle.Edges)
	if err != nil {
		t.Fatal(err)
	}
	if err := cycTopo.ValidateAcyclic(); !bizerr.IsCode(err, bizerr.CodeTopologyCycle) {
		t.Fatalf("cycle err=%v", err)
	}

	v2, err := schemaio.Encode(p, model.SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	var v2doc map[string]any
	_ = json.Unmarshal(v2, &v2doc)
	policy := v2doc["policy"].(map[string]any)
	policy["scoreScale"] = float64(7)
	badScale, _ := json.Marshal(v2doc)
	if _, err := schemaio.Load(badScale); !bizerr.IsCode(err, bizerr.CodeScaleOutOfRange) {
		t.Fatalf("scale err=%v", err)
	}
}

func TestFixedPointOverflowAndSaturation(t *testing.T) {
	_, err := fixedpoint.Mul(fixedpoint.Value{Raw: math.MaxInt64}, fixedpoint.Value{Raw: math.MaxInt64})
	if !bizerr.IsCode(err, bizerr.CodeFixedOverflow) {
		t.Fatalf("mul overflow err=%v want %s", err, bizerr.CodeFixedOverflow)
	}
	sum, err := fixedpoint.Add(fixedpoint.Value{Raw: math.MaxInt64}, fixedpoint.Value{Raw: 1})
	if !bizerr.IsCode(err, bizerr.CodeFixedOverflow) {
		t.Fatalf("add overflow err=%v", err)
	}
	if !sum.Saturated || sum.Raw != math.MaxInt64 {
		t.Fatalf("saturated sum = %+v", sum)
	}
}

func TestSnapshotReplacementInvalidatesCache(t *testing.T) {
	ctx := context.Background()
	p := fixture(t)
	eng, err := engine.New(p, engine.Options{Concurrency: concurrency.Config{Workers: 2, BufferLimit: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Analyze(ctx); err != nil {
		t.Fatal(err)
	}
	same, err := eng.ReplaceSnapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	if same.CacheCleared {
		t.Fatal("identical snapshot should not require cache clear")
	}
	v2Data, err := schemaio.Encode(p, model.SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	v2Project, err := schemaio.Load(v2Data)
	if err != nil {
		t.Fatal(err)
	}
	schemaChange, err := eng.ReplaceSnapshot(v2Project)
	if err != nil {
		t.Fatal(err)
	}
	if !schemaChange.CacheCleared {
		t.Fatal("schema variant replacement should discard caches")
	}
	if _, err := eng.Analyze(ctx); err != nil {
		t.Fatal(err)
	}
	changed, err := engine.CloneProject(p)
	if err != nil {
		t.Fatal(err)
	}
	changed.Revision = "rev-changed"
	revisionChange, err := eng.ReplaceSnapshot(changed)
	if err != nil {
		t.Fatal(err)
	}
	if !revisionChange.CacheCleared {
		t.Fatal("revision change should discard caches")
	}
	if _, err := eng.Analyze(ctx); err != nil {
		t.Fatal(err)
	}
}

func within(got, want, tol int64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= tol
}

func contains(in []string, want string) bool {
	for _, v := range in {
		if v == want {
			return true
		}
	}
	return false
}
