package sqlstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	acoustic "levelacoustic"
	"levelacoustic/internal/samplegen"
)

var errCancelAfterCommit = errors.New("cancel after checkpoint")

func TestKilledProcessHelper(t *testing.T) {
	if os.Getenv("ACOUSTIC_KILL_CHILD") != "1" {
		return
	}
	databasePath := os.Getenv("ACOUSTIC_KILL_DB")
	project, err := samplegen.Sample(samplegen.DefaultSeed)
	if err != nil {
		os.Exit(2)
	}
	store, err := Open(databasePath)
	if err != nil {
		os.Exit(3)
	}
	session, err := acoustic.NewSession(project, acoustic.SessionOptions{
		Store: store,
		AfterCommit: func(roomID, committed int) error {
			if committed == 1 {
				os.Exit(0)
			}
			return nil
		},
	})
	if err != nil {
		os.Exit(4)
	}
	if _, err := session.Run(context.Background()); err != nil {
		os.Exit(5)
	}
	os.Exit(5)
}

func TestForcedProcessTerminationLeavesOnlyPersistedPrefix(t *testing.T) {
	project, err := samplegen.Sample(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(t.TempDir(), "killed.db")
	command := exec.Command(os.Args[0], "-test.run=TestKilledProcessHelper")
	command.Env = append(os.Environ(), "ACOUSTIC_KILL_CHILD=1", "ACOUSTIC_KILL_DB="+databasePath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("killed child failed: %v\n%s", err, output)
	}
	store, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fingerprint, err := acoustic.ProjectFingerprint(project)
	if err != nil {
		t.Fatal(err)
	}
	load, err := store.Load(context.Background(), fingerprint, project.ParamsVersion, project.AlgorithmVersion)
	if err != nil {
		t.Fatal(err)
	}
	if load.Invalidated || len(load.Snapshots) != 1 || load.Snapshots[0].RoomID != 1 || load.Snapshots[0].CommitSequence != 1 {
		t.Fatalf("unexpected recovered suffix: %+v", load)
	}
	digest, err := acoustic.SnapshotDigest(load.Snapshots[0])
	if err != nil {
		t.Fatal(err)
	}
	if digest != load.Snapshots[0].ContentHash {
		t.Fatal("recovered checkpoint hash is not consistent")
	}
}

func TestCancellationResumeMatchesFullRun(t *testing.T) {
	project, err := samplegen.Sample(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	resumeDB := filepath.Join(directory, "resume.db")
	fullDB := filepath.Join(directory, "full.db")
	workerOptions := acoustic.WorkerOptions{MaxWorkers: 3, PerWorkerBuffer: 2}

	store, err := Open(resumeDB)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := acoustic.NewSession(project, acoustic.SessionOptions{
		Store:         store,
		WorkerOptions: workerOptions,
		AfterCommit: func(roomID, committed int) error {
			if committed == 1 {
				return errCancelAfterCommit
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	partial, err := preview.Run(context.Background())
	if !errors.Is(err, errCancelAfterCommit) {
		t.Fatalf("expected cancellation after first commit, got %v", err)
	}
	if len(partial.CommitSequence) != 1 || partial.CommitSequence[0] != 1 {
		t.Fatalf("unexpected partial commit sequence: %v", partial.CommitSequence)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(resumeDB)
	if err != nil {
		t.Fatal(err)
	}
	resumedSession, err := acoustic.NewSession(project, acoustic.SessionOptions{
		Store:         reopened,
		WorkerOptions: workerOptions,
	})
	if err != nil {
		t.Fatal(err)
	}
	restore, err := resumedSession.Restore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(restore.LoadedRooms) != 1 || restore.LoadedRooms[0] != 1 || restore.Invalidated {
		t.Fatalf("unexpected restore report: %+v", restore)
	}
	resumed, err := resumedSession.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	fullStore, err := Open(fullDB)
	if err != nil {
		t.Fatal(err)
	}
	fullSession, err := acoustic.NewSession(project, acoustic.SessionOptions{
		Store:         fullStore,
		WorkerOptions: workerOptions,
	})
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullSession.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := fullStore.Close(); err != nil {
		t.Fatal(err)
	}

	equal, differences := acoustic.CompareRunResults(full, resumed)
	if !equal {
		t.Fatalf("resume differs from full recompute: %+v", differences)
	}
	if len(resumed.CommitSequence) != 3 {
		t.Fatalf("unexpected resumed commits: %v", resumed.CommitSequence)
	}
	truth := acoustic.CompareTruth(project, resumed.ListenerSolutions)
	if !truth.Passed {
		t.Fatalf("resumed result does not match truth: %+v", truth.Mismatches)
	}
}

func TestContentHashMismatchVoidsEntireSnapshot(t *testing.T) {
	project, err := samplegen.Sample(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tamper.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	session, err := acoustic.NewSession(project, acoustic.SessionOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Snapshots) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(result.Snapshots))
	}
	if _, err := store.db.Exec(`UPDATE room_checkpoints SET content_hash='tampered' WHERE room_id=2`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := acoustic.ProjectFingerprint(project)
	if err != nil {
		t.Fatal(err)
	}
	load, err := reopened.Load(context.Background(), fingerprint, project.ParamsVersion, project.AlgorithmVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !load.Invalidated || len(load.Snapshots) != 0 {
		t.Fatalf("tampered snapshot was not voided: %+v", load)
	}
	var count int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM room_checkpoints`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("checkpoint rows remain after invalidation: %d", count)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVersionMismatchVoidsEntireSnapshot(t *testing.T) {
	project, err := samplegen.Sample(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "version.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	session, err := acoustic.NewSession(project, acoustic.SessionOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := acoustic.ProjectFingerprint(project)
	if err != nil {
		t.Fatal(err)
	}
	load, err := store.Load(context.Background(), fingerprint, project.ParamsVersion, "different-algorithm")
	if err != nil {
		t.Fatal(err)
	}
	if !load.Invalidated || len(load.Snapshots) != 0 {
		t.Fatalf("version mismatch was not voided: %+v", load)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM room_checkpoints`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rows remain after version invalidation: %d", count)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
