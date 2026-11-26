package meshpolicy_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"meshpolicy/internal/engine"
	"meshpolicy/internal/gen"
	"meshpolicy/internal/model"
	"meshpolicy/internal/sched"
	"meshpolicy/internal/schema"
	"meshpolicy/internal/store"
	"meshpolicy/internal/workbench"
	"meshpolicy/internal/xerr"
)

func TestAdmissionReplacementTruthAndGuards(t *testing.T) {
	sample := gen.Generate(gen.DefaultSeed)
	for _, scenario := range sample.Truth.Replacements {
		p := model.CloneProject(sample.Project)
		req := engine.ReplaceRequest{
			Namespace: scenario.Namespace, TargetID: scenario.TargetID, AtSeq: scenario.AtSeq,
			NewPolicy: scenario.NewPolicy,
		}
		result, err := engine.ReplacePolicy(p, req)
		if err != nil {
			t.Fatalf("replace %s: %v", scenario.TargetID, err)
		}
		if result.RuleID != scenario.RuleID {
			t.Fatalf("rule for %s = %s want %s", scenario.TargetID, result.RuleID, scenario.RuleID)
		}
		target, _ := p.PolicyByID(scenario.TargetID)
		if target.State != model.StateReplaced || target.ReplacedBy != scenario.NewPolicy.ID {
			t.Fatalf("target %s was not marked replaced: %+v", scenario.TargetID, target)
		}
		report, err := engine.Run(p, engine.RunOptions{Workers: 2, BufferPerNS: 64})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range report.Decisions {
			if d.AppliedPolicyID == scenario.TargetID && d.Seq >= scenario.AtSeq {
				t.Fatalf("replaced policy %s still applied at %s", scenario.TargetID, d.AttemptID)
			}
		}
		markerChanges := map[string]bool{}
		for _, layer := range report.Propagation.Layers {
			markerChanges[layer.ChangeID] = true
		}
		if !markerChanges[result.Change.ID] {
			t.Fatalf("replacement change %s did not enter propagation trajectory", result.Change.ID)
		}
		if scenario.Namespace == "pay" && !markerChanges["C-01"] {
			t.Fatalf("replacement erased prior C-01 propagation trajectory")
		}
	}

	p := model.CloneProject(sample.Project)
	p.PolicyLimits["pay"] = 0
	_, err := engine.ReplacePolicy(p, engine.ReplaceRequest{
		Namespace: "pay", TargetID: "pol-pay-expired-legacy", AtSeq: 6,
		NewPolicy: sample.Truth.Replacements[0].NewPolicy,
	})
	if xerr.CodeOf(err) != xerr.EPolicyLimitZero {
		t.Fatalf("zero limit code=%s err=%v", xerr.CodeOf(err), err)
	}

	p = model.CloneProject(sample.Project)
	bad := sample.Truth.Replacements[0].NewPolicy
	bad.Priority = 5000
	_, err = engine.ReplacePolicy(p, engine.ReplaceRequest{
		Namespace: "pay", TargetID: "pol-pay-expired-legacy", AtSeq: 6, NewPolicy: bad,
	})
	if xerr.CodeOf(err) != xerr.EPriorityRange {
		t.Fatalf("priority code=%s", xerr.CodeOf(err))
	}

	_, err = engine.ReplacePolicy(model.CloneProject(sample.Project), engine.ReplaceRequest{
		Namespace: "pay", TargetID: "pol-pay-order-allow", AtSeq: 6,
		NewPolicy: sample.Truth.Replacements[0].NewPolicy,
	})
	if xerr.CodeOf(err) != xerr.EReplaceTargetActive {
		t.Fatalf("active target code=%s", xerr.CodeOf(err))
	}
}

