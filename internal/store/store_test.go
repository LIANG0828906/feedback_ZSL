package store

import (
	"os"
	"path/filepath"
	"testing"

	"vqa/internal/metrics"
)

func testMeta() Meta {
	return Meta{
		AlgorithmVersion: "vqa-algo-1",
		Quality:          70,
		ManifestHash:     "abc",
		Params:           metrics.DefaultParams(8, 32, 32, 1),
		FrameCount:       2,
		BlocksPerFrame:   16,
	}
}

func TestTornTailIsTruncated(t *testing.T) {
	dir := t.TempDir()
	st, err := Create(dir, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	rec := &BlockRecord{Frame: 0, BX: 0, BY: 0, State: "committed", ReconPixels: []uint8{1, 2}}
	if err := st.AppendBlock(rec); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a forced kill mid-write: append an unterminated corrupt line.
	logPath := filepath.Join(dir, LogName)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"kind\":\"block\",\"frame\":9,\"crc\":123}"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("torn tail must be recoverable: %v", err)
	}
	blocks := ro.Blocks()
	if len(blocks) != 1 || blocks[0].Frame != 0 {
		t.Fatalf("expected only the durable block, got %d", len(blocks))
	}
}

func TestVersionMismatchInvalidatesSnapshot(t *testing.T) {
	dir := t.TempDir()
	st, err := Create(dir, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	changed := testMeta()
	changed.Params.Window = 5
	if _, err := Open(dir, "abc", 70, changed.Params); err == nil {
		t.Fatal("parameter mismatch must invalidate the snapshot")
	}
	if Exists(dir) {
		t.Fatal("invalidated snapshot files must be removed")
	}
}
