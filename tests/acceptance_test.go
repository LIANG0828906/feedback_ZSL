package acceptance

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"

	"tlscheck/internal/bizerr"
	"tlscheck/internal/checkpoint"
	"tlscheck/internal/engine"
	"tlscheck/internal/incremental"
	"tlscheck/internal/model"
	"tlscheck/internal/projectio"
	"tlscheck/internal/runtime"
	"tlscheck/internal/sample"
)

func TestThreeSchemaVersionsLoadEquivalent(t *testing.T) {
	b, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	docs := [][]byte{b.SchemaV1, b.SchemaV2, b.SchemaV3}
	var hashes []string
	var loaded []*model.Project
	for i, data := range docs {
		p, err := projectio.Load(data)
		if err != nil {
			t.Fatalf("schema %d: %v", i+1, err)
		}
		h, err := p.CanonicalHash()
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
		loaded = append(loaded, p)
	}
	if hashes[0] != hashes[1] || hashes[1] != hashes[2] {
		t.Fatalf("canonical hashes differ: %s %s %s", hashes[0][:12], hashes[1][:12], hashes[2][:12])
	}
	for _, p := range loaded {
		if got, want := endpointIDs(p), endpointIDs(&b.Project); !sameStrings(got, want) {
			t.Fatalf("endpoint set mismatch: %v != %v", got, want)
		}
		if got, want := chainIDs(p), chainIDs(&b.Project); !reflect.DeepEqual(got, want) {
			t.Fatalf("chain order mismatch: %v != %v", got, want)
		}
		if got, want := requestIDs(p), requestIDs(&b.Project); !reflect.DeepEqual(got, want) {
			t.Fatalf("request sequence mismatch: %v != %v", got, want)
		}
	}
}

func TestDeterministicJudgmentsAndTruth(t *testing.T) {
	b, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{b.SchemaV1, b.SchemaV2, b.SchemaV3} {
		p, err := projectio.Load(data)
		if err != nil {
			t.Fatal(err)
		}
		r, err := engine.Run(*p)
		if err != nil {
			t.Fatal(err)
		}
		ok, err := incremental.JudgmentsEqual(r, b.Truth)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("schema-derived judgment differs from generator truth")
		}
	}

	byReq := map[string]*engine.RequestResult{}
	for _, rr := range b.Truth.Results {
		byReq[rr.RequestID] = rr
	}
	must := func(id string) *engine.RequestResult {
		rr := byReq[id]
		if rr == nil {
			t.Fatalf("missing result %s", id)
		}
		return rr
	}
	assertAdmit := func(id string, want int64) {
		t.Helper()
		rr := must(id)
		if !rr.HasFinalAdmission || rr.FinalAdmissionAtMs != want {
			t.Fatalf("%s admission=%v/%d, want %d", id, rr.HasFinalAdmission, rr.FinalAdmissionAtMs, want)
		}
	}
	assertAdmit("B-2", 3000)
	assertAdmit("B-3", 5000)
	assertAdmit("G-3", 10000)
	assertAdmit("G-4", 10000)
	assertAdmit("D-1", 4000)

	if rr := must("E-1"); rr.SelectedChainID != "chain-epsilon-v1" || rr.Fallback == nil {
		t.Fatalf("E-1 fallback missing: selected=%s fallback=%+v", rr.SelectedChainID, rr.Fallback)
	}
	if rr := must("E-2"); rr.Status != engine.StatusRejected || rr.GateCategory != engine.GateChainInvalid {
		t.Fatalf("E-2 expected chain rejection, got %s/%s", rr.Status, rr.GateCategory)
	}
	if rr := must("Z-1"); rr.Status != engine.StatusRejected || rr.ChainResults[0].Levels[0].Code != "ISSUER_SUBJECT_MISMATCH" {
		t.Fatalf("Z-1 expected issuer mismatch, got %+v", rr.ChainResults)
	}
	if rr := must("H-3"); rr.BackpressureCode != bizerr.EBufferFull {
		t.Fatalf("H-3 expected buffer backpressure, got %+v", rr)
	}

	var frozenQueue *engine.QueueRecord
	for _, q := range b.Truth.QueueRecords {
		if q.RequestID == "D-1" {
			frozenQueue = q
		}
	}
	if frozenQueue == nil || frozenQueue.PrimaryGate != engine.GateBudgetFrozen ||
		len(frozenQueue.ViolatedConstraints) == 0 || frozenQueue.ViolatedConstraints[0] != engine.ViolationFreeze {
		t.Fatalf("D-1 freeze queue record wrong: %+v", frozenQueue)
	}

	for _, rr := range b.Truth.Results {
		for _, cr := range rr.ChainResults {
			sawFailed := false
			for _, lvl := range cr.Levels {
				if sawFailed && !lvl.Skipped {
					t.Fatalf("chain %s later level passed/judged after failure", cr.ChainID)
				}
				if !lvl.Passed && !lvl.Skipped {
					sawFailed = true
				}
			}
		}
	}
}

