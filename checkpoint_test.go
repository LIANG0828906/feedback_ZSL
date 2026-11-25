package plancache

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func commitFirstRequests(t *testing.T, db string, count int) (*Project, *Checkpoint, *Engine) {
	t.Helper()
	p := GenerateProject(DefaultSeed)
	cp, err := OpenCheckpoint(db, p.EngineeringVersion)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count && i < len(p.Requests); i++ {
		req := p.Requests[i]
		report, err := engine.ProcessRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := cp.CheckpointRequest(req, *report, engine.ActiveEntries()); err != nil {
			t.Fatal(err)
		}
	}
	return p, cp, engine
}

func TestCheckpointCommittedRecovery(t *testing.T) {
	db := filepath.Join(t.TempDir(), "checkpoint.db")
	p, cp, _ := commitFirstRequests(t, db, 3)
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, rec, rebuilt, err := RecoverOrRebuild(db, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if rebuilt {
		t.Fatal("matching checkpoint should not rebuild")
	}
	if rec.LastSeq != 3 || len(rec.RequestSeqs) != 3 {
		t.Fatalf("unexpected recovery cursor/requests: %+v", rec)
	}
	if len(rec.Entries) == 0 {
		t.Fatal("expected committed cache entries")
	}
}

func TestCheckpointVersionMismatchRequiresFullRebuild(t *testing.T) {
	db := filepath.Join(t.TempDir(), "version.db")
	_, cp, _ := commitFirstRequests(t, db, 2)
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}

	wrong := GenerateProject(DefaultSeed)
	wrong.EngineeringVersion = "ENG-DIFFERENT"
	_, err := OpenCheckpoint(db, wrong.EngineeringVersion)
	assertCode(t, err, CodeCheckpointVersion)

	incoming := GenerateProject(DefaultSeed)
	incoming.EngineeringVersion = "ENG-DIFFERENT"
	cp2, rec, rebuilt, err := RecoverOrRebuild(db, incoming)
	if err != nil {
		t.Fatal(err)
	}
	defer cp2.Close()
	if !rebuilt {
		t.Fatal("version mismatch should trigger full rebuild")
	}
	if rec.LastSeq != 0 || len(rec.Entries) != 0 || len(rec.RequestSeqs) != 0 {
		t.Fatalf("mismatched checkpoint must not partially reuse old state: %+v", rec)
	}
}

func TestTruncatedCheckpointCode(t *testing.T) {
	db := filepath.Join(t.TempDir(), "truncated.db")
	_, cp, _ := commitFirstRequests(t, db, 2)
	if _, err := cp.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := TruncateCheckpointForInjection(db); err != nil {
		t.Fatal(err)
	}
	p := GenerateProject(DefaultSeed)
	_, err := OpenCheckpoint(db, p.EngineeringVersion)
	assertCode(t, err, CodeCheckpointTruncated)
}

func TestZeroByteCheckpointCode(t *testing.T) {
	db := filepath.Join(t.TempDir(), "zero.db")
	if err := os.WriteFile(db, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	p := GenerateProject(DefaultSeed)
	_, err := OpenCheckpoint(db, p.EngineeringVersion)
	assertCode(t, err, CodeCheckpointTruncated)
}

func TestCheckpointInvalidationProgressIsCommittedInOrder(t *testing.T) {
	db := filepath.Join(t.TempDir(), "progress.db")
	p := GenerateProject(DefaultSeed)
	cp, err := OpenCheckpoint(db, p.EngineeringVersion)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	req := p.Requests[0]
	report, err := engine.ProcessRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	committedProgress := []InvalidationProgress{{EventID: "inv-001", AppliedItems: 3, MaxGeneration: 2, Status: "APPLIED"}}
	if err := cp.SavePendingEntryState(req.Seq, engine.ActiveEntries(), committedProgress); err != nil {
		t.Fatal(err)
	}
	if err := cp.CommitPendingRequest(req, *report); err != nil {
		t.Fatal(err)
	}
	unfinished := []InvalidationProgress{{EventID: "inv-unfinished", AppliedItems: 1, MaxGeneration: 1, Status: "PENDING"}}
	if err := cp.SavePendingEntryState(99, nil, unfinished); err != nil {
		t.Fatal(err)
	}
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, rec, _, err := RecoverOrRebuild(db, p)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(rec.Progress) != 1 || rec.Progress[0].EventID != "inv-001" {
		t.Fatalf("only committed progress should be readable, got %+v", rec.Progress)
	}
}

func TestCheckpointForceKillRecovery(t *testing.T) {
	if os.Getenv("PLANCACHE_CRASH_HELPER") == "1" {
		runCrashHelper()
		return
	}
	db := filepath.Join(t.TempDir(), "force-kill.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCheckpointForceKillRecovery$")
	cmd.Env = append(os.Environ(), "PLANCACHE_CRASH_HELPER=1", "PLANCACHE_CRASH_DB="+db)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		t.Fatalf("crash helper should have been SIGKILLed; output=%s", out.String())
	}

	p := GenerateProject(DefaultSeed)
	cp, rec, rebuilt, err := RecoverOrRebuild(db, p)
	if err != nil {
		t.Fatalf("recovery after kill: %v\nhelper output: %s", err, out.String())
	}
	defer cp.Close()
	if rebuilt {
		t.Fatal("engineering version matches; expected recovery, not rebuild")
	}
	if rec.LastSeq != 1 {
		t.Fatalf("only seq1 should be committed, got recovery %+v", rec)
	}
	if len(rec.RequestSeqs) != 1 || rec.RequestSeqs[0] != 1 {
		t.Fatalf("unexpected committed request seqs: %+v", rec.RequestSeqs)
	}
	if rec.DiscardedPending == 0 {
		t.Fatal("expected unfinished seq2 pending state to be discarded")
	}
	for _, seq := range rec.RequestSeqs {
		if seq == 2 {
			t.Fatal("unfinished request seq2 became readable")
		}
	}
}

func runCrashHelper() {
	db := os.Getenv("PLANCACHE_CRASH_DB")
	p := GenerateProject(DefaultSeed)
	cp, err := OpenCheckpoint(db, p.EngineeringVersion)
	if err != nil {
		fmtFatal(err)
	}
	engine, err := NewEngine(p)
	if err != nil {
		fmtFatal(err)
	}
	req1 := p.Requests[0]
	rep1, err := engine.ProcessRequest(req1)
	if err != nil {
		fmtFatal(err)
	}
	if err := cp.CheckpointRequest(req1, *rep1, engine.ActiveEntries()); err != nil {
		fmtFatal(err)
	}

	req2 := p.Requests[1]
	if _, err := engine.ProcessRequest(req2); err != nil {
		fmtFatal(err)
	}
	if err := cp.SavePendingEntryState(req2.Seq, engine.ActiveEntries(), nil); err != nil {
		fmtFatal(err)
	}
	killSelf()
	select {}
}

func fmtFatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func killSelf() {
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
}
