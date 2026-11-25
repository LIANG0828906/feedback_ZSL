package main_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/liang/hapticfeedback/internal/haptic/checkpoint"
	"github.com/liang/hapticfeedback/internal/haptic/diag"
	"github.com/liang/hapticfeedback/internal/haptic/model"
	"github.com/liang/hapticfeedback/internal/haptic/pipeline"
	"github.com/liang/hapticfeedback/internal/haptic/reference"
	"github.com/liang/hapticfeedback/internal/haptic/samplegen"
	"github.com/liang/hapticfeedback/internal/haptic/verify"
)

func TestAcceptanceReportPasses(t *testing.T) {
	root := t.TempDir()
	report, _, err := verify.Run(filepath.Join(root, "samples"), filepath.Join(root, "work"))
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		b, _ := json.MarshalIndent(report, "", "  ")
		t.Fatalf("acceptance report failed:\n%s", b)
	}
	if report.PacketCount != 5 || report.TotalSamples != 39 {
		t.Fatalf("unexpected packet/sample total: %d/%d", report.PacketCount, report.TotalSamples)
	}
	if !report.IndependentReviewOK || report.ReviewViolation != nil {
		t.Fatalf("independent review failed: %#v", report.ReviewViolation)
	}
}

func TestGeneratorIsByteDeterministic(t *testing.T) {
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	if _, err := samplegen.Generate(dirA); err != nil {
		t.Fatal(err)
	}
	if _, err := samplegen.Generate(dirB); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"branch_1.json", "branch_2.json", "branch_3.json", "truth.json", "manifest.json"} {
		a := readTestFile(t, filepath.Join(dirA, name))
		b := readTestFile(t, filepath.Join(dirB, name))
		if string(a) != string(b) {
			t.Fatalf("generated file %s is not deterministic", name)
		}
	}
}

func TestIndependentReviewRecomputesTamperedFrame(t *testing.T) {
	stream := samplegen.CanonicalStream()
	for i := range stream.Events {
		stream.Events[i].DesignerNote = ""
	}
	plan, err := pipeline.BuildPlan(&stream, samplegen.Capability(), 1)
	if err != nil {
		t.Fatal(err)
	}
	pkt := plan.Packets[0]
	pkt.Frames[0].FrameQ8 += 4 * model.FixedScale
	pkt.FrameDigest = model.DigestFrames(pkt.Frames)
	pkt.ContentHash = model.PacketContentHash(pkt)

	violation := reference.ReviewPacket(pkt, stream.Fingerprint())
	if violation == nil {
		t.Fatal("independent reviewer accepted tampered frame")
	}
	if violation.Item != "frame_q8" || violation.PacketSeq != 0 {
		t.Fatalf("unexpected violation: %#v", violation)
	}
}

func TestCheckpointVersionMismatchForcesFullRecompute(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "version.db")
	stream := samplegen.CanonicalStream()
	for i := range stream.Events {
		stream.Events[i].DesignerNote = ""
	}
	plan, err := pipeline.BuildPlan(&stream, samplegen.Capability(), 1)
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	bad := plan.Packets[0]
	bad.RuleVersion = "rules-tampered"
	if err := store.CommitPacket(bad, checkpoint.Meta{Fingerprint: stream.Fingerprint()}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := pipeline.RunCheckpointed(&stream, samplegen.Capability(), 1, pipeline.RunOptions{
		DBPath: dbPath, Resume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.CheckpointReset {
		t.Fatal("version mismatch did not reset checkpoint")
	}
	if len(result.Packets) != len(plan.Packets) {
		t.Fatalf("packets=%d want %d", len(result.Packets), len(plan.Packets))
	}
	if diff := reference.FirstPacketDifference(plan.Packets, result.Packets); diff != nil {
		t.Fatalf("recomputed packets differ: %#v", diff)
	}
}

func TestDiagnosticAPIStageFailureAndRetry(t *testing.T) {
	root := t.TempDir()
	server, err := diag.NewServer(filepath.Join(root, "samples"), filepath.Join(root, "work"))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	state := getState(t, httpServer.URL+"/api/state")
	if state["ok"] != true {
		t.Fatalf("initial state not ok: %#v", state)
	}

	injected := postState(t, httpServer.URL+"/api/inject?kind=stage")
	inj, _ := injected["injection"].(map[string]any)
	if inj == nil || inj["code"] != "E_STAGE" {
		t.Fatalf("expected E_STAGE injection, got %#v", injected["injection"])
	}

	retried := postState(t, httpServer.URL+"/api/retry?seq=2")
	matrix, _ := retried["matrix"].([]any)
	if len(matrix) != 5 {
		t.Fatalf("matrix length=%d", len(matrix))
	}
	foundCommitted := false
	for _, item := range matrix {
		m := item.(map[string]any)
		if m["seq"].(float64) == 2 && m["state"] == "committed" {
			foundCommitted = true
		}
	}
	if !foundCommitted {
		t.Fatalf("packet 2 was not retried to committed: %#v", matrix)
	}
	if committed, _ := retried["committed"].(float64); committed != 5 {
		t.Fatalf("retry did not continue ordered sequence, committed=%v", retried["committed"])
	}
	review, _ := retried["review"].(map[string]any)
	if review == nil || review["ok"] != true {
		t.Fatalf("review after retry failed: %#v", retried["review"])
	}
}

func getState(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func postState(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
