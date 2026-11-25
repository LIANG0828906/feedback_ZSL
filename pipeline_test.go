package plancache

import (
	"fmt"
	"strings"
	"testing"
)

func conclusionSignature(reports []RequestReport) string {
	parts := make([]string, 0, len(reports))
	for _, r := range reports {
		evictions := make([]string, 0)
		if r.Write != nil {
			for _, e := range r.Write.Evictions {
				evictions = append(evictions, e.PlanID+"@"+e.Rule)
			}
		}
		incompatible := make([]string, 0)
		for _, d := range r.SettingDiffs {
			if !d.Compatible {
				incompatible = append(incompatible, d.Key)
			}
		}
		parts = append(parts, fmt.Sprintf("%d|%s|%s|%s|evict=%s|bad=%s",
			r.Seq, r.Decision, r.CacheKey, r.HitPlanID,
			strings.Join(evictions, ","), strings.Join(incompatible, ",")))
	}
	return strings.Join(parts, "\n")
}

func runVersionPipeline(t *testing.T, p *Project) *PipelineReport {
	t.Helper()
	report, _, err := RunParallelPipeline(p)
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	return report
}

func TestCrossSchemaReuseConclusionsMatch(t *testing.T) {
	loaded := loadedVersions(t)
	var want string
	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		report := runVersionPipeline(t, loaded[version])
		got := conclusionSignature(report.RequestReports)
		if want == "" {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("reuse conclusions differ for %s\n--- got ---\n%s\n--- want ---\n%s", version, got, want)
		}
	}

	truth := GenerateProject(DefaultSeed)
	report := runVersionPipeline(t, truth)
	bySeq := map[int]RequestReport{}
	for _, r := range report.RequestReports {
		bySeq[r.Seq] = r
	}
	if got := bySeq[2].Decision; got != DecisionReuseHit || bySeq[2].HitPlanID != "PLAN-S1-users" {
		t.Fatalf("seq2 expected baseline hit, got %+v", bySeq[2])
	}
	if got := bySeq[5].Decision; got != DecisionReuseHit {
		t.Fatalf("ignored application_name difference should still hit, got %s", got)
	}
	if bySeq[4].Decision != DecisionDerivedSettingsRejected {
		t.Fatalf("seq4 expected settings rejection, got %s", bySeq[4].Decision)
	}
	foundIsolation := false
	for _, d := range bySeq[4].SettingDiffs {
		if d.Key == "isolation_level" && !d.Compatible && d.EntryValue == "read_committed" && d.RequestValue == "serializable" {
			foundIsolation = true
		}
	}
	if !foundIsolation {
		t.Fatalf("seq4 missing incompatible isolation_level diff: %+v", bySeq[4].SettingDiffs)
	}
	if bySeq[10].Decision != DecisionDerivedSettingsRejected {
		t.Fatalf("seq10 expected work_mem rejection, got %s", bySeq[10].Decision)
	}
	foundWorkMem := false
	for _, d := range bySeq[10].SettingDiffs {
		if d.Key == "work_mem_kib" && !d.Compatible {
			foundWorkMem = true
		}
	}
	if !foundWorkMem {
		t.Fatalf("seq10 missing incompatible work_mem diff: %+v", bySeq[10].SettingDiffs)
	}
}