func TestParallelOrderAndConcurrencyDoNotChangeJudgment(t *testing.T) {
	b, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	sequential, err := engine.Run(b.Project)
	if err != nil {
		t.Fatal(err)
	}
	reversed := endpointIDs(&b.Project)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	for _, workers := range []int{1, 2, 4, 8} {
		parallel, err := runtime.RunParallel(b.Project, runtime.Config{
			Workers: workers, EndpointOrder: reversed,
		})
		if err != nil {
			t.Fatal(err)
		}
		equal, err := incremental.JudgmentsEqual(sequential, parallel)
		if err != nil {
			t.Fatal(err)
		}
		if !equal {
			t.Fatalf("workers=%d parallel judgment differs", workers)
		}
		if parallel.Diagnostics.PeakWorkers > workers {
			t.Fatalf("peak workers %d exceeds budget %d", parallel.Diagnostics.PeakWorkers, workers)
		}
		if len(parallel.Diagnostics.EndpointElapsedNS) != len(b.Project.Endpoints) {
			t.Fatalf("missing elapsed diagnostics: %+v", parallel.Diagnostics.EndpointElapsedNS)
		}
	}
	pool, err := runtime.NewBoundedPool(1)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := pool.TryAcquire()
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.TryAcquire()
	if bizerr.CodeOf(err) != bizerr.EThreadBudget {
		t.Fatalf("expected E_THREAD_BUDGET, got %v", err)
	}
	lease.Release()
}

