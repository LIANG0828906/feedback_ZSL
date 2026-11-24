package checkpoint

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"saveinterop/internal/core"
)

func TestConcurrentWindowWritesAreSerialized(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	meta := RunMetadata{
		RunID: "run-1", Mode: "import", SourceBranch: "aurora",
		TargetBranch: "runtime", MappingVersion: "mapping-v1",
		WindowSize: 2, InputHash: "input",
	}
	first, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := first.ValidateOrCreate(context.Background(), meta); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ValidateOrCreate(context.Background(), meta); err != nil {
		t.Fatal(err)
	}

	record := WindowRecord{
		Index: 0, ItemStart: 0, ItemEnd: 2,
		FingerprintDelta:      "delta-0",
		CumulativeFingerprint: NextCumulative("", "delta-0"),
		Payload:               []byte("payload-0"),
	}
	results := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(1)
	write := func(store *Store) {
		start.Wait()
		results <- store.AppendWindow(context.Background(), meta, record)
	}
	go write(first)
	go write(second)
	start.Done()

	successes, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case core.CodeOf(err) == core.CodeVersion:
			conflicts++
		default:
			t.Fatalf("unexpected write error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	count, err := first.CountWindows(context.Background(), meta.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("window count = %d, want 1", count)
	}
	next := WindowRecord{
		Index: 1, ItemStart: 2, ItemEnd: 4,
		FingerprintDelta:      "delta-1",
		CumulativeFingerprint: NextCumulative(record.CumulativeFingerprint, "delta-1"),
		Payload:               []byte("payload-1"),
	}
	if err := first.AppendWindow(context.Background(), meta, next); err != nil {
		t.Fatalf("append next window: %v", err)
	}
	if err := first.Complete(context.Background(), meta, 2); err != nil {
		t.Fatalf("complete: %v", err)
	}
	records, err := first.ValidateOrCreate(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("validated windows = %d", len(records))
	}
}
