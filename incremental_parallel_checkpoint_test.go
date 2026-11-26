package leasekit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIncrementalWindowEditMatchesFullRecompute(t *testing.T) {
	p := GenerateProject(FixedSeed)
	session, err := NewIncrementalSession(p)
	if err != nil {
		t.Fatal(err)
	}
	inc, err := session.EditRenewWindow("lock-order", 1600)
	if err != nil {
		t.Fatal(err)
	}
	full, err := session.FullRecompute()
	if err != nil {
		t.Fatal(err)
	}
	if string(inc.Report.CanonicalJSON()) != string(full.CanonicalJSON()) {
		t.Fatalf("incremental window result differs from full recompute\ninc=%s\nfull=%s", inc.Report.CanonicalJSON(), full.CanonicalJSON())
	}
	if got := inc.Report.ResultBySeq(10); got.Code != CodeOK {
		t.Fatalf("edited wider window should accept seq10, got %s", got.Code)
	}
	if got := inc.Report.ResultBySeq(22); got.Source.AtMS != 4500 {
		t.Fatalf("downstream acquire should use edited chain expiry 4500, got %+v", got.Source)
	}
	if !containsInt(inc.RecomputedSeqs, 10) || !containsInt(inc.CacheHitSeqs, 2) {
		t.Fatalf("dependency marking wrong recomputed=%v cached=%v", inc.RecomputedSeqs, inc.CacheHitSeqs)
	}
	if len(inc.CachedResources) != 4 {
		t.Fatalf("expected four cached resources, got %v", inc.CachedResources)
	}
	for seq, source := range inc.SourcesBySeq {
		if source.Event == "" && source.Reason == "" {
			t.Fatalf("seq=%d missing invalidation source", seq)
		}
	}
}

