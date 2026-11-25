package acceptance_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"quotaengine/gen"
	"quotaengine/quota"
)

func TestThreeSchemaLoadAndTruth(t *testing.T) {
	g, err := gen.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	docs := []struct {
		name string
		raw  []byte
	}{{"v1", g.Documents.V1}, {"v2", g.Documents.V2}, {"v3", g.Documents.V3}}
	var projects []*quota.Project
	for _, doc := range docs {
		before := append([]byte(nil), doc.raw...)
		p, err := quota.LoadWithOptions(doc.raw, quota.LoadOptions{ExpectedVersion: doc.name})
		if err != nil {
			t.Fatalf("%s load: %v", doc.name, err)
		}
		if !bytes.Equal(before, doc.raw) {
			t.Fatalf("%s loader mutated source bytes", doc.name)
		}
		projects = append(projects, p)
	}
	for i := 1; i < len(projects); i++ {
		equal, err := quota.Equivalent(projects[0], projects[i])
		if err != nil {
			t.Fatal(err)
		}
		if !equal {
			t.Fatalf("schema %s is not semantically equal", docs[i].name)
		}
	}
	for _, p := range projects {
		top := p.Topology()
		if len(top) != 2 || top[0].ID != "rack-a" || top[1].ID != "rack-b" {
			t.Fatalf("unexpected topology: %+v", top)
		}
	}
	for _, workers := range []int{1, 2, 4, 8} {
		run, err := quota.Run(context.Background(), projects[1], quota.Options{Workers: workers, QueuePerTenant: 64})
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if err := g.Truth.Check(run); err != nil {
			t.Fatalf("workers=%d truth mismatch: %v", workers, err)
		}
		for _, step := range run.Steps {
			for _, ledger := range step.Tenants {
				if ledger.UsedMilli > ledger.QuotaMilli+ledger.BurstMilli {
					t.Fatalf("tenant %s used=%d exceeds hard cap=%d at %dms",
						ledger.TenantID, ledger.UsedMilli, ledger.QuotaMilli+ledger.BurstMilli, step.TimeMilli)
				}
			}
		}
	}
}

func TestEventSubmissionOrderDoesNotChangeOutcome(t *testing.T) {
	g, err := gen.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	p, err := quota.Load(g.Documents.V2)
	if err != nil {
		t.Fatal(err)
	}
	base, err := quota.Run(context.Background(), p, quota.Options{Workers: 2, QueuePerTenant: 64})
	if err != nil {
		t.Fatal(err)
	}
	shuffled := &quota.Project{
		SchemaVersion: p.SchemaVersion, Seed: p.Seed,
		Tenants: append([]quota.Tenant(nil), p.Tenants...),
		Nodes:   append([]quota.Node(nil), p.Nodes...),
	}
	for i := range shuffled.Nodes {
		shuffled.Nodes[i].AllowedTenants = append([]string(nil), p.Nodes[i].AllowedTenants...)
	}
	// Reverse each timestamp group while retaining its stable sequence field.
	for start := 0; start < len(p.Events); {
		end := start + 1
		for end < len(p.Events) && p.Events[end].TimeMilli == p.Events[start].TimeMilli {
			end++
		}
		for i := start; i < end; i++ {
			shuffled.Events = append(shuffled.Events, p.Events[end-1-(i-start)])
		}
		start = end
	}
	got, err := quota.Run(context.Background(), shuffled, quota.Options{Workers: 4, QueuePerTenant: 64})
	if err != nil {
		t.Fatal(err)
	}
	if err := quota.CompareTimelines(got, base); err != nil {
		t.Fatal(err)
	}
}

