package engine_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"proxydiag/bizerr"
	"proxydiag/domain"
	"proxydiag/engine"
	"proxydiag/samplegen"
	"proxydiag/schema"
)

func loadProject(t *testing.T) *domain.Project {
	t.Helper()
	dir := t.TempDir()
	if _, err := samplegen.Write(dir, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	p, err := schema.LoadFile(filepath.Join(dir, "project_v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func withoutCache(results []engine.Result) []engine.Result {
	out := make([]engine.Result, len(results))
	copy(out, results)
	for i := range out {
		out[i].Cache = engine.CacheInfo{}
	}
	return out
}

func TestIncrementalMatchesFullRecompute(t *testing.T) {
	p := loadProject(t)
	originalFingerprint := p.CoreFingerprint()
	eng := engine.New(p, nil)
	eng.RunAll()

	edits := []engine.Edit{
		{Kind: engine.EditHealth, UpstreamID: "u-blue", Healthy: false, AtSeq: 1},
		{Kind: engine.EditWeight, UpstreamID: "u-green", WeightPPM: 160000, AtSeq: 1},
		{Kind: engine.EditRewrite, RouteID: "R-api-v1", StripPrefix: "/api/v1/", AddPrefix: "/backend/v2/", AtSeq: 1},
		{Kind: engine.EditDelete, Seq: 20, AtSeq: 20},
		{
			Kind: engine.EditAdd, AtSeq: 41,
			Request: &domain.Request{
				Seq: 41, ArrivalMS: 10000, Method: "GET", RawPath: "/api/v1/new/41",
				Headers: []domain.Header{{Name: "X-Request-ID", Value: "req-041"}},
			},
		},
	}
	for i, edit := range edits {
		report, err := eng.Apply(edit)
		if err != nil {
			t.Fatalf("edit %d (%s): %v", i, edit.Kind, err)
		}
		if len(report.Recomputed) == 0 && edit.Kind != engine.EditHealth {
			t.Fatalf("edit %d recomputed nothing", i)
		}
		got := withoutCache(eng.Results())
		full := withoutCache(engine.EvaluateAll(eng.Project(), nil))
		if !reflect.DeepEqual(got, full) {
			t.Fatalf("edit %d (%s) incremental != full", i, edit.Kind)
		}
		if i < 4 && len(report.Reused) == 0 {
			t.Fatalf("edit %d did not reuse any cached request: %+v", i, report)
		}
		if p.CoreFingerprint() != originalFingerprint {
			t.Fatalf("edit %d mutated the caller's source project", i)
		}
	}
}

func TestInvalidEditLeavesCommittedCacheUsable(t *testing.T) {
	p := loadProject(t)
	eng := engine.New(p, nil)
	eng.RunAll()
	beforeFP := eng.Project().CoreFingerprint()
	before := withoutCache(eng.Results())
	_, err := eng.Apply(engine.Edit{Kind: engine.EditWeight, UpstreamID: "u-blue", WeightPPM: 0, AtSeq: 1})
	if got := bizerr.Code(err); got != bizerr.WeightRange {
		t.Fatalf("expected %s, got %s (%v)", bizerr.WeightRange, got, err)
	}
	if eng.Project().CoreFingerprint() != beforeFP {
		t.Fatal("failed edit changed the active snapshot")
	}
	if !reflect.DeepEqual(before, withoutCache(eng.Results())) {
		t.Fatal("failed edit disturbed committed cache")
	}
}

func TestSnapshotVersionIsAuthoritative(t *testing.T) {
	p := loadProject(t)
	eng := engine.New(p, nil)
	eng.RunAll()
	count := len(eng.Results())

	sameVersion := eng.Project()
	sameVersion.Upstreams[0].WeightPPM = 123000
	_, err := eng.ReplaceSnapshot(sameVersion, nil)
	if got := bizerr.Code(err); got != bizerr.SnapshotVersion {
		t.Fatalf("expected %s, got %s (%v)", bizerr.SnapshotVersion, got, err)
	}
	if len(eng.Results()) != count {
		t.Fatal("rejected snapshot replacement discarded usable cache")
	}

	next := eng.Project()
	next.Version = "edge-lab-2026@2"
	next.Upstreams[0].WeightPPM = 123000
	report, err := eng.ReplaceSnapshot(next, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !report.FullReset || len(report.Recomputed) != count {
		t.Fatalf("version change must fully reset: %+v", report)
	}
}

func TestConcurrencyDoesNotChangeDecisions(t *testing.T) {
	p := loadProject(t)
	a := engine.New(p, nil).RunConcurrent(engine.ConcurrencyConfig{Workers: 2})
	b := engine.New(p, nil).RunConcurrent(engine.ConcurrencyConfig{Workers: 8})
	if len(a.Backpressure) != 0 || len(b.Backpressure) != 0 {
		t.Fatalf("unexpected backpressure: %v / %v", a.Backpressure, b.Backpressure)
	}
	if !reflect.DeepEqual(withoutCache(a.Results), withoutCache(b.Results)) {
		t.Fatal("worker budget changed forwarding decisions")
	}
	if len(a.Results) != len(p.Requests) {
		t.Fatalf("expected %d results, got %d", len(p.Requests), len(a.Results))
	}
}

func TestBackpressureIsObservableAndDeterministic(t *testing.T) {
	p := loadProject(t)
	a := engine.New(p, nil).RunConcurrent(engine.ConcurrencyConfig{Workers: 1})
	b := engine.New(p, nil).RunConcurrent(engine.ConcurrencyConfig{Workers: 1})
	if len(a.Backpressure) == 0 {
		t.Fatal("expected worker backpressure for a multi-request arrival batch")
	}
	if len(a.Results) != len(p.Requests) {
		t.Fatalf("backpressure silently dropped requests: got %d want %d", len(a.Results), len(p.Requests))
	}
	if !reflect.DeepEqual(a.Backpressure, b.Backpressure) {
		t.Fatal("backpressure admission is not deterministic")
	}
	checkUniqueSeqs(t, a.Results, len(p.Requests))

	bufferCfg := engine.ConcurrencyConfig{
		Workers:                16,
		ListenerBufferOverride: map[string]int{"edge-api": 0},
	}
	c := engine.New(p, nil).RunConcurrent(bufferCfg)
	foundBuffer := false
	for _, bp := range c.Backpressure {
		if bp.Code == bizerr.BackpressureBuffer {
			foundBuffer = true
		}
	}
	if !foundBuffer {
		t.Fatalf("expected %s in %+v", bizerr.BackpressureBuffer, c.Backpressure)
	}
	checkUniqueSeqs(t, c.Results, len(p.Requests))
}

func checkUniqueSeqs(t *testing.T, results []engine.Result, want int) {
	t.Helper()
	seen := map[int]bool{}
	for _, r := range results {
		if seen[r.Seq] {
			t.Fatalf("duplicate result for seq %d", r.Seq)
		}
		seen[r.Seq] = true
	}
	if len(seen) != want {
		t.Fatalf("missing results: got %d want %d", len(seen), want)
	}
}

func TestRewriteEmptyIsRejectedWithoutMutation(t *testing.T) {
	p := loadProject(t)
	p.Requests = append(p.Requests, domain.Request{
		Seq: 41, ArrivalMS: 10000, Method: "GET", RawPath: "/api/v1/",
		Headers: []domain.Header{{Name: "X-Request-ID", Value: "empty"}},
	})
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	route, _, ok := p.RouteByID("R-api-v1")
	if !ok {
		t.Fatal("missing route")
	}
	route.Rewrite.AddPrefix = ""
	before := p.CoreFingerprint()
	results := engine.EvaluateAll(p, nil)
	var last engine.Result
	for _, r := range results {
		if r.Seq == 41 {
			last = r
		}
	}
	if last.ErrorCode != bizerr.RewriteEmpty {
		t.Fatalf("expected %s, got %s (%+v)", bizerr.RewriteEmpty, last.ErrorCode, last)
	}
	if p.CoreFingerprint() != before {
		t.Fatal("evaluation mutated source project")
	}
}