func TestCheckpointInterruptResumeAndRecoveryErrors(t *testing.T) {
	b, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, runErr := engine.Run(b.Project, engine.Options{Faults: engine.FaultOptions{
		InterruptRequestIDs: map[string]bool{"A-1": true},
	}})
	if bizerr.CodeOf(runErr) != bizerr.EInterrupt {
		t.Fatalf("expected interrupt, got %v", runErr)
	}
	cp := checkpoint.FromReport(b.Project, interrupted, 1)
	if err := store.Save(b.Project, cp, checkpoint.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store2.LoadForProject(b.Project)
	if err != nil {
		t.Fatal(err)
	}
	initialResults := map[string][]*engine.RequestResult{}
	for _, rr := range loaded.Results {
		initialResults[rr.EndpointID] = append(initialResults[rr.EndpointID], rr)
	}
	recovered, err := engine.Run(b.Project, engine.Options{
		InitialStates: loaded.States, InitialResults: initialResults,
	})
	if err != nil {
		t.Fatal(err)
	}
	full, err := engine.Run(b.Project)
	if err != nil {
		t.Fatal(err)
	}
	equal, err := incremental.OutcomesEqual(recovered, full)
	if err != nil {
		t.Fatal(err)
	}
	if !equal {
		t.Fatalf("recovered judgment differs from full run")
	}
	alpha := recovered.EndpointStates["alpha-ok"]
	if got := len(alpha.Charges); got != 6 {
		t.Fatalf("expected six unique A-1/A-2 budget charges, got %d", got)
	}
	for _, ch := range alpha.Charges {
		if ch.RequestID == "A-1" && ch.ChainID != "chain-alpha" {
			t.Fatalf("A-1 budget leaked to another chain: %+v", ch)
		}
	}

	// A second fault before global_sequence must roll back and preserve old commit.
	badCP := checkpoint.FromReport(b.Project, full, 99)
	err = store2.Save(b.Project, badCP, checkpoint.SaveOptions{FaultBeforeGlobalSequence: true})
	if bizerr.CodeOf(err) != bizerr.EInterrupt {
		t.Fatalf("expected fault interrupt, got %v", err)
	}
	again, err := store2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if again.GlobalSequence == 99 {
		t.Fatalf("uncommitted global sequence became visible")
	}
	if err := store2.Close(); err != nil {
		t.Fatal(err)
	}

	truncPath := filepath.Join(t.TempDir(), "truncated.db")
	if err := writeTruncatedCheckpoint(truncPath); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.Open(truncPath); bizerr.CodeOf(err) != bizerr.ECheckpointTruncated {
		t.Fatalf("expected E_CHECKPOINT_TRUNCATED, got %v", err)
	}

	mismatchDB := filepath.Join(t.TempDir(), "version.db")
	vstore, err := checkpoint.Open(mismatchDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := vstore.Save(b.Project, checkpoint.FromReport(b.Project, full, 1), checkpoint.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := vstore.Close(); err != nil {
		t.Fatal(err)
	}
	goodStore, err := checkpoint.Open(mismatchDB)
	if err != nil {
		t.Fatal(err)
	}
	edited := model.CloneProject(b.Project)
	edited.ProjectID = "replaced-snapshot"
	if _, err := goodStore.LoadForProject(edited); bizerr.CodeOf(err) != bizerr.ESnapshotMismatch {
		t.Fatalf("expected E_SNAPSHOT_MISMATCH, got %v", err)
	}
	if err := goodStore.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", mismatchDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE checkpoint_meta SET value='tls-probe-canonical/v0' WHERE key='canonical_version'`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	vstore2, err := checkpoint.Open(mismatchDB)
	if err != nil {
		t.Fatal(err)
	}
	defer vstore2.Close()
	if _, err := vstore2.LoadForProject(b.Project); bizerr.CodeOf(err) != bizerr.ECheckpointVersion {
		t.Fatalf("expected E_CHECKPOINT_VERSION, got %v", err)
	}
}

func TestChainUpdateFreezeRecovery(t *testing.T) {
	b, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := engine.Run(b.Project, engine.Options{Faults: engine.FaultOptions{
		InterruptUpdateIDs: map[string]bool{"D-U1": true},
	}})
	if bizerr.CodeOf(err) != bizerr.EInterrupt {
		t.Fatal(err)
	}
	if !interrupted.EndpointStates["delta-freeze"].Frozen {
		t.Fatalf("delta should remain frozen before commit")
	}
	db := filepath.Join(t.TempDir(), "freeze.db")
	st, err := checkpoint.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Save(b.Project, checkpoint.FromReport(b.Project, interrupted, 1), checkpoint.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	cp, err := st.LoadForProject(b.Project)
	if err != nil {
		t.Fatal(err)
	}
	results := map[string][]*engine.RequestResult{}
	for _, rr := range cp.Results {
		results[rr.EndpointID] = append(results[rr.EndpointID], rr)
	}
	recovered, err := engine.Run(b.Project, engine.Options{InitialStates: cp.States, InitialResults: results})
	if err != nil {
		t.Fatal(err)
	}
	full, _ := engine.Run(b.Project)
	equal, err := incremental.OutcomesEqual(recovered, full)
	if err != nil || !equal {
		t.Fatalf("freeze recovery mismatch equal=%v err=%v", equal, err)
	}
}

func TestIncrementalEditsMatchFullRecompute(t *testing.T) {
	b, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("validity", func(t *testing.T) {
		mgr, err := incremental.New(b.Project)
		if err != nil {
			t.Fatal(err)
		}
		change, err := mgr.EditCertificateValidity("chain-gamma", 0, 0, 1500, 1000)
		if err != nil {
			t.Fatal(err)
		}
		inc, err := mgr.Refresh(change)
		if err != nil {
			t.Fatal(err)
		}
		full, err := engine.Run(mgr.Project())
		if err != nil {
			t.Fatal(err)
		}
		assertJudgmentsEqual(t, inc, full)
		assertProvenanceSource(t, inc, "G-3", "SRC_CHAIN_VALIDITY:chain-gamma#0")
	})

	t.Run("issuer", func(t *testing.T) {
		mgr, err := incremental.New(b.Project)
		if err != nil {
			t.Fatal(err)
		}
		change, err := mgr.EditIssuer("chain-beta", 1, "CN=changed-root", 1000)
		if err != nil {
			t.Fatal(err)
		}
		inc, err := mgr.Refresh(change)
		if err != nil {
			t.Fatal(err)
		}
		full, _ := engine.Run(mgr.Project())
		assertJudgmentsEqual(t, inc, full)
	})

	t.Run("period dependency", func(t *testing.T) {
		mgr, err := incremental.New(b.Project)
		if err != nil {
			t.Fatal(err)
		}
		change, err := mgr.EditProbePeriod("beta-parallel", 2000, 0)
		if err != nil {
			t.Fatal(err)
		}
		inc, err := mgr.Refresh(change)
		if err != nil {
			t.Fatal(err)
		}
		full, _ := engine.Run(mgr.Project())
		assertJudgmentsEqual(t, inc, full)
		assertProvenanceSource(t, inc, "B-1", "SRC_PROBE_PERIOD:beta-parallel")
	})

	t.Run("append and delete", func(t *testing.T) {
		mgr, err := incremental.New(b.Project)
		if err != nil {
			t.Fatal(err)
		}
		req := model.Request{
			ID: "A-3-APPENDED", EndpointID: "alpha-ok", OfferedProtocol: "TLS1.3",
			OfferedCiphers: []string{"TLS_AES_256_GCM_SHA384"}, DurationMs: 1000,
		}
		appendChange, err := mgr.AppendRequest(3000, req)
		if err != nil {
			t.Fatal(err)
		}
		inc, err := mgr.Refresh(appendChange)
		if err != nil {
			t.Fatal(err)
		}
		full, _ := engine.Run(mgr.Project())
		assertJudgmentsEqual(t, inc, full)
		if resultByID(inc, req.ID) == nil {
			t.Fatalf("appended result missing")
		}
		if rr := resultByID(inc, "A-1"); rr == nil || !rr.Provenance.Reused {
			t.Fatalf("A-1 should reuse terminal prefix result: %+v", rr)
		}

		delChange, err := mgr.DeleteRequest("H-3", 1000)
		if err != nil {
			t.Fatal(err)
		}
		inc2, err := mgr.Refresh(delChange)
		if err != nil {
			t.Fatal(err)
		}
		full2, _ := engine.Run(mgr.Project())
		assertJudgmentsEqual(t, inc2, full2)
		if resultByID(inc2, "H-3") != nil {
			t.Fatalf("deleted request result still present")
		}
	})
}

func TestStableBusinessErrors(t *testing.T) {
	if _, err := projectio.Load([]byte(`{"schema":"nope"}`)); bizerr.CodeOf(err) != bizerr.ESchemaUnsupported {
		t.Fatalf("expected schema unsupported, got %v", err)
	}
	conflict := []byte(`{
		"schema":"` + model.SchemaV1 + `","project_id":"x","tick_ms":100,"thread_budget":1,
		"targets":[{"target":"e","tls_min":"TLS1.2","tls_max":"TLS1.3",
		"probe_every_ticks":1,"probe_period_ms":100,"timeout_ticks":1,
		"max_parallel_handshakes":1,"pending_buffer":1,"validation_budget_permille":10}],
		"certificate_chains":[{"chain_id":"c","certificates":[{"cert_digest":"d","subject_cn":"s","issuer_cn":"s","valid_from_tick":0,"valid_until_tick":10,"check_cost_permille":1}]}],
		"logical_events":[]}`)
	if _, err := projectio.Load(conflict); bizerr.CodeOf(err) != bizerr.ESchemaFieldConflict {
		t.Fatalf("expected field conflict, got %v", err)
	}
	if _, err := projectio.ScaledToBP(1, 31337); bizerr.CodeOf(err) != bizerr.EBudgetScaleUnsupported {
		t.Fatalf("expected budget scale error, got %v", err)
	}
	cycle := model.Chain{ID: "cycle", Certificates: []model.Certificate{
		{Digest: "a", Subject: "A", Issuer: "B"},
		{Digest: "b", Subject: "B", Issuer: "A"},
	}}
	if err := projectio.ValidateChainStructure(cycle); bizerr.CodeOf(err) != bizerr.EChainCycle {
		t.Fatalf("expected chain cycle, got %v", err)
	}
}

func writeTruncatedCheckpoint(path string) error {
	st, err := checkpoint.Open(path)
	if err != nil {
		return err
	}
	b, _ := sample.Build(sample.DefaultSeed)
	if err := st.Save(b.Project, checkpoint.FromReport(b.Project, b.Truth, 1), checkpoint.SaveOptions{}); err != nil {
		return err
	}
	if err := st.Close(); err != nil {
		return err
	}
	return overwriteTruncate(path, 64)
}

func overwriteTruncate(path string, size int64) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if int64(len(data)) < size {
		size = int64(len(data)) / 2
	}
	return os.WriteFile(path, data[:size], 0o644)
}

func endpointIDs(p *model.Project) []string {
	out := make([]string, 0, len(p.Endpoints))
	for _, ep := range p.Endpoints {
		out = append(out, ep.ID)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sortStrings(ac)
	sortStrings(bc)
	return reflect.DeepEqual(ac, bc)
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

func chainIDs(p *model.Project) []string {
	out := make([]string, 0, len(p.Chains))
	for _, ch := range p.Chains {
		out = append(out, ch.ID)
	}
	return out
}

func requestIDs(p *model.Project) []string {
	var out []string
	for _, ev := range p.Events {
		if ev.Request != nil {
			out = append(out, ev.Request.ID)
		}
	}
	return out
}

func resultByID(r *engine.Report, id string) *engine.RequestResult {
	for _, rr := range r.Results {
		if rr.RequestID == id {
			return rr
		}
	}
	return nil
}

func assertJudgmentsEqual(t *testing.T, a, b *engine.Report) {
	t.Helper()
	equal, err := incremental.JudgmentsEqual(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if equal {
		return
	}
	ja, _ := incremental.JudgmentJSON(a)
	jb, _ := incremental.JudgmentJSON(b)
	t.Fatalf("incremental judgment differs\ninc=%s\nfull=%s", compact(ja), compact(jb))
}

func assertProvenanceSource(t *testing.T, r *engine.Report, requestID, source string) {
	t.Helper()
	rr := resultByID(r, requestID)
	if rr == nil {
		t.Fatalf("missing %s", requestID)
	}
	for _, s := range rr.Provenance.InvalidationSource {
		if s == source {
			return
		}
	}
	t.Fatalf("%s missing source %s: %+v", requestID, source, rr.Provenance)
}

func compact(b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	out, _ := json.Marshal(v)
	s := string(out)
	if len(s) > 4000 {
		return s[:4000] + "..."
	}
	return s
}
