package qos

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCheckpointResumeMatchesFullRun(t *testing.T) {
	p := Scenario(42)
	full, err := ExecuteTrace(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "resume.db")
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	prefix := make([]StepResult, 0, 7)
	for i := 0; i < 7; i++ {
		st, err := eng.Step()
		if err != nil {
			t.Fatal(err)
		}
		prefix = append(prefix, st)
		if err := store.Save(context.Background(), p, eng.Snapshot()); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snap, err := store.Load(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if snap.NextStep != 7 || snap.NextMS != 70 {
		t.Fatalf("unexpected checkpoint clock %d/%d", snap.NextStep, snap.NextMS)
	}
	seenDelays := map[string]bool{}
	for _, d := range snap.Delays {
		if seenDelays[d.PacketID] {
			t.Fatalf("delay queue duplicate in checkpoint: %s", d.PacketID)
		}
		seenDelays[d.PacketID] = true
	}
	resumed, status, err := ResumeEngine(context.Background(), store, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(status, "resume:") {
		t.Fatalf("status=%s", status)
	}
	rest, err := resumed.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	combined := append(append([]StepResult(nil), prefix...), rest.Steps...)
	if !stepsEquivalent(full.Steps, combined) {
		t.Fatal("resumed steps differ from full execution")
	}
	if !reflect.DeepEqual(full.Result.Emissions, rest.Emissions) ||
		!reflect.DeepEqual(full.Result.Drops, rest.Drops) ||
		!reflect.DeepEqual(full.Result.Expired, rest.Expired) ||
		!reflect.DeepEqual(full.Result.FinalTokens, rest.FinalTokens) {
		t.Fatal("resumed cumulative result differs from full execution")
	}
}

func TestCheckpointVersionMismatchRestartsFromZero(t *testing.T) {
	p := Scenario(42)
	path := filepath.Join(t.TempDir(), "version.db")
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := NewEngine(p)
	if err := eng.StepUntil(3); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), p, eng.Snapshot()); err != nil {
		t.Fatal(err)
	}
	other := CloneProject(p)
	other.Version += "-other"
	_, err = store.Load(context.Background(), other)
	if CodeOf(err) != ECheckpointVersion {
		t.Fatalf("load code=%s err=%v", CodeOf(err), err)
	}
	fresh, status, err := ResumeEngine(context.Background(), store, other)
	if CodeOf(err) != ECheckpointVersion {
		t.Fatalf("resume code=%s err=%v", CodeOf(err), err)
	}
	if fresh == nil || fresh.NextStep() != 0 || !strings.HasPrefix(status, "full-restart:") {
		t.Fatalf("version mismatch did not restart from zero: step=%d status=%s", fresh.NextStep(), status)
	}
	store.Close()
}

func TestCheckpointTruncatedFile(t *testing.T) {
	p := Scenario(42)
	path := filepath.Join(t.TempDir(), "truncated.db")
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := NewEngine(p)
	if err := eng.StepUntil(2); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), p, eng.Snapshot()); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if err := os.Truncate(path, 80); err != nil {
		t.Fatal(err)
	}
	_, err = OpenCheckpoint(path)
	if CodeOf(err) != ECheckpointTruncated {
		t.Fatalf("truncated checkpoint code=%s err=%v", CodeOf(err), err)
	}
}

func TestCheckpointSIGKILLThenResume(t *testing.T) {
	if os.Getenv("QOS_CHECKPOINT_KILL_HELPER") == "1" {
		checkpointKillHelper()
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "kill.db")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestCheckpointSIGKILLThenResume")
	cmd.Env = append(os.Environ(), "QOS_CHECKPOINT_KILL_HELPER=1", "QOS_CHECKPOINT_KILL_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	ready := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "READY") {
			ready = true
			break
		}
	}
	if !ready {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper did not reach a committed checkpoint: %s", stderr.String())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if ctx.Err() != nil {
		t.Fatal("SIGKILL helper timed out")
	}

	p := Scenario(42)
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatalf("open killed checkpoint: %v stderr=%s", err, stderr.String())
	}
	defer store.Close()
	snap, err := store.Load(context.Background(), p)
	if err != nil {
		t.Fatalf("load killed checkpoint: %v", err)
	}
	if snap.NextStep < 5 || snap.NextMS != int64(snap.NextStep)*p.TickMS {
		t.Fatalf("killed checkpoint is not at a complete point: %#v", snap)
	}
	resumed, _, err := ResumeEngine(context.Background(), store, p)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := resumed.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	full, err := ExecuteTrace(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full.Result.Emissions, rest.Emissions) ||
		!reflect.DeepEqual(full.Result.Drops, rest.Drops) ||
		!reflect.DeepEqual(full.Result.FinalTokens, rest.FinalTokens) {
		t.Fatal("result after SIGKILL resume differs from full run")
	}
}

func checkpointKillHelper() {
	path := os.Getenv("QOS_CHECKPOINT_KILL_PATH")
	p := Scenario(42)
	store, err := OpenCheckpoint(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	eng, err := NewEngine(p)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for i := 0; i < 6; i++ {
		if _, err := eng.Step(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := store.Save(context.Background(), p, eng.Snapshot()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	fmt.Printf("READY next_step=%d\n", eng.NextStep())
	os.Stdout.Sync()
	select {}
}
