package dnslab_test

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"

	"dnslab/checkpoint"
	"dnslab/codes"
	"dnslab/editor"
	"dnslab/engine"
	"dnslab/fixedint"
	"dnslab/generator"
	"dnslab/loader"
	"dnslab/spec"
)

func TestLoaderStableErrors(t *testing.T) {
	bundle, err := generator.Generate(7)
	if err != nil {
		t.Fatal(err)
	}
	raw := bundle.Docs[spec.SchemaV2]
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	t.Run("missing field", func(t *testing.T) {
		bad := cloneMap(doc)
		delete(bad, "projectId")
		_, err := loader.Load(mustJSON(t, bad))
		assertCode(t, err, codes.EFieldMissing)
	})

	t.Run("duplicate region", func(t *testing.T) {
		bad := cloneMap(doc)
		zones := bad["zones"].([]any)
		first := zones[0].(map[string]any)
		dup := map[string]any{}
		for k, v := range first {
			dup[k] = v
		}
		dup["name"] = "duplicate.example.test"
		bad["zones"] = append(zones, dup)
		_, err := loader.Load(mustJSON(t, bad))
		assertCode(t, err, codes.ERegionDuplicate)
	})

	t.Run("version conflict", func(t *testing.T) {
		bad := cloneMap(doc)
		bad["schema"] = spec.SchemaV1
		_, err := loader.Load(mustJSON(t, bad))
		assertCode(t, err, codes.EVersionConflict)
	})

	t.Run("invalid upstream reference", func(t *testing.T) {
		bad := cloneMap(doc)
		rules := bad["rules"].([]any)
		rule := rules[0].(map[string]any)
		rule["upstreamIndex"] = 99
		_, err := loader.Load(mustJSON(t, bad))
		assertCode(t, err, codes.EUpstreamReference)
	})
}

func TestFixedWidthSaturation(t *testing.T) {
	if r := fixedint.Mul64(math.MaxInt64, 1000); !r.Saturated || r.Value != math.MaxInt64 {
		t.Fatalf("multiplication saturation wrong: %+v", r)
	}
	if r := fixedint.Add64(math.MaxInt64, 1); !r.Saturated || r.Value != math.MaxInt64 {
		t.Fatalf("addition saturation wrong: %+v", r)
	}
	ms, saturated := fixedint.SecondsToMS(120)
	if saturated || ms != 120_000 {
		t.Fatalf("seconds conversion wrong: %d %v", ms, saturated)
	}
	if err := fixedint.ValidateTTLMS(-1); err == nil || codes.CodeOf(err) != codes.ETTLRange {
		t.Fatalf("ttl range code wrong: %v", err)
	}
	if err := fixedint.ValidateTimeoutMS(0); err == nil || codes.CodeOf(err) != codes.ETimeoutRange {
		t.Fatalf("timeout range code wrong: %v", err)
	}
}