func TestGuaranteePriorityAndReservation(t *testing.T) {
	orderProject := &quota.Project{
		SchemaVersion: "v2",
		Tenants: []quota.Tenant{
			{ID: "t-low", QuotaBP: 10000, GuaranteeBP: 3000, Priority: 1},
			{ID: "t-high", QuotaBP: 6000, GuaranteeBP: 2000, BurstBP: 5000, Priority: 9},
		},
		Nodes: []quota.Node{{ID: "n1", CapacityMilli: 1200, RackID: "r1", AllowedTenants: []string{"t-low", "t-high"}}},
		Events: []quota.Event{
			{Seq: 1, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-high", NodeID: "n1", AmountMilli: 700},
			{Seq: 2, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-low", NodeID: "n1", AmountMilli: 300},
		},
	}
	run, err := quota.Run(context.Background(), orderProject, quota.Options{Workers: 1, QueuePerTenant: 4})
	if err != nil {
		t.Fatal(err)
	}
	decisions := run.Steps[0].Decisions
	if len(decisions) != 2 {
		t.Fatalf("decisions: %+v", decisions)
	}
	if decisions[0].TenantID != "t-low" || decisions[0].Action != "admitted" ||
		decisions[1].TenantID != "t-high" || decisions[1].Action != "admitted" {
		t.Fatalf("guarantee class did not precede burst/high-priority class: %+v", decisions)
	}

	reserveProject := &quota.Project{
		SchemaVersion: "v2",
		Tenants: []quota.Tenant{
			{ID: "t-low", QuotaBP: 10000, GuaranteeBP: 3000, Priority: 1},
			{ID: "t-high", QuotaBP: 6000, GuaranteeBP: 2000, BurstBP: 5000, Priority: 9},
		},
		Nodes: []quota.Node{{ID: "n1", CapacityMilli: 1000, RackID: "r1", AllowedTenants: []string{"t-low", "t-high"}}},
		Events: []quota.Event{
			{Seq: 1, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-high", NodeID: "n1", AmountMilli: 800},
		},
	}
	run, err = quota.Run(context.Background(), reserveProject, quota.Options{Workers: 1, QueuePerTenant: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Steps[0].Decisions) != 1 || run.Steps[0].Decisions[0].Constraint != quota.ConstraintGuaranteeReserve {
		t.Fatalf("burst request should be stopped by guarantee reserve: %+v", run.Steps[0].Decisions)
	}
	if len(run.Steps[0].Queue) != 1 {
		t.Fatalf("rejected request must enter waiting queue: %+v", run.Steps[0].Queue)
	}
}

func TestPrecisionAndCrossSchemaQuota(t *testing.T) {
	g, err := gen.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	loaded := []*quota.Project{}
	for _, raw := range [][]byte{g.Documents.V1, g.Documents.V2, g.Documents.V3} {
		p, err := quota.Load(raw)
		if err != nil {
			t.Fatal(err)
		}
		loaded = append(loaded, p)
	}
	for _, tenantID := range []string{"t-alpha", "t-beta", "t-gamma", "t-delta"} {
		var want int64 = -1
		for i, p := range loaded {
			tt, _ := p.Tenant(tenantID)
			c, err := quota.QuotaForNode(4000, tt.QuotaBP, 1000, "cross_schema")
			if err != nil {
				t.Fatalf("schema %d quota: %v", i+1, err)
			}
			if want < 0 {
				want = c.Result
			} else if c.Result != want || c.Overflow {
				t.Fatalf("schema %d quota differs: got=%d want=%d", i+1, c.Result, want)
			}
		}
	}
	for _, c := range gen.CriticalConversions() {
		conv, err := quota.QuotaForNode(c.CapacityMilli, c.RatioBP, c.WeightPermille, c.CaseID)
		report := quota.CheckPrecision(conv, c.Truth, quota.DefaultToleranceMilli, c.CaseID)
		if c.CaseID == "permille-overflow" {
			if err == nil || !conv.Overflow || report.Accepted {
				t.Fatalf("overflow case must be marked and rejected: conv=%+v err=%v report=%+v", conv, err, report)
			}
			continue
		}
		if err != nil || !report.Accepted || report.ExceedsTolerance {
			t.Fatalf("critical case %s rejected: conv=%+v err=%v report=%+v", c.CaseID, conv, err, report)
		}
	}
	conv, err := quota.ScaleRatio(922337203685477580, 1000, 10000, "overflow")
	if err == nil || !conv.Overflow || conv.ErrorCode != quota.CodeOverflow || !conv.Saturated {
		t.Fatalf("ratio overflow not explicit: conv=%+v err=%v", conv, err)
	}
	miss := quota.CheckPrecision(quota.Conversion{Result: 11}, 10, 0, "tolerance-miss")
	if !miss.ExceedsTolerance || miss.Accepted {
		t.Fatalf("tolerance miss not marked: %+v", miss)
	}
}

func TestIncrementalEditsMatchFullRecompute(t *testing.T) {
	g, err := gen.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	p, err := quota.Load(g.Documents.V2)
	if err != nil {
		t.Fatal(err)
	}
	base, err := quota.Run(context.Background(), p, quota.Options{Workers: 3, QueuePerTenant: 64})
	if err != nil {
		t.Fatal(err)
	}
	edits := []quota.Edit{
		{Kind: quota.EditNodeCapacity, NodeID: "n-core-1", TimeMilli: 0, Value: 5000},
		{Kind: quota.EditTenantQuota, TenantID: "t-alpha", TimeMilli: 1000, Value: 3500},
		{Kind: quota.EditTenantGuarantee, TenantID: "t-delta", TimeMilli: 3000, Value: 1000},
		{Kind: quota.EditWeightEvent, TenantID: "t-gamma", EventSeq: 12, TimeMilli: 3000, Value: 2000},
	}
	for _, edit := range edits {
		result, err := quota.ApplyEdit(context.Background(), p, quota.Options{Workers: 2, QueuePerTenant: 64}, base, edit)
		if err != nil {
			t.Fatalf("%s: %v", edit.Kind, err)
		}
		if !result.FullEquivalent || result.RecomputedSteps == 0 || len(result.Closure) == 0 {
			t.Fatalf("%s did not report closure/equivalence: %+v", edit.Kind, result)
		}
		if edit.TimeMilli > 0 && result.ReusedSteps == 0 {
			t.Fatalf("%s did not reuse prefix", edit.Kind)
		}
		for _, inv := range result.Invalidation {
			if inv.TimeMilli < edit.TimeMilli {
				continue
			}
			if inv.Reused {
				t.Fatalf("%s incorrectly marked impacted time as reused: %+v", edit.Kind, inv)
			}
		}
	}
}

func TestCheckpointResumeVersionMismatchAndTruncation(t *testing.T) {
	g, err := gen.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	p, err := quota.Load(g.Documents.V2)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := quota.OpenCheckpoint(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	fullRun, err := quota.RunWithCheckpointLimit(context.Background(), p, quota.Options{Workers: 3, QueuePerTenant: 64}, store, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !fullRun.Interrupted || len(fullRun.Timeline.Steps) != 3 {
		t.Fatalf("expected three committed steps: %+v", fullRun)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = quota.OpenCheckpoint(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := quota.RunWithCheckpoint(context.Background(), p, quota.Options{Workers: 2, QueuePerTenant: 64}, store)
	if err != nil {
		t.Fatal(err)
	}
	if !rest.Resumed || rest.ResumedFromMilli != fullRun.Timeline.Steps[2].TimeMilli {
		t.Fatalf("resume metadata wrong: %+v", rest)
	}
	combined := &quota.Timeline{
		ProjectFingerprint: rest.Timeline.ProjectFingerprint,
		Steps:              append(append([]quota.TimeStep(nil), fullRun.Timeline.Steps...), rest.Timeline.Steps...),
		FinalState:         rest.Timeline.FinalState,
	}
	baseline, err := quota.Run(context.Background(), p, quota.Options{Workers: 4, QueuePerTenant: 64})
	if err != nil {
		t.Fatal(err)
	}
	if err := quota.CompareTimelines(combined, baseline); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Load(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.State.Committed || snap.Meta.CommittedTimeMilli != baseline.Steps[len(baseline.Steps)-1].TimeMilli {
		t.Fatalf("final checkpoint metadata: %+v", snap.Meta)
	}
	if _, err := store.Load(context.Background(), g.Project); err == nil || quota.ErrorCode(err) != quota.CodeVersionMismatch {
		t.Fatalf("schema version mismatch: %v", err)
	}
	if err := store.SetCheckpointVersionForDiagnostics(context.Background(), "quota-checkpoint-v0"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), p); err == nil || quota.ErrorCode(err) != quota.CodeCheckpointVersion {
		t.Fatalf("checkpoint version mismatch: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	truncated := filepath.Join(t.TempDir(), "truncated.db")
	if err := gen.TruncateCopy(dbPath, truncated); err != nil {
		t.Fatal(err)
	}
	if _, err := quota.OpenCheckpoint(truncated); err == nil || quota.ErrorCode(err) != quota.CodeCheckpointTruncated {
		t.Fatalf("truncated checkpoint code: %v", err)
	}

	restartPath := filepath.Join(t.TempDir(), "restart.db")
	restartStore, err := quota.OpenCheckpoint(restartPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restartStore.Close()
	if _, err := quota.RunWithCheckpoint(context.Background(), p, quota.Options{Workers: 2, QueuePerTenant: 64}, restartStore); err != nil {
		t.Fatal(err)
	}
	v1, err := quota.Load(g.Documents.V1)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := quota.RunWithCheckpoint(context.Background(), v1, quota.Options{Workers: 2, QueuePerTenant: 64}, restartStore)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.Restarted || restarted.RestartReason != quota.CodeVersionMismatch || len(restarted.Timeline.Steps) != len(baseline.Steps) {
		t.Fatalf("version mismatch did not restart from origin: %+v", restarted)
	}
}

func TestExceptionCodesAndSourceImmutability(t *testing.T) {
	g, err := gen.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), g.Documents.V2...)
	cases := []struct {
		name string
		fn   func([]byte) ([]byte, map[string]any, error)
		code string
	}{
		{"negative-quota", gen.InjectNegativeQuota, quota.CodeNegativeQuota},
		{"guarantee-over-capacity", gen.InjectGuaranteeOverCapacity, quota.CodeGuaranteeOverCapacity},
		{"quota-scale-overflow", gen.InjectQuotaScaleOverflow, quota.CodeQuotaRange},
		{"unknown-schema", gen.InjectUnknownSchema, quota.CodeSchemaUnknown},
	}
	for _, tc := range cases {
		mutated, _, err := tc.fn(g.Documents.V2)
		if err != nil {
			t.Fatalf("%s mutate: %v", tc.name, err)
		}
		if _, err := quota.Load(mutated); err == nil || quota.ErrorCode(err) != tc.code {
			t.Fatalf("%s code: %v", tc.name, err)
		}
	}
	if !bytes.Equal(original, g.Documents.V2) {
		t.Fatal("fault injection mutated source schema document")
	}
	p, err := quota.Load(g.Documents.V2)
	if err != nil {
		t.Fatal(err)
	}
	fpBefore, _ := p.Fingerprint()
	if _, err := quota.Run(context.Background(), p, quota.Options{Workers: 65, QueuePerTenant: 64}); err == nil || quota.ErrorCode(err) != quota.CodeInvalidBudget {
		t.Fatalf("worker budget code: %v", err)
	}
	if _, err := quota.Run(context.Background(), p, quota.Options{Workers: 2, QueuePerTenant: 4097}); err == nil || quota.ErrorCode(err) != quota.CodeInvalidBudget {
		t.Fatalf("queue budget code: %v", err)
	}
	queuePressure := &quota.Project{
		SchemaVersion: "v2",
		Tenants:       []quota.Tenant{{ID: "t-zero", QuotaBP: 0, GuaranteeBP: 0, Priority: 0}},
		Nodes:         []quota.Node{{ID: "n1", CapacityMilli: 1000, RackID: "r1", AllowedTenants: []string{"t-zero"}}},
		Events: []quota.Event{
			{Seq: 1, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-zero", NodeID: "n1", AmountMilli: 1000},
			{Seq: 2, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-zero", NodeID: "n1", AmountMilli: 1000},
		},
	}
	if _, err := quota.Run(context.Background(), queuePressure, quota.Options{Workers: 1, QueuePerTenant: 1}); err == nil || quota.ErrorCode(err) != quota.CodeBackpressureQueue {
		t.Fatalf("queue backpressure code: %v", err)
	}
	constrained, err := quota.Run(context.Background(), p, quota.Options{Workers: 1, QueuePerTenant: 64})
	if err != nil {
		t.Fatal(err)
	}
	workerPressure := false
	for _, step := range constrained.Steps {
		for _, bp := range step.Backpressure {
			if bp.Code == quota.CodeBackpressureWorker {
				workerPressure = true
			}
		}
	}
	if !workerPressure {
		t.Fatal("worker budget pressure was not observable")
	}
	weightedGuarantee := &quota.Project{
		SchemaVersion: "v2",
		Tenants: []quota.Tenant{
			{ID: "t-a", QuotaBP: 10000, GuaranteeBP: 3000, Priority: 1},
			{ID: "t-b", QuotaBP: 10000, GuaranteeBP: 3000, Priority: 2},
		},
		Nodes: []quota.Node{{ID: "n1", CapacityMilli: 1000, RackID: "r1", AllowedTenants: []string{"t-a", "t-b"}}},
		Events: []quota.Event{
			{Seq: 1, TimeMilli: 0, Type: quota.EventWeight, TenantID: "t-a", WeightPermille: 4000},
		},
	}
	if _, err := quota.Run(context.Background(), weightedGuarantee, quota.Options{Workers: 1, QueuePerTenant: 4}); err == nil || quota.ErrorCode(err) != quota.CodeGuaranteeOverCapacity {
		t.Fatalf("weighted guarantee overflow code: %v", err)
	}
	fpAfter, _ := p.Fingerprint()
	if fpBefore != fpAfter {
		t.Fatal("exception path mutated source project")
	}
}

func TestBusinessErrorHasLocatableContext(t *testing.T) {
	g, err := gen.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	mutated, _, err := gen.InjectNegativeQuota(g.Documents.V2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = quota.Load(mutated)
	var be *quota.BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("not a business error: %v", err)
	}
	if be.Code != quota.CodeNegativeQuota || be.Path == "" || be.TenantID == "" {
		t.Fatalf("missing stable context: %+v", be)
	}
}