func TestCheckpointVersionCrashAndTruncation(t *testing.T) {
	sample := gen.Generate(gen.DefaultSeed)
	versions := map[string]*model.Project{}
	for _, v := range []string{"v1", "v2", "v3"} {
		p, err := schema.Load(sample.Documents[v])
		if err != nil {
			t.Fatal(err)
		}
		versions[v] = p
	}
	v1 := store.CanonicalVersion(versions["v1"])
	if v1 != store.CanonicalVersion(versions["v2"]) || v1 != store.CanonicalVersion(versions["v3"]) {
		t.Fatalf("canonical versions differ: %s %s %s", v1, store.CanonicalVersion(versions["v2"]), store.CanonicalVersion(versions["v3"]))
	}

	report, err := engine.Run(versions["v1"], engine.RunOptions{Workers: 3, BufferPerNS: 64})
	if err != nil {
		t.Fatal(err)
	}
	cp4 := store.BuildCheckpoint(versions["v1"], report, 4)
	if cp4.CommittedSeq != 4 || len(cp4.Workloads) != len(versions["v1"].Workloads) || len(cp4.Queue) == 0 {
		t.Fatalf("unexpected checkpoint: seq=%d workloads=%d queue=%d", cp4.CommittedSeq, len(cp4.Workloads), len(cp4.Queue))
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "cp.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCheckpoint(cp4); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.LoadCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CheckVersion(loaded, v1); err != nil {
		t.Fatal(err)
	}
	if loaded.CommittedSeq != 4 {
		t.Fatalf("loaded seq=%d", loaded.CommittedSeq)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	crashPath := filepath.Join(dir, "crash.db")
	crashStore, err := store.Open(crashPath)
	if err != nil {
		t.Fatal(err)
	}
	cp3 := cp4
	cp3.CommittedSeq = 3
	if err := crashStore.SaveCheckpoint(cp3); err != nil {
		t.Fatal(err)
	}
	err = crashStore.SaveCheckpointWithOptions(cp4, store.SaveOptions{CrashAfterChildren: true})
	if !errors.Is(err, store.ErrSimulatedCrash) {
		t.Fatalf("expected simulated crash, got %v", err)
	}
	_ = crashStore.Close()
	reopened, err := store.Open(crashPath)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := reopened.LoadCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.CommittedSeq != 3 {
		t.Fatalf("torn commit exposed seq=%d, want 3", recovered.CommittedSeq)
	}
	_ = reopened.Close()

	truncPath := filepath.Join(dir, "trunc.db")
	truncStore, err := store.Open(truncPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := truncStore.SaveCheckpoint(cp4); err != nil {
		t.Fatal(err)
	}
	_ = truncStore.Close()
	if err := os.Truncate(truncPath, 64); err != nil {
		t.Fatal(err)
	}
	_, err = store.Open(truncPath)
	if xerr.CodeOf(err) != xerr.ECheckpointTruncated {
		t.Fatalf("truncated code=%s err=%v", xerr.CodeOf(err), err)
	}

	emptyPath := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(emptyPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = store.Open(emptyPath)
	if xerr.CodeOf(err) != xerr.ECheckpointTruncated {
		t.Fatalf("empty checkpoint code=%s err=%v", xerr.CodeOf(err), err)
	}

	badVersionPath := filepath.Join(dir, "version.db")
	badStore, err := store.Open(badVersionPath)
	if err != nil {
		t.Fatal(err)
	}
	badCP := cp4
	badCP.ProjectVersion = "pv-other"
	if err := badStore.SaveCheckpoint(badCP); err != nil {
		t.Fatal(err)
	}
	_ = badStore.Close()
	badRead, err := store.Open(badVersionPath)
	if err != nil {
		t.Fatal(err)
	}
	badLoaded, err := badRead.LoadCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if xerr.CodeOf(store.CheckVersion(badLoaded, v1)) != xerr.ECheckpointVersionMismatch {
		t.Fatal("expected version mismatch")
	}
	_ = badRead.Close()
}

func TestSchedulerBudgetAndBackpressureCodes(t *testing.T) {
	_, err := sched.NewPipeline[int](sched.Config{Workers: 128, BufferPerNS: 64})
	if xerr.CodeOf(err) != xerr.EBudgetWorkerInvalid {
		t.Fatalf("worker code=%s", xerr.CodeOf(err))
	}
	_, err = sched.NewPipeline[int](sched.Config{Workers: 2, BufferPerNS: 99999})
	if xerr.CodeOf(err) != xerr.EBudgetBufferInvalid {
		t.Fatalf("buffer code=%s", xerr.CodeOf(err))
	}
	pipe, err := sched.NewPipeline[int](sched.Config{Workers: 1, BufferPerNS: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := pipe.Submit("pay", 1); err != nil {
		t.Fatal(err)
	}
	if err := pipe.Submit("pay", 2); xerr.CodeOf(err) != xerr.EBackpressure {
		t.Fatalf("backpressure code=%s err=%v", xerr.CodeOf(err), err)
	}

	adm, _ := sched.NewAdmission(sched.Config{Workers: 1, BufferPerNS: 1})
	release, err := adm.TryAdmit("pay")
	if err != nil {
		t.Fatal(err)
	}
	_, err = adm.TryAdmit("pay")
	if xerr.CodeOf(err) != xerr.EWorkerBudgetExhausted {
		t.Fatalf("worker exhausted code=%s", xerr.CodeOf(err))
	}
	release()
}

func TestHTTPWorkbenchAndStableInjectionError(t *testing.T) {
	svc, err := workbench.NewService(gen.DefaultSeed, filepath.Join(t.TempDir(), "http.db"))
	if err != nil {
		t.Fatal(err)
	}
	handler := workbench.NewHandler(svc)

	home := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(home, req)
	if home.Code != http.StatusOK || !bytes.Contains(home.Body.Bytes(), []byte("诊断工作台")) {
		t.Fatalf("home status=%d body=%d", home.Code, home.Body.Len())
	}

	body := []byte(`{"type":"action_conflict"}`)
	rec := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/inject", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error.Code != xerr.EActionConflict {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}

	loadBody := []byte(`{"version":"v3","workers":2,"buffer_per_ns":64}`)
	loadRec := httptest.NewRecorder()
	loadReq := httptest.NewRequest(http.MethodPost, "/api/load", bytes.NewReader(loadBody))
	handler.ServeHTTP(loadRec, loadReq)
	if loadRec.Code != http.StatusOK {
		t.Fatalf("load v3 status=%d body=%s", loadRec.Code, loadRec.Body.String())
	}
}

func TestGateDeniesWithoutCommittedBaseline(t *testing.T) {
	allow := model.Policy{
		ID: "p1", Namespace: "demo", Priority: 100, Order: 1,
		Source:   model.Selector{Namespace: "demo", MatchLabels: map[string]string{"app": "caller"}},
		PortFrom: 8080, PortTo: 8080, Action: model.ActionAllow,
		StartSeq: 1, EndSeq: 99, State: model.StateActive, Revision: 1,
	}
	p := &model.Project{
		ID: "gate-no-baseline", TickMS: 100, DefaultDeny: true,
		Workloads: []model.Workload{
			{ID: "src", Namespace: "demo", ServiceAccount: "src-sa", Labels: map[string]string{"app": "caller"}},
			{ID: "dst", Namespace: "demo", ServiceAccount: "dst-sa", Labels: map[string]string{"app": "dst"}},
		},
		Policies: []model.Policy{allow},
		Timeline: []model.Event{
			{Seq: 1, Order: 5, Change: &model.PolicyChange{
				ID: "c1", Seq: 1, Order: 5, Kind: model.ChangePriority, Namespace: "demo", PolicyID: "p1",
				Before: &allow, After: func() *model.Policy { x := allow; x.Priority = 90; x.Revision = 2; return &x }(),
			}},
			{Seq: 1, Order: 10, Attempt: &model.ConnectionAttempt{
				ID: "a1", Seq: 1, Order: 10, SourceWorkloadID: "src", TargetWorkloadID: "dst", Port: 8080, Protocol: "TCP",
			}},
		},
	}
	r, err := engine.Run(p, engine.RunOptions{Workers: 1, BufferPerNS: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Decisions) != 1 {
		t.Fatalf("decisions=%d", len(r.Decisions))
	}
	d := r.Decisions[0]
	if d.Allow || d.Gate.State != "denied_no_baseline" || d.Gate.Code != xerr.EGateNoBaseline {
		t.Fatalf("expected explicit no-baseline gate deny, got %+v", d)
	}
	if d.Gate.MarkerID == "" || d.Gate.Generation != 1 {
		t.Fatalf("missing marker basis: %+v", d.Gate)
	}
}
