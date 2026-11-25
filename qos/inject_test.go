package qos

import (
	"context"
	"os"
	"testing"
)

func TestInjectionCasesReturnStableBusinessCodes(t *testing.T) {
	report, err := RunInjectionCases(context.Background(), 42, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]Code{
		"negative_rate":               ERateNegative,
		"bucket_smaller_than_packet":  EBucketTooSmall,
		"field_conflict":              EFieldConflict,
		"thread_range":                EThreadRange,
		"queue_budget_range":          EQueueBudget,
		"expiry_order_reversed":       EExpiryOrder,
		"checkpoint_version_mismatch": ECheckpointVersion,
		"truncated_checkpoint":        ECheckpointTruncated,
	}
	if len(report.Cases) != len(expected) {
		t.Fatalf("case count=%d expected=%d", len(report.Cases), len(expected))
	}
	for _, c := range report.Cases {
		if !c.Handled || !c.SourceStable {
			t.Fatalf("case %s handled=%v source_stable=%v", c.Name, c.Handled, c.SourceStable)
		}
		if want := expected[c.Name]; c.Code != want {
			t.Fatalf("case %s code=%s want=%s", c.Name, c.Code, want)
		}
		if c.Path == "" {
			t.Fatalf("case %s lacks a locatable path", c.Name)
		}
	}
	if !report.AllPass {
		t.Fatalf("injection report not all-pass: %#v", report)
	}
}

func TestGeneratorIsDeterministicAndWritesTruth(t *testing.T) {
	dir := t.TempDir()
	m1, err := GenerateSample(context.Background(), 77, dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(dir + "/sample_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := GenerateSample(context.Background(), 77, dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(dir + "/sample_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) || m1.ProjectDigest != m2.ProjectDigest {
		t.Fatal("generator output is not deterministic")
	}
	if !m1.VersionsNormalized || m1.Steps == 0 || m1.Emitted == 0 || m1.DelayedEvents == 0 || m1.Dropped == 0 {
		t.Fatalf("sample did not exercise required paths: %#v", m1)
	}
}
