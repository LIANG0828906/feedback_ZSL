package checkpoint_test

import (
	"os"
	"path/filepath"
	"testing"

	"dialogueaudio/audioerr"
	"dialogueaudio/checkpoint"
	"dialogueaudio/engine"
	"dialogueaudio/generator"
)

func fixture(t *testing.T) (*generator.SampleSet, checkpoint.Identity, map[string]engine.ClipEnvelope, map[string]engine.ClipCalibration) {
	t.Helper()
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	identity := checkpoint.IdentityFor(set.Project, "q", "c", "a")
	params := engine.FrameParams{
		SampleRate: set.Project.SampleRate, FrameLen: set.Project.FrameLenSamples,
		Hop: set.Project.HopSamples, SessionGainQ16: set.Project.SessionGainQ16,
	}
	envelopes := make(map[string]engine.ClipEnvelope)
	for _, clip := range set.Project.Clips {
		env, err := engine.MeasureClip(clip, params, 960)
		if err != nil {
			t.Fatal(err)
		}
		envelopes[clip.ID] = env
	}
	cal, err := engine.CalibrateProject(set.Project, envelopes, 256)
	if err != nil {
		t.Fatal(err)
	}
	calibrations := engine.CalibrationByClip(cal)
	return set, identity, envelopes, calibrations
}

func TestAtomicLedgerAndStableOrder(t *testing.T) {
	set, identity, envelopes, calibrations := fixture(t)
	dir := t.TempDir()
	store, err := checkpoint.New(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	firstID := identity.ClipIDs[0]
	secondID := identity.ClipIDs[1]
	if _, err := store.Commit(checkpoint.CommitInput{
		ClipID: secondID, EnvelopeKey: "e2", CalibrationKey: "c2",
		Envelope: envelopes[secondID], Calibration: calibrations[secondID],
	}); !audioerr.IsCode(err, audioerr.CodeCheckpoint) {
		t.Fatalf("out-of-order commit accepted: %v", err)
	}
	record, err := store.Commit(checkpoint.CommitInput{
		ClipID: firstID, EnvelopeKey: "e1", CalibrationKey: "c1",
		Envelope: envelopes[firstID], Calibration: calibrations[firstID],
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Seq != 1 || record.ContentHash != set.Project.Clips[0].ContentHash {
		t.Fatalf("bad record: %+v", record)
	}
	restarted, err := checkpoint.New(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	records, err := restarted.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ClipID != firstID {
		t.Fatalf("bad restored records: %+v", records)
	}
}

func TestAnyHashMismatchRequiresWholeRebuild(t *testing.T) {
	_, identity, envelopes, calibrations := fixture(t)
	dir := t.TempDir()
	store, err := checkpoint.New(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	clipID := identity.ClipIDs[0]
	if _, err := store.Commit(checkpoint.CommitInput{
		ClipID: clipID, EnvelopeKey: "e", CalibrationKey: "c",
		Envelope: envelopes[clipID], Calibration: calibrations[clipID],
	}); err != nil {
		t.Fatal(err)
	}
	changed := identity
	changed.ClipHashes = map[string]string{}
	for key, value := range identity.ClipHashes {
		changed.ClipHashes[key] = value
	}
	changed.ClipHashes[clipID] = "different"
	stale, err := checkpoint.New(dir, changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Load(); !checkpoint.IsRebuildRequired(err) {
		t.Fatalf("want rebuild required, got %v", err)
	}
	if err := stale.Reset(); err != nil {
		t.Fatal(err)
	}
	records, err := stale.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("reset left records: %+v", records)
	}
}

func TestTamperedLedgerIsRejected(t *testing.T) {
	_, identity, envelopes, calibrations := fixture(t)
	dir := t.TempDir()
	store, err := checkpoint.New(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	clipID := identity.ClipIDs[0]
	if _, err := store.Commit(checkpoint.CommitInput{
		ClipID: clipID, EnvelopeKey: "e", CalibrationKey: "c",
		Envelope: envelopes[clipID], Calibration: calibrations[clipID],
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "checkpoint.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !checkpoint.IsRebuildRequired(err) {
		t.Fatalf("tampered ledger accepted: %v", err)
	}
}