func TestConcurrencyAndOrderDoNotChangeConclusions(t *testing.T) {
	truth := GenerateProject(DefaultSeed)
	_, sequential, err := RunSequential(truth)
	if err != nil {
		t.Fatal(err)
	}
	want := conclusionSignature(sequential)
	closureWant := ""

	for _, workers := range []int{1, 2, 4, 8} {
		p := GenerateProject(DefaultSeed)
		p.Runtime.Workers = workers
		report := runVersionPipeline(t, p)
		if got := conclusionSignature(report.RequestReports); got != want {
			t.Fatalf("workers=%d changed conclusions\n%s", workers, got)
		}
		gotClosure := closureSignature(report.Closures)
		if closureWant == "" {
			closureWant = gotClosure
		} else if gotClosure != closureWant {
			t.Fatalf("workers=%d changed closures\n%s", workers, gotClosure)
		}
	}

	reversed := GenerateProject(DefaultSeed)
	for i, j := 0, len(reversed.Requests)-1; i < j; i, j = i+1, j-1 {
		reversed.Requests[i], reversed.Requests[j] = reversed.Requests[j], reversed.Requests[i]
	}
	report := runVersionPipeline(t, reversed)
	if got := conclusionSignature(report.RequestReports); got != want {
		t.Fatalf("reversed submission order changed conclusions\n%s", got)
	}

	batches := GenerateProject(DefaultSeed)
	batchA := append([]SessionRequest(nil), batches.Requests[:5]...)
	batchB := append([]SessionRequest(nil), batches.Requests[5:]...)
	engine, err := NewEngine(batches)
	if err != nil {
		t.Fatal(err)
	}
	for _, batch := range [][]SessionRequest{batchA, batchB} {
		for _, req := range batch {
			if _, err := engine.ProcessRequest(req); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := conclusionSignature(engine.Reports); got != want {
		t.Fatalf("split commit batches changed conclusions\n%s", got)
	}
}

func TestCapacityConstraintsAndTotalBytesEviction(t *testing.T) {
	p := GenerateProject(DefaultSeed)
	p.Capacity.MaxCacheBytes = 2600
	engine, _, err := RunSequential(p)
	if err != nil {
		t.Fatal(err)
	}
	totalRuleEvictions := 0
	for _, wr := range engine.WriteReports {
		if wr.OccupiedBytesFinal > wr.ByteLimit {
			t.Fatalf("byte limit exceeded after seq %d: %d > %d", wr.Seq, wr.OccupiedBytesFinal, wr.ByteLimit)
		}
		activeForStmt := map[string]int{}
		for _, ent := range engine.ActiveEntries() {
			activeForStmt[ent.StatementID]++
		}
		for stmt, n := range activeForStmt {
			if n > wr.PerStatementLimit {
				t.Fatalf("stmt %s exceeds per-statement limit: %d", stmt, n)
			}
		}
		for _, ev := range wr.Evictions {
			if ev.Rule == p.Eviction.TotalBytesRule {
				totalRuleEvictions++
			}
		}
	}
	if totalRuleEvictions == 0 {
		t.Fatal("expected at least one total-byte eviction under tight capacity")
	}

	first := evictionPlanSignature(engine.Evictions)
	for repeat := 0; repeat < 3; repeat++ {
		rerun, _, err := RunSequential(GenerateProject(DefaultSeed))
		if err != nil {
			t.Fatal(err)
		}
		_ = evictionPlanSignature(rerun.Evictions)
	}
	// Tight-capacity rerun must be identical too.
	tightRerunProject := GenerateProject(DefaultSeed)
	tightRerunProject.Capacity.MaxCacheBytes = 2600
	tightRerun, _, err := RunSequential(tightRerunProject)
	if err != nil {
		t.Fatal(err)
	}
	if got := evictionPlanSignature(tightRerun.Evictions); got != first {
		t.Fatalf("eviction order changed:\n%s\nvs\n%s", got, first)
	}
}

func evictionPlanSignature(evs []EvictionRecord) string {
	parts := make([]string, 0, len(evs))
	for _, e := range evs {
		parts = append(parts, fmt.Sprintf("%s:%s:%d", e.PlanID, e.Rule, e.LastUsedSeq))
	}
	return strings.Join(parts, "\n")
}

func closureSignature(reports []ClosureReport) string {
	parts := make([]string, 0, len(reports))
	for _, r := range reports {
		for _, item := range r.Items {
			parts = append(parts, fmt.Sprintf("%s|%s|%d|%s", r.EventID, item.PlanID, item.Generation, strings.Join(item.Path, "->")))
		}
	}
	return strings.Join(parts, "\n")
}

func TestEvictedPlanIsNeverReused(t *testing.T) {
	p := GenerateProject(DefaultSeed)
	engine, reports, err := RunSequential(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reports {
		if r.HitPlanID == "" {
			continue
		}
		ent, ok := engine.Entry(r.HitPlanID)
		if !ok {
			t.Fatalf("hit plan %s not found", r.HitPlanID)
		}
		if ent.Evicted || ent.Invalidated {
			t.Fatalf("request %d reused unavailable plan %s", r.Seq, r.HitPlanID)
		}
	}

	evicted := map[string]bool{}
	for _, ev := range engine.Evictions {
		evicted[ev.PlanID] = true
	}
	if len(evicted) == 0 {
		t.Fatal("fixture should evict at least one S1 variant")
	}
	for _, r := range reports {
		if evicted[r.HitPlanID] {
			t.Fatalf("evicted plan %s was reused at seq %d", r.HitPlanID, r.Seq)
		}
	}
}

func TestZeroCapacityReturnsStableCode(t *testing.T) {
	p := GenerateProject(DefaultSeed)
	p.Capacity.MaxCacheBytes = 0
	_, _, err := RunSequential(p)
	assertCode(t, err, CodeCapacityBytesZero)
}

func TestCacheKeySetIndependentOfStatementOrder(t *testing.T) {
	p := GenerateProject(DefaultSeed)
	first, err := DeriveAllKeys(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	shuffled := GenerateProject(DefaultSeed)
	for i, j := 0, len(shuffled.Statements)-1; i < j; i, j = i+1, j-1 {
		shuffled.Statements[i], shuffled.Statements[j] = shuffled.Statements[j], shuffled.Statements[i]
	}
	second, err := DeriveAllKeys(shuffled, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(KeySet(first), ",") != strings.Join(KeySet(second), ",") {
		t.Fatal("cache key set changed when statement order changed")
	}
}

func TestBadRequestLeavesCommittedEntriesUsable(t *testing.T) {
	p := GenerateProject(DefaultSeed)
	engine, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	req1 := p.Requests[0]
	if _, err := engine.ProcessRequest(req1); err != nil {
		t.Fatal(err)
	}
	req2 := p.Requests[1]
	if _, err := engine.ProcessRequest(req2); err != nil {
		t.Fatal(err)
	}

	bad := req1
	bad.RequestID = "req-bad"
	bad.StatementID = "S404-missing"
	_, err = engine.ProcessRequest(bad)
	assertCode(t, err, CodeRequestNotFound)

	hit, err := engine.ProcessRequest(req2)
	if err != nil {
		t.Fatal(err)
	}
	if hit.Decision != DecisionReuseHit || hit.HitPlanID != "PLAN-S1-users" {
		t.Fatalf("committed entry became unavailable after bad request: %+v", hit)
	}
}

func TestPipelineDoesNotMutateSourceProject(t *testing.T) {
	p := GenerateProject(DefaultSeed)
	before := canonicalJSON(p)
	if _, _, err := RunParallelPipeline(p); err != nil {
		t.Fatal(err)
	}
	after := canonicalJSON(p)
	if before != after {
		t.Fatal("parallel pipeline mutated source project data")
	}
}
