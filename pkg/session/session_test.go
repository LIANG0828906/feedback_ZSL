package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"volumelab/pkg/model"
	"volumelab/pkg/pool"
	"volumelab/pkg/samplegen"
	"volumelab/pkg/volume"
)

func fixture(t *testing.T) *volume.Volume {
	t.Helper()
	result, err := samplegen.Generate(samplegen.Options{OutputDir: t.TempDir(), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	_, v, err := volume.LoadFromManifest(result.ManifestPath, "vol_a")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func testExecutor(t *testing.T, workers, pixels int) *pool.Executor {
	t.Helper()
	ex, err := pool.New(workers, pixels)
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

func TestSnapshotRestoreIsPixelAndValueExact(t *testing.T) {
	v := fixture(t)
	snapshotPath := filepath.Join(t.TempDir(), "vol_a.session.json")
	store := NewStore(snapshotPath)
	sess, opened, appErr := Open(context.Background(), v, store, testExecutor(t, 3, 31))
	if appErr != nil {
		t.Fatal(appErr)
	}
	if opened.Recovered {
		t.Fatal("first open unexpectedly recovered a snapshot")
	}
	ctx := context.Background()
	if _, appErr := sess.SetWindow(ctx, model.Window{Width: 800, Level: 100}); appErr != nil {
		t.Fatal(appErr)
	}
	if _, appErr := sess.SetCrosshair(ctx, model.Vec3{20, 30, 27}); appErr != nil {
		t.Fatal(appErr)
	}
	if _, appErr := sess.SetThickness(ctx, model.PlaneAxial, 4.5); appErr != nil {
		t.Fatal(appErr)
	}
	withAnnotation, appErr := sess.AddAutoAnnotation(ctx, "sphere_a", "sphere_diameter")
	if appErr != nil {
		t.Fatal(appErr)
	}
	before := withAnnotation.State
	beforeTriple := withAnnotation.Triple
	beforeChecksum := withAnnotation.Snapshot.Checksum

	_, reopened, appErr := Open(ctx, v, store, testExecutor(t, 1, 17))
	if appErr != nil {
		t.Fatal(appErr)
	}
	if !reopened.Recovered {
		t.Fatalf("expected recovery, reason=%q", reopened.InvalidReason)
	}
	if !reflect.DeepEqual(reopened.State, before) {
		t.Fatalf("state did not restore exactly\nbefore=%+v\nafter=%+v", before, reopened.State)
	}
	for i := range beforeTriple.Images {
		if beforeTriple.Images[i].PixelHash != reopened.Triple.Images[i].PixelHash {
			t.Fatalf("%s image hash changed after recovery", beforeTriple.Images[i].Plane)
		}
		if !reflect.DeepEqual(beforeTriple.Images[i].Pixels, reopened.Triple.Images[i].Pixels) {
			t.Fatalf("%s pixels changed after recovery", beforeTriple.Images[i].Plane)
		}
	}
	if reopened.Snapshot.Checksum != beforeChecksum {
		t.Fatalf("recovery rewrote snapshot: %s != %s", reopened.Snapshot.Checksum, beforeChecksum)
	}
	if len(reopened.State.Annotations) != 1 ||
		reopened.State.Annotations[0].EndpointA != before.Annotations[0].EndpointA ||
		reopened.State.Annotations[0].EndpointB != before.Annotations[0].EndpointB ||
		reopened.State.Annotations[0].WorldLengthMM != before.Annotations[0].WorldLengthMM ||
		reopened.State.Annotations[0].VoxelLength != before.Annotations[0].VoxelLength {
		t.Fatalf("annotation dual readings changed after recovery")
	}
}

func TestCancelDoesNotChangeCommittedSnapshot(t *testing.T) {
	v := fixture(t)
	store := NewStore(filepath.Join(t.TempDir(), "session.json"))
	sess, opened, appErr := Open(context.Background(), v, store, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	before := opened.State
	beforeChecksum := opened.Snapshot.Checksum
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, appErr := sess.SetWindow(ctx, model.Window{Width: 100, Level: 100}); appErr == nil || appErr.Code != "E_CANCEL" {
		t.Fatalf("expected E_CANCEL, got %#v", appErr)
	}
	after := sess.State()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("canceled operation changed in-memory state")
	}
	_, reopened, appErr := Open(context.Background(), v, store, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	if !reflect.DeepEqual(before, reopened.State) {
		t.Fatalf("canceled operation polluted the snapshot")
	}
	if reopened.Snapshot.Checksum != beforeChecksum {
		t.Fatalf("snapshot checksum changed after cancel")
	}
}

func TestVersionMismatchInvalidatesWholeSnapshot(t *testing.T) {
	v := fixture(t)
	path := filepath.Join(t.TempDir(), "session.json")
	store := NewStore(path)
	sess, _, appErr := Open(context.Background(), v, store, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	if _, appErr := sess.AddAutoAnnotation(context.Background(), "tube_a", "tube_axis"); appErr != nil {
		t.Fatal(appErr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope snapshotEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	payloadBytes, _ := base64.StdEncoding.DecodeString(envelope.Payload)
	var payload SnapshotPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatal(err)
	}
	payload.Version = 999
	payloadBytes, _ = json.Marshal(payload)
	envelope.Payload = base64.StdEncoding.EncodeToString(payloadBytes)
	envelope.Checksum = checksum(payloadBytes)
	raw, _ = json.MarshalIndent(envelope, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, reopened, appErr := Open(context.Background(), v, store, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	if reopened.Recovered {
		t.Fatal("version-mismatched snapshot was partially reused")
	}
	if reopened.InvalidReason == "" {
		t.Fatal("expected invalid snapshot reason")
	}
	if len(reopened.State.Annotations) != 0 || reopened.State.Sequence != 1 {
		t.Fatalf("snapshot was not fully rebuilt: %+v", reopened.State)
	}
}

func TestContentHashMismatchInvalidatesSnapshot(t *testing.T) {
	v := fixture(t)
	store := NewStore(filepath.Join(t.TempDir(), "session.json"))
	sess, _, appErr := Open(context.Background(), v, store, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	if _, appErr := sess.AddAutoAnnotation(context.Background(), "sphere_a", "sphere_diameter"); appErr != nil {
		t.Fatal(appErr)
	}
	changed := *v
	changed.ContentHash = "0000000000000000000000000000000000000000000000000000000000000000"
	_, reopened, appErr := Open(context.Background(), &changed, store, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	if reopened.Recovered || len(reopened.State.Annotations) != 0 {
		t.Fatalf("content mismatch did not invalidate snapshot: %+v", reopened)
	}
}

func TestCorruptSnapshotIsReplacedAtomically(t *testing.T) {
	v := fixture(t)
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte(`{"format":"volumelab.session/v1","payload":"AAAA","checksum":"bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	_, opened, appErr := Open(context.Background(), v, store, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	if opened.Recovered || opened.InvalidReason == "" {
		t.Fatalf("corrupt snapshot was accepted: %+v", opened)
	}
	loaded := store.Load()
	if !loaded.Recovered || loaded.Info.Sequence != 1 {
		t.Fatalf("replacement snapshot is invalid: %+v", loaded)
	}
}

func TestParallelAndSerialSessionsProduceSameStateAndCommitChecksum(t *testing.T) {
	v := fixture(t)
	run := func(executor *pool.Executor) (*View, string) {
		store := NewStore(filepath.Join(t.TempDir(), "session.json"))
		sess, opened, appErr := Open(context.Background(), v, store, executor)
		if appErr != nil {
			t.Fatal(appErr)
		}
		view := &opened.View
		steps := []func(context.Context) (*View, *model.AppError){
			func(ctx context.Context) (*View, *model.AppError) {
				return sess.SetCrosshair(ctx, model.Vec3{20, 30, 27})
			},
			func(ctx context.Context) (*View, *model.AppError) {
				return sess.SetWindow(ctx, model.Window{Width: 900, Level: 250})
			},
			func(ctx context.Context) (*View, *model.AppError) {
				return sess.SetThickness(ctx, model.PlaneCoronal, 6)
			},
			func(ctx context.Context) (*View, *model.AppError) {
				return sess.AddAutoAnnotation(ctx, "tube_a", "tube_axis")
			},
		}
		for _, step := range steps {
			next, appErr := step(context.Background())
			if appErr != nil {
				t.Fatal(appErr)
			}
			view = next
		}
		return view, store.Load().Info.Checksum
	}
	serial, serialChecksum := run(testExecutor(t, 1, 29))
	parallel, parallelChecksum := run(testExecutor(t, 3, 29))
	if serialChecksum != parallelChecksum {
		t.Fatalf("commit result differs: %s != %s", serialChecksum, parallelChecksum)
	}
	if !reflect.DeepEqual(serial.State, parallel.State) {
		t.Fatalf("state differs\nserial=%+v\nparallel=%+v", serial.State, parallel.State)
	}
	for i := range serial.Triple.Images {
		if serial.Triple.Images[i].PixelHash != parallel.Triple.Images[i].PixelHash {
			t.Fatalf("%s image differs between serial and parallel", serial.Triple.Images[i].Plane)
		}
	}
}

func TestRecoveredPathContinuesToSameFinalState(t *testing.T) {
	v := fixture(t)
	uninterruptedStore := NewStore(filepath.Join(t.TempDir(), "u.json"))
	uninterrupted, _, appErr := Open(context.Background(), v, uninterruptedStore, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	if _, appErr := uninterrupted.SetCrosshair(context.Background(), model.Vec3{16, 18, 20}); appErr != nil {
		t.Fatal(appErr)
	}
	if _, appErr := uninterrupted.SetThickness(context.Background(), model.PlaneAxial, 1.5); appErr != nil {
		t.Fatal(appErr)
	}
	applyTail := func(s *Session) *View {
		view := &View{}
		steps := []func(context.Context) (*View, *model.AppError){
			func(ctx context.Context) (*View, *model.AppError) {
				return s.SetWindow(ctx, model.Window{Width: 750, Level: 150})
			},
			func(ctx context.Context) (*View, *model.AppError) {
				return s.SetCrosshair(ctx, model.Vec3{18, 26, 22})
			},
			func(ctx context.Context) (*View, *model.AppError) {
				return s.AddAutoAnnotation(ctx, "sphere_a", "sphere_diameter")
			},
		}
		for _, step := range steps {
			next, appErr := step(context.Background())
			if appErr != nil {
				t.Fatal(appErr)
			}
			view = next
		}
		return view
	}
	finalUninterrupted := applyTail(uninterrupted)

	interruptedStore := NewStore(filepath.Join(t.TempDir(), "i.json"))
	interrupted, _, appErr := Open(context.Background(), v, interruptedStore, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	if _, appErr := interrupted.SetCrosshair(context.Background(), model.Vec3{16, 18, 20}); appErr != nil {
		t.Fatal(appErr)
	}
	if _, appErr := interrupted.SetThickness(context.Background(), model.PlaneAxial, 1.5); appErr != nil {
		t.Fatal(appErr)
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, appErr := interrupted.SetWindow(canceledCtx, model.Window{Width: 200, Level: 100}); appErr == nil || appErr.Code != "E_CANCEL" {
		t.Fatalf("expected canceled tail operation, got %#v", appErr)
	}
	interrupted.Close()
	resumed, resumedResult, appErr := Open(context.Background(), v, interruptedStore, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	if !resumedResult.Recovered {
		t.Fatal("snapshot was not recovered")
	}
	finalResumed := applyTail(resumed)
	if !reflect.DeepEqual(finalUninterrupted.State, finalResumed.State) {
		t.Fatalf("resumed final state differs\nuninterrupted=%+v\nresumed=%+v", finalUninterrupted.State, finalResumed.State)
	}
	for i := range finalUninterrupted.Triple.Images {
		if finalUninterrupted.Triple.Images[i].PixelHash != finalResumed.Triple.Images[i].PixelHash {
			t.Fatalf("%s image differs after resume", finalUninterrupted.Triple.Images[i].Plane)
		}
	}
}

func TestForceKillRecoversOnlyCommittedSnapshot(t *testing.T) {
	v := fixture(t)
	result, err := samplegen.Generate(samplegen.Options{OutputDir: t.TempDir(), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(t.TempDir(), "kill-session.json")

	cmd := exec.Command(os.Args[0], "-test.run=^TestForceKillHelper$")
	cmd.Env = append(os.Environ(),
		"VOLUMELAB_KILL_HELPER=1",
		"VOLUMELAB_MANIFEST="+result.ManifestPath,
		"VOLUMELAB_STORE="+storePath,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	readyPath := storePath + ".ready"
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("helper did not report a committed snapshot")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	store := NewStore(storePath)
	_, recovered, appErr := Open(context.Background(), v, store, nil)
	if appErr != nil {
		t.Fatal(appErr)
	}
	if !recovered.Recovered {
		t.Fatalf("force-kill snapshot was not recovered: %+v", recovered)
	}
	if recovered.State.Sequence != 4 || len(recovered.State.Annotations) != 1 {
		t.Fatalf("unexpected committed state after SIGKILL: %+v", recovered.State)
	}
	if recovered.State.Annotations[0].WorldLengthMM != 16 {
		t.Fatalf("annotation was not restored exactly: %+v", recovered.State.Annotations[0])
	}
}

func TestForceKillHelper(t *testing.T) {
	if os.Getenv("VOLUMELAB_KILL_HELPER") != "1" {
		return
	}
	manifestPath := os.Getenv("VOLUMELAB_MANIFEST")
	storePath := os.Getenv("VOLUMELAB_STORE")
	_, v, err := volume.LoadFromManifest(manifestPath, "vol_a")
	if err != nil {
		fmt.Println("HELPER_ERROR", err)
		os.Exit(2)
	}
	sess, _, appErr := Open(context.Background(), v, NewStore(storePath), nil)
	if appErr != nil {
		fmt.Println("HELPER_ERROR", appErr)
		os.Exit(2)
	}
	ops := []func() error{
		func() error {
			_, appErr := sess.SetCrosshair(context.Background(), model.Vec3{20, 30, 27})
			if appErr != nil {
				return appErr
			}
			return nil
		},
		func() error {
			_, appErr := sess.SetWindow(context.Background(), model.Window{Width: 800, Level: 200})
			if appErr != nil {
				return appErr
			}
			return nil
		},
		func() error {
			_, appErr := sess.AddAutoAnnotation(context.Background(), "sphere_a", "sphere_diameter")
			if appErr != nil {
				return appErr
			}
			return nil
		},
	}
	for _, op := range ops {
		if err := op(); err != nil {
			fmt.Println("HELPER_ERROR", err)
			os.Exit(2)
		}
	}
	if err := os.WriteFile(storePath+".ready", []byte(fmt.Sprintf("sequence=%d\n", sess.State().Sequence)), 0o600); err != nil {
		fmt.Println("HELPER_ERROR", err)
		os.Exit(2)
	}
	select {}
}
