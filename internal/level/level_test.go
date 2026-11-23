package level

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func expectCode(t *testing.T, err error, code ErrCode) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", code)
	}
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error with code %s, got %v", code, err)
	}
	if e.Code != code {
		t.Fatalf("expected code %s, got %s (%v)", code, e.Code, e)
	}
	return e
}

func TestValidationErrors(t *testing.T) {
	proj := BasicProject()
	valid, _ := json.MarshalIndent(proj, "", "  ")

	var missing map[string]any
	mustJSON(t, valid, &missing)
	delete(missing, "global_seed")
	if e := expectCode(t, parseProjectErr(missing), ESchema); e.Field != "project.global_seed" {
		t.Fatalf("want field project.global_seed, got %s", e.Field)
	}

	bad := BasicProject()
	bad.GridRows = 0
	expectCode(t, ValidateProject(bad), EPara)

	bad = BasicProject()
	bad.Partitions[2].BudgetMin = -5
	if e := expectCode(t, ValidateProject(bad), EPara); e.Partition != 2 || e.Field != "budget_min" {
		t.Fatalf("bad locating context: %+v", e)
	}

	bad = BasicProject()
	bad.Partitions[4].ResourceCap = bad.GlobalResourceCap + 1
	expectCode(t, ValidateProject(bad), EPara)

	ops := BasicOps()
	ops[0].Partition = 999
	if e := expectCode(t, ValidateOps(proj, ops), EPara); e.Partition != 999 {
		t.Fatalf("want partition 999, got %d", e.Partition)
	}

	expectCode(t, ValidateOps(proj, HistRegressionOps()), EHist)

	_, err := NewSession(proj, HistRegressionOps(), nil, nil)
	e := expectCode(t, err, EHist)
	if e.OpSeq != 2 {
		t.Fatalf("want op_seq 2, got %d", e.OpSeq)
	}
}

func parseProjectErr(v any) error {
	data, _ := json.MarshalIndent(v, "", "  ")
	_, err := ParseProject(data)
	return err
}

func mustJSON(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func TestDeterministicGeneration(t *testing.T) {
	proj := BasicProject()
	pc := &proj.Partitions[7]
	a, err := GeneratePartition(proj, pc, proj.PreciseDensity)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GeneratePartition(proj, pc, proj.PreciseDensity)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Canonical(), b.Canonical()) {
		t.Fatal("same partition generated twice differs")
	}

	ids := []int{0, 1, 2, 3, 4, 5, 6, 7}
	runWith := func(workers, buffers int) map[int][]byte {
		pool := NewPool(workers, buffers)
		jobs := make([]GenJob, 0, len(ids))
		for _, id := range ids {
			pc := proj.Partitions[id]
			jobs = append(jobs, GenJob{PartitionID: id, Fn: func() (*PartitionResult, error) {
				return GeneratePartition(proj, &pc, proj.PreciseDensity)
			}})
		}
		out := map[int][]byte{}
		for _, oc := range pool.Run(jobs) {
			if oc.Err != nil {
				t.Fatal(oc.Err)
			}
			out[oc.PartitionID] = oc.Result.Canonical()
		}
		return out
	}
	slow := runWith(1, 1)
	fast := runWith(8, 4)
	for id, want := range slow {
		if !bytes.Equal(want, fast[id]) {
			t.Fatalf("partition %d differs across worker/buffer configs", id)
		}
	}
}