func TestIncrementalExpiryAndResultEditsMatchFull(t *testing.T) {
	p := GenerateProject(FixedSeed)

	session, err := NewIncrementalSession(p)
	if err != nil {
		t.Fatal(err)
	}
	inc, err := session.EditHolderExpiry("holder-order-a", 25, 5500, 1)
	if err != nil {
		t.Fatal(err)
	}
	full, err := session.FullRecompute()
	if err != nil {
		t.Fatal(err)
	}
	if string(inc.Report.CanonicalJSON()) != string(full.CanonicalJSON()) {
		t.Fatal("expiry edit incremental result differs from full recompute")
	}
	if !containsInt(inc.CacheHitSeqs, 2) {
		t.Fatal("unrelated seq should be cache hit")
	}

	session2, err := NewIncrementalSession(p)
	if err != nil {
		t.Fatal(err)
	}
	inc2, err := session2.EditRenewalResult(10, ResultOverride{Granted: true, ExpiresAtMS: 4500, RenewCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	full2, err := session2.FullRecompute()
	if err != nil {
		t.Fatal(err)
	}
	if string(inc2.Report.CanonicalJSON()) != string(full2.CanonicalJSON()) {
		t.Fatal("renewal result edit incremental result differs from full recompute")
	}
	if got := inc2.Report.ResultBySeq(12); got.Code != CodeRenewTooEarly {
		t.Fatalf("edited seq10 to success should make seq12 early against expiry4500, got %s", got.Code)
	}
}

func TestParallelSchedulingAndBudgetsAreDeterministic(t *testing.T) {
	p := GenerateProject(FixedSeed)
	truth, err := GenerateTruth(p)
	if err != nil {
		t.Fatal(err)
	}
	configs := []ParallelOptions{
		{Workers: 1, AttemptBudgetPerResource: 8, BatchSize: 1},
		{Workers: 2, AttemptBudgetPerResource: 8, BatchSize: 2, ReverseSubmission: true},
		{Workers: 4, AttemptBudgetPerResource: 8, BatchSize: 3},
		{Workers: 8, AttemptBudgetPerResource: 8},
	}
	var first []byte
	for i, cfg := range configs {
		out, err := RunParallel(p, cfg)
		if err != nil {
			t.Fatalf("config %d: %v", i, err)
		}
		body := out.Report.CanonicalJSON()
		if first == nil {
			first = body
		} else if string(first) != string(body) {
			t.Fatalf("config %d produced nondeterministic report", i)
		}
		if string(body) != string(truth.Report.CanonicalJSON()) {
			t.Fatalf("parallel config %d differs from serial truth", i)
		}
		if len(out.Observations) != len(p.Resources) {
			t.Fatalf("observations=%d resources=%d", len(out.Observations), len(p.Resources))
		}
		for _, obs := range out.Observations {
			if obs.ResourceID == "" || obs.OperationCount == 0 {
				t.Fatalf("bad observation %+v", obs)
			}
		}
	}

	limited, err := RunParallel(p, ParallelOptions{Workers: 4, AttemptBudgetPerResource: 1})
	if err != nil {
		t.Fatal(err)
	}
	exhausted := 0
	for _, r := range limited.Report.Results {
		if r.Code == CodeAttemptsExhausted {
			exhausted++
		}
	}
	if exhausted == 0 {
		t.Fatal("expected deterministic attempt-budget rejections")
	}
	limitedAgain, err := RunParallel(p, ParallelOptions{Workers: 2, AttemptBudgetPerResource: 1, ReverseSubmission: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(limited.Report.CanonicalJSON()) != string(limitedAgain.Report.CanonicalJSON()) {
		t.Fatal("limited budget result changed with schedule")
	}

	for _, bad := range []ParallelOptions{{Workers: -1}, {Workers: MaxWorkerBudget + 1}, {AttemptBudgetPerResource: -2}, {AttemptBudgetPerResource: MaxAttemptBudget + 1}} {
		if _, err := RunParallel(p, bad); err == nil {
			t.Fatalf("expected budget error for %+v", bad)
		} else if biz, ok := AsError(err); !ok || !strings.HasPrefix(biz.Code, "E_") {
			t.Fatalf("budget error lacks stable code: %v", err)
		}
	}
}

func TestFileCheckpointOrderTruncationAndVersion(t *testing.T) {
	p := GenerateProject(FixedSeed)
	dir := t.TempDir()
	store := NewFileCheckpointStore(dir)

	cp12, err := BuildCheckpoint(p, 12)
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitCheckpoint(store, cp12); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CommittedSeq != 12 {
		t.Fatalf("committed seq=%d", loaded.CommittedSeq)
	}

	cp20, err := BuildCheckpoint(p, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteHolderStates(*cp20); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadCheckpoint(); err == nil {
		t.Fatal("crash-between-writes should be detected")
	} else if biz, ok := AsError(err); !ok || biz.Code != CodeCheckpointIncomplete {
		t.Fatalf("expected incomplete code, got %v", err)
	}
	rec, err := RecoverOrRecompute(p, store)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Recomputed || rec.ReasonCode != CodeCheckpointIncomplete {
		t.Fatalf("recovery=%+v", rec)
	}
	if err := rec.Repair(p, store); err != nil {
		t.Fatal(err)
	}

	truncDir := t.TempDir()
	truncStore := NewFileCheckpointStore(truncDir)
	if err := os.WriteFile(filepath.Join(truncDir, "commit-seq.json"), []byte(`{"committedSeq":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := truncStore.LoadCheckpoint(); err == nil {
		t.Fatal("expected truncation error")
	} else if biz, ok := AsError(err); !ok || biz.Code != CodeCheckpointTruncated {
		t.Fatalf("expected E_CHECKPOINT_TRUNCATED, got %v", err)
	}

	badVersionDir := t.TempDir()
	badStore := NewFileCheckpointStore(badVersionDir)
	bad := *cp12
	bad.SchemaVersion = "lease-project/v0-wrong"
	if err := badStore.WriteHolderStates(bad); err != nil {
		t.Fatal(err)
	}
	if err := badStore.WriteCommittedSeq(bad); err != nil {
		t.Fatal(err)
	}
	if _, err := badStore.LoadCheckpoint(); err == nil {
		t.Fatal("expected version mismatch")
	} else if biz, ok := AsError(err); !ok || biz.Code != CodeSchemaVersionMismatch {
		t.Fatalf("expected E_SCHEMA_VERSION_MISMATCH, got %v", err)
	}
}

func TestSQLiteCheckpointRecovery(t *testing.T) {
	p := GenerateProject(FixedSeed)
	path := filepath.Join(t.TempDir(), "lease-checkpoint.db")
	store := NewSQLiteCheckpointStore(path)
	cp, err := BuildCheckpoint(p, 12)
	if err != nil {
		t.Fatal(err)
	}
	if err := CommitCheckpoint(store, cp); err != nil {
		if biz, ok := AsError(err); ok && biz.Code == "E_SQLITE_CGO_DISABLED" {
			t.Skip("cgo/system SQLite unavailable")
		}
		t.Fatal(err)
	}
	header := make([]byte, 16)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(header); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if string(header[:15]) != "SQLite format 3" {
		t.Fatalf("not a real sqlite database header: %q", header)
	}
	loaded, err := store.LoadCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CommittedSeq != 12 || len(loaded.Holders) != len(p.Holders) {
		t.Fatalf("bad sqlite checkpoint: %+v", loaded)
	}

	cp20, _ := BuildCheckpoint(p, 20)
	if err := store.WriteHolderStates(*cp20); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadCheckpoint(); err == nil {
		t.Fatal("sqlite staged future holder state must not be accepted")
	}

	db, err := openSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.exec("UPDATE checkpoint_meta SET value='lease-project/v0-wrong' WHERE key='schema_version'"); err != nil {
		t.Fatal(err)
	}
	db.close()
	if _, err := store.LoadCheckpoint(); err == nil {
		t.Fatal("expected sqlite version mismatch")
	} else if biz, ok := AsError(err); !ok || biz.Code != CodeSchemaVersionMismatch {
		t.Fatalf("wrong sqlite version error: %v", err)
	}

	truncPath := filepath.Join(t.TempDir(), "truncated.db")
	truncStore := NewSQLiteCheckpointStore(truncPath)
	if err := CommitCheckpoint(truncStore, cp); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(truncPath, 64); err != nil {
		t.Fatal(err)
	}
	if _, err := truncStore.LoadCheckpoint(); err == nil {
		t.Fatal("expected truncated sqlite error")
	} else if biz, ok := AsError(err); !ok || biz.Code != CodeCheckpointTruncated {
		t.Fatalf("expected sqlite truncation code, got %v", err)
	}
}

func containsInt(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
