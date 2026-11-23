package checkpoint

import (
	"errors"
	"os"
	"testing"

	"loudnessworkbench/apperr"
	"loudnessworkbench/dsp"
)

func TestSnapshotRoundTripAndHashValidation(t *testing.T) {
	store := NewStore(t.TempDir(), "project/with unsafe name")
	snapshot := Snapshot{
		AlgorithmVersion:    AlgorithmVersion,
		ProjectID:           "program",
		SourceVersion:       "v1",
		ParametersHash:      "params",
		TargetLUFS:          -23,
		ProgramLoudnessLUFS: -24,
		GlobalGainDB:        1,
		Records: []Record{{
			Index: 0, SegmentID: "seg", State: "committed",
			SourceMeasurement: dsp.Measurement{IntegratedLUFS: -20, FramePowers: []float64{1}},
			OutputFrames:      100, OutputChannels: 2,
		}},
	}
	if err := store.Save(snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Records) != 1 || loaded.SnapshotHash == "" {
		t.Fatalf("unexpected snapshot: %+v", loaded)
	}
	if err := store.Invalidate(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("got %v, want ErrNoSnapshot", err)
	}
}

func TestCorruptCheckpointIsRejected(t *testing.T) {
	store := NewStore(t.TempDir(), "program")
	if err := store.Save(Snapshot{ProjectID: "program"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := store.Load()
	if !apperr.IsCode(err, apperr.CodeCheckpoint) {
		t.Fatalf("got %v, want E_CHECKPOINT", err)
	}
}
