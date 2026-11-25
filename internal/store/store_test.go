package store

import (
	"path/filepath"
	"sync"
	"testing"

	"mocapbench/internal/core"
)

func TestVersionMismatchInvalidatesSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoints.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Initialize("clip", "target", "pv1", "av1"); err != nil {
		t.Fatal(err)
	}
	frame := core.FrameResult{ClipID: "clip", TargetID: "target", Frame: 0, ParamsVersion: "pv1", AlgoVersion: "av1", State: "committed"}
	frame.ContentHash = frame.ComputeHash()
	if err := st.SaveCommittedFrame(frame); err != nil {
		t.Fatal(err)
	}
	frames, lerr := st.LoadCommittedFrames("clip", "target", "pv1", "av1")
	if lerr != nil || len(frames) != 1 {
		t.Fatalf("load before restart: frames=%d err=%v", len(frames), lerr)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	info, ierr := reopened.Initialize("clip", "target", "pv2", "av1")
	if ierr != nil {
		t.Fatal(ierr)
	}
	if !info.Reset {
		t.Fatal("parameter version mismatch must reset snapshot")
	}
	frames, lerr = reopened.LoadCommittedFrames("clip", "target", "pv2", "av1")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(frames) != 0 {
		t.Fatalf("version mismatch must void all checkpoints, found %d", len(frames))
	}
}

func TestConcurrentCheckpointWritesAreSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serial.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Initialize("c", "t", "p", "a"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := core.FrameResult{ClipID: "c", TargetID: "t", Frame: i, ParamsVersion: "p", AlgoVersion: "a", State: "committed"}
			f.ContentHash = f.ComputeHash()
			if e := st.SaveCommittedFrame(f); e != nil {
				t.Errorf("save %d: %v", i, e)
			}
		}()
	}
	wg.Wait()
	count, e := st.Count()
	if e != nil {
		t.Fatal(e)
	}
	if count != 20 {
		t.Fatalf("expected 20 durable frames, got %d", count)
	}
	frames, lerr := st.LoadCommittedFrames("c", "t", "p", "a")
	if lerr != nil {
		t.Fatal(lerr)
	}
	for i, f := range frames {
		if f.Frame != i {
			t.Fatalf("frame order unstable at %d: got %d", i, f.Frame)
		}
	}
}