func TestConcurrencyDeterminismAndBackpressure(t *testing.T) {
	bundle, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loader.Load(bundle.Docs[spec.SchemaV3])
	if err != nil {
		t.Fatal(err)
	}

	for _, workers := range []int{1, 2, 4, 8} {
		for iteration := 0; iteration < 5; iteration++ {
			eng := engine.NewEngine(p.Project)
			report, err := engine.RunSamples(context.Background(), eng, bundle.Queries, workers)
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range bundle.Truth {
				if !engine.CompareSemantic(report.Results[i], want) {
					t.Fatalf("workers=%d iter=%d query=%d mismatch", workers, iteration, i+1)
				}
			}
		}
	}

	budget, err := engine.NewBudget(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.TryAcquire(); err != nil {
		t.Fatal(err)
	}
	if err := budget.TryAcquire(); err != nil {
		t.Fatal(err)
	}
	err = budget.TryAcquire()
	assertCode(t, err, codes.EBackpressureWorkers)
}

func TestCanceledRunIsInterrupted(t *testing.T) {
	bundle, _ := generator.Generate(42)
	loaded, _ := loader.Load(bundle.Docs[spec.SchemaV3])
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := engine.RunSamples(ctx, engine.NewEngine(loaded.Project), bundle.Queries, 2)
	assertCode(t, err, codes.EInterrupted)
}

func TestIncrementalClosureAndEquality(t *testing.T) {
	bundle, _ := generator.Generate(42)
	loaded, err := loader.Load(bundle.Docs[spec.SchemaV3])
	if err != nil {
		t.Fatal(err)
	}
	p := loaded.Project

	next, err := editor.Apply(p, editor.Edit{
		Kind: "rule_upstream", RuleNumber: "R-002", UpstreamIndex: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	old := engine.NewEngine(p)
	reconciled, report := engine.Reconcile(old, next, spec.SchemaV3, spec.SchemaV3, nil)
	report.AnnotateQueries(reconciled, bundle.Queries)

	if !containsString(report.RebuiltRegions, "rg-api") {
		t.Fatalf("rg-api must rebuild: %+v", report)
	}
	if !containsString(report.ReusedRegions, "rg-example") {
		t.Fatalf("rg-example must be reused: %+v", report)
	}
	if report.BeforeRevisions["rg-example"] != report.AfterRevisions["rg-example"] {
		t.Fatalf("unaffected revision changed: %+v", report)
	}

	inc, err := engine.RunSamples(context.Background(), reconciled, bundle.Queries, 1)
	if err != nil {
		t.Fatal(err)
	}
	full, err := engine.RunSamples(context.Background(), engine.NewEngine(next), bundle.Queries, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(inc.Results) != len(full.Results) {
		t.Fatal("result length mismatch")
	}
	for i := range inc.Results {
		truth := spec.Truth{
			Code: full.Results[i].Code, Matched: full.Results[i].Matched,
			RuleNumber: full.Results[i].RuleNumber, RuleOrdinal: full.Results[i].RuleOrdinal,
			DecisionBasis: full.Results[i].DecisionBasis, CandidateRules: full.Results[i].CandidateRules,
			UpstreamIndex: full.Results[i].UpstreamIndex, HopCount: full.Results[i].HopCount,
			Source: full.Results[i].Source, Negative: full.Results[i].Negative,
			Records: full.Results[i].Records, RemainingTTLMS: full.Results[i].RemainingTTLMS,
			EntryRemainingMS: full.Results[i].EntryRemainingMS,
			VirtualElapsedMS: full.Results[i].VirtualElapsedMS,
		}
		if !engine.CompareSemantic(inc.Results[i], truth) {
			a, _ := json.MarshalIndent(inc.Results[i], "", "  ")
			b, _ := json.MarshalIndent(full.Results[i], "", "  ")
			t.Fatalf("incremental/full mismatch query %d\ninc=%s\nfull=%s", i+1, a, b)
		}
	}

	schemaOnly, schemaReport := engine.Reconcile(engine.NewEngine(p), p.Clone(), spec.SchemaV1, spec.SchemaV3, nil)
	if !schemaReport.SchemaOnly || len(schemaReport.PropagationClosure) != 0 {
		t.Fatalf("same canonical content should be schema-only: %+v", schemaReport)
	}
	_ = schemaOnly
}

func TestCheckpointSaveRestoreAndInjections(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "dns.sqlite")
	bundle, _ := generator.Generate(42)
	loaded, _ := loader.Load(bundle.Docs[spec.SchemaV3])
	p := loaded.Project
	eng := engine.NewEngine(p)
	report, err := engine.RunSamples(ctx, eng, bundle.Queries, 2)
	if err != nil {
		t.Fatal(err)
	}

	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cp := checkpoint.Checkpoint{
		ProjectVersion: p.Version, SchemaVersion: spec.SchemaV3,
		CommittedSeq: len(report.Results), Segments: eng.SegmentSnapshots(),
		Pending: []checkpoint.PendingFetch{{
			ID: "pending-13", QuerySeq: 13, RegionID: "rg-lab",
			Payload: `{"cache_key":"A:node.lab.test","reason":"deterministic_queue_fixture"}`,
		}},
	}
	for _, r := range report.Results {
		cp.Queries = append(cp.Queries, checkpoint.CommittedQuery{Sequence: r.Sequence, QueryID: r.QueryID, Result: r})
	}
	if err := store.Save(ctx, cp); err != nil {
		t.Fatal(err)
	}
	restored, err := store.LoadRestored(ctx, p.Version)
	if err != nil {
		t.Fatal(err)
	}
	if restored.CommittedSeq != len(bundle.Queries) || len(restored.Queries) != len(bundle.Queries) {
		t.Fatalf("checkpoint sequence mismatch: %+v", restored)
	}
	if len(restored.Segments) != len(eng.SegmentSnapshots()) {
		t.Fatalf("segment count mismatch: %d vs %d", len(restored.Segments), len(eng.SegmentSnapshots()))
	}
	if len(restored.Pending) != 1 || restored.Pending[0].ID != "pending-13" {
		t.Fatalf("pending queue not restored: %+v", restored.Pending)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	mismatchPath := filepath.Join(dir, "mismatch.sqlite")
	mstore, err := checkpoint.Open(mismatchPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := mstore.Save(ctx, checkpoint.Checkpoint{
		ProjectVersion: "wrong-version", CommittedSeq: 0, Segments: map[string]engine.Segment{},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = mstore.LoadRestored(ctx, p.Version)
	assertCode(t, err, codes.ECheckpointVersion)
	_ = mstore.Close()

	truncPath := filepath.Join(dir, "trunc.sqlite")
	tstore, err := checkpoint.Open(truncPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := tstore.Save(ctx, checkpoint.Checkpoint{
		ProjectVersion: p.Version, Segments: map[string]engine.Segment{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tstore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.TruncateForInjection(truncPath, 48); err != nil {
		t.Fatal(err)
	}
	_, err = checkpoint.Open(truncPath)
	assertCode(t, err, codes.ECheckpointTruncated)
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected code %s, got nil", want)
	}
	if got := codes.CodeOf(err); got != want {
		t.Fatalf("code = %s, want %s (%v)", got, want, err)
	}
}

func cloneMap(in map[string]any) map[string]any {
	b, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
