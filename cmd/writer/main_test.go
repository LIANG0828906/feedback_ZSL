package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAndScanTruncatesIncompleteFinalBatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.jsonl")
	lines := []string{
		`{"_batch_id":"b1","_tenant_id":"tenant-01","_record_id":"r1","_batch_record_index":0,"_batch_record_count":2}`,
		`{"_batch_id":"b1","_tenant_id":"tenant-01","_record_id":"r2","_batch_record_index":1,"_batch_record_count":2}`,
		`{"_batch_id":"b2","_tenant_id":"tenant-02","_record_id":"r3","_batch_record_index":0,"_batch_record_count":3}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writer := &Writer{
		outputPath: path,
		seen:       make(map[string]struct{}),
		stats: Stats{
			Tenants: map[string]TenantStats{
				"tenant-01": {},
				"tenant-02": {},
			},
		},
	}
	if err := writer.openAndScan(); err != nil {
		t.Fatal(err)
	}
	defer writer.file.Close()
	if writer.stats.WrittenRecords != 2 || writer.stats.WrittenBatches != 1 {
		t.Fatalf("stats after truncation = %d records/%d batches, want 2/1", writer.stats.WrittenRecords, writer.stats.WrittenBatches)
	}
	if _, ok := writer.seen["b1"]; !ok {
		t.Fatal("complete batch b1 was not indexed")
	}
	if _, ok := writer.seen["b2"]; ok {
		t.Fatal("incomplete batch b2 must not be indexed")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(lines[0])+1+len(lines[1])+1) {
		t.Fatalf("output was not truncated to the last complete batch: size=%d", info.Size())
	}
	var row map[string]any
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	firstLine := strings.SplitN(string(raw), "\n", 2)[0]
	if err := json.Unmarshal([]byte(firstLine), &row); err != nil {
		t.Fatal(err)
	}
	if row["_batch_id"] != "b1" {
		t.Fatalf("unexpected surviving row: %v", row)
	}
}

func TestOpenAndScanKeepsCompleteBatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.jsonl")
	line := `{"_batch_id":"b1","_tenant_id":"tenant-01","_record_id":"r1","_batch_record_index":0,"_batch_record_count":1}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writer := &Writer{
		outputPath: path,
		seen:       make(map[string]struct{}),
		stats:      Stats{Tenants: map[string]TenantStats{"tenant-01": {}}},
	}
	if err := writer.openAndScan(); err != nil {
		t.Fatal(err)
	}
	defer writer.file.Close()
	if _, ok := writer.seen["b1"]; !ok {
		t.Fatal("complete batch was not indexed")
	}
	if writer.stats.WrittenRecords != 1 {
		t.Fatalf("written records = %d, want 1", writer.stats.WrittenRecords)
	}
}