func TestTruthMatch(t *testing.T) {
	proj := BasicProject()
	truth, err := BuildTruth(proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckTruth(proj, truth); err != nil {
		t.Fatal(err)
	}
	truth.Partitions[3].ObstacleCount++
	if err := CheckTruth(proj, truth); err == nil {
		t.Fatal("tampered truth not detected")
	}
}

func TestBudgetTradeoff(t *testing.T) {
	proj := BasicProject()
	pc := &proj.Partitions[0]
	pc.ObstacleQuota = 8
	pc.BudgetMin = 0
	pc.BudgetMax = 300
	pc.ResourceCap = 0
	res, err := GeneratePartition(proj, pc, proj.PreciseDensity)
	if err != nil {
		t.Fatal(err)
	}
	if res.Difficulty > pc.BudgetMax {
		t.Fatalf("difficulty %d above budget max %d", res.Difficulty, pc.BudgetMax)
	}
	if len(res.Tradeoffs) == 0 {
		t.Fatal("expected tradeoff records")
	}
	for _, tr := range res.Tradeoffs {
		if tr.Partition != 0 || tr.CandidateID == "" || tr.Reason == "" {
			t.Fatalf("incomplete tradeoff record: %+v", tr)
		}
	}

	scenario := BudgetScenarioProject()
	bad := &scenario.Partitions[1]
	_, err = GeneratePartition(scenario, bad, scenario.PreciseDensity)
	e := expectCode(t, err, EBudget)
	if e.Partition != 1 || e.Difficulty >= e.BudgetMin || e.BudgetMin != 20000 {
		t.Fatalf("bad E_BUDGET context: %+v", e)
	}
}

func TestConnectivityRepair(t *testing.T) {
	proj := ConnScenarioProject()
	sess, err := NewSession(proj, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sess.Connectivity().Connected {
		t.Fatal("expected repaired connectivity")
	}
	if len(sess.Connectivity().Repairs) == 0 {
		t.Fatal("expected boundary repairs")
	}
	for id, res := range sess.Parts() {
		pc := proj.PartitionByID(id)
		if res.Difficulty < pc.BudgetMin || res.Difficulty > pc.BudgetMax {
			t.Fatalf("partition %d out of budget after repair: %d not in [%d,%d]",
				id, res.Difficulty, pc.BudgetMin, pc.BudgetMax)
		}
	}
}

func TestConnectivityFail(t *testing.T) {
	proj := ConnFailProject()
	_, err := NewSession(proj, nil, nil, nil)
	e := expectCode(t, err, EConn)
	if len(e.Unreachable) == 0 {
		t.Fatal("E_CONN must list unreachable targets")
	}
	found := false
	for _, id := range e.Unreachable {
		if id == "t1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected t1 unreachable, got %v", e.Unreachable)
	}
	if len(e.Path) == 0 {
		t.Fatal("E_CONN must list traversed partition ids")
	}
}

func TestThreePathConsistency(t *testing.T) {
	proj := BasicProject()
	ops := BasicOps()
	sess, err := NewSession(proj, ops, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.ApplyAll(); err != nil {
		t.Fatal(err)
	}
	if err := CompareThreePaths(sess, 5); err != nil {
		t.Fatal(err)
	}

	// Every commit manifest must mark unaffected partitions as reused.
	last := sess.Commits()[len(sess.Commits())-1]
	reused := 0
	for _, e := range last.Manifest.Entries {
		if e.Reused {
			reused++
		}
	}
	if reused == 0 {
		t.Fatal("expected reused partitions in incremental manifest")
	}
}

func TestComparePathsDetectsDiff(t *testing.T) {
	proj := BasicProject()
	sess, err := NewSession(proj, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := sess.Capture()
	b := sess.Capture()
	b.Parts[3].Difficulty++
	err = ComparePaths(a, b)
	e := expectCode(t, err, EDiff)
	if e.Partition != 3 || e.Item != "difficulty" {
		t.Fatalf("bad E_DIFF context: %+v", e)
	}
}

func TestPreviewFinalize(t *testing.T) {
	proj := BasicProject()
	ops := BasicOps()
	sess, err := NewSession(proj, ops, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(t.TempDir(), "diverge.json")
	for sess.HasNext() {
		op := sess.NextOp()
		preview, err := sess.PreviewOp(op)
		if err != nil {
			t.Fatal(err)
		}
		if preview.ElapsedMs >= 100 {
			t.Fatalf("preview took %.2fms, exceeds 100ms budget", preview.ElapsedMs)
		}
		if preview.Density != proj.PreviewDensity || len(preview.Partitions) == 0 || preview.Basis == "" {
			t.Fatalf("preview report incomplete: %+v", preview)
		}
		report, err := sess.FinalizePreview(preview, reportPath)
		if err != nil {
			t.Fatalf("op %d diverged: %v", op.Seq, err)
		}
		if report.Item != "none" {
			t.Fatalf("unexpected divergence item %s", report.Item)
		}
		if _, err := sess.ApplyNextOp(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatal("divergence report not exported")
	}
}

func TestDivergeDetection(t *testing.T) {
	proj := BasicProject()
	sess, err := NewSession(proj, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	op := EditOp{Seq: 1, Timestamp: 1, Kind: OpSetResourceCap, Partition: 2, Value: 3}
	preview, err := sess.PreviewOp(op)
	if err != nil {
		t.Fatal(err)
	}
	preview.GlobalResourceTotal += 5
	_, err = sess.FinalizePreview(preview, "")
	e := expectCode(t, err, EDiverge)
	if e.Report == nil || e.Report.ResourceDiff != 5 || e.Item != "global_resource_total" {
		t.Fatalf("bad E_DIVERGE report: %+v", e.Report)
	}
}

func TestRollbackReplay(t *testing.T) {
	proj := BasicProject()
	ops := BasicOps()
	continuous, err := NewSession(proj, ops, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := continuous.ApplyAll(); err != nil {
		t.Fatal(err)
	}
	want := continuous.Capture()

	replayed, err := NewSession(proj, ops, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := replayed.ApplyAll(); err != nil {
		t.Fatal(err)
	}
	if err := replayed.Rollback(4); err != nil {
		t.Fatal(err)
	}
	if err := replayed.ApplyAll(); err != nil {
		t.Fatal(err)
	}
	if err := ComparePaths(want, replayed.Capture()); err != nil {
		t.Fatal(err)
	}
}

func TestForcedKillResume(t *testing.T) {
	dir := t.TempDir()
	proj := BasicProject()
	ops := BasicOps()

	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewSession(proj, ops, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := sess.ApplyNextOp(); err != nil {
			t.Fatal(err)
		}
	}

	// Simulate a kill mid-commit: a half-written commit-4 file exists but
	// HEAD still points at 3.
	if err := os.WriteFile(filepath.Join(dir, "commit-000004.json"), []byte(`{"op_seq": 4, corrupt`), 0o644); err != nil {
		t.Fatal(err)
	}

	resumed, err := OpenSession(proj, ops, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resumed.Commits()) - 1; got != 3 {
		t.Fatalf("expected resume at op 3, got %d", got)
	}
	if err := resumed.ApplyAll(); err != nil {
		t.Fatal(err)
	}

	continuous, err := NewSession(proj, ops, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := continuous.ApplyAll(); err != nil {
		t.Fatal(err)
	}
	if err := ComparePaths(continuous.Capture(), resumed.Capture()); err != nil {
		t.Fatal(err)
	}
}

func TestVersionMismatchInvalidatesHistory(t *testing.T) {
	dir := t.TempDir()
	proj := BasicProject()
	ops := BasicOps()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewSession(proj, ops, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := sess.ApplyNextOp(); err != nil {
			t.Fatal(err)
		}
	}

	mutated := proj.Clone()
	mutated.SubSeedVersion = 2
	reopened, err := OpenSession(mutated, ops, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reopened.Commits()) - 1; got != 0 {
		t.Fatalf("expected history invalidated to commit 0, got %d", got)
	}
}

func TestPoolIsolationAndRetry(t *testing.T) {
	proj := BasicProject()
	ops := []EditOp{
		{Seq: 1, Timestamp: 100, Kind: OpSetBudget, Partition: 5, Value: 0, Value2: 9000},
		{Seq: 2, Timestamp: 200, Kind: OpSetBudget, Partition: 6, Value: 0, Value2: 9000},
	}
	sess, err := NewSession(proj, ops, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	commitsBefore := len(sess.Commits())

	crashed := map[int]bool{5: true}
	base := sess.GenFunc
	sess.GenFunc = func(p *Project, pc *PartitionConstraint, density int) (*PartitionResult, error) {
		if crashed[pc.ID] {
			panic("simulated worker crash")
		}
		return base(p, pc, density)
	}
	if _, err := sess.ApplyNextOp(); err == nil {
		t.Fatal("expected failure from panicking partition")
	}
	if len(sess.Commits()) != commitsBefore {
		t.Fatal("failed op must not commit")
	}

	// Retry with the fault cleared: the op succeeds and previously
	// committed state is untouched.
	delete(crashed, 5)
	if _, err := sess.ApplyNextOp(); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.ApplyNextOp(); err != nil {
		t.Fatal(err)
	}

	clean, err := NewSession(proj, ops, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := clean.ApplyAll(); err != nil {
		t.Fatal(err)
	}
	if err := ComparePaths(clean.Capture(), sess.Capture()); err != nil {
		t.Fatal(err)
	}
}

func TestSampleFilesDeterministic(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	if err := WriteSample(dir1, "basic"); err != nil {
		t.Fatal(err)
	}
	if err := WriteSample(dir2, "basic"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"project.json", "ops.json", "truth.json", "meta.json"} {
		a, err := os.ReadFile(filepath.Join(dir1, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir2, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("sample file %s not deterministic", name)
		}
	}
}

func TestBudgetScenarioSampleFailsWithEBudget(t *testing.T) {
	proj := BudgetScenarioProject()
	_, err := NewSession(proj, nil, nil, nil)
	expectCode(t, err, EBudget)
}

func TestOpValueValidation(t *testing.T) {
	proj := BasicProject()
	ops := []EditOp{
		{Seq: 1, Timestamp: 100, Kind: OpSetResourceCap, Partition: 1, Value: 99},
	}
	sess, err := NewSession(proj, ops, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sess.ApplyNextOp()
	e := expectCode(t, err, EPara)
	if e.OpSeq != 1 || e.Partition != 1 {
		t.Fatalf("bad E_PARA context: %+v", e)
	}
}

func ExampleDeriveSubSeed() {
	fmt.Println(DeriveSubSeed(42, 1, 2, 7, 1) == DeriveSubSeed(42, 1, 2, 7, 1))
	// Output: true
}
