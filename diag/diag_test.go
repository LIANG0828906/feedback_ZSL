package diag_test

import (
	"testing"

	"proxydiag/bizerr"
	"proxydiag/diag"
	"proxydiag/samplegen"
)

func samplesDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := samplegen.Write(dir, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestVerificationReport(t *testing.T) {
	report, err := diag.Verify(samplesDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if !report.EquivalentCore || !report.AllTruthMatched || !report.DecisionParity || !report.TickMsParity {
		t.Fatalf("verification failed: %+v", report)
	}
}

func TestIncrementalReport(t *testing.T) {
	report, err := diag.Incremental(samplesDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if !report.SourceUnchanged || !report.AllMatchFull || len(report.Steps) != 5 {
		t.Fatalf("unexpected incremental report: %+v", report)
	}
}

func TestCheckpointReport(t *testing.T) {
	report, err := diag.CheckpointDemo(samplesDir(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !report.AllDecisionsMatch || !report.UncommittedIgnored || !report.PriorUsableAfterCut {
		t.Fatalf("checkpoint checks failed: %+v", report)
	}
	if report.TruncationCode != bizerr.CheckpointTruncated {
		t.Fatalf("truncation code = %s", report.TruncationCode)
	}
	if report.VersionMismatchCode != bizerr.CheckpointVersion {
		t.Fatalf("version mismatch code = %s", report.VersionMismatchCode)
	}
	if report.VersionMismatchRecomputed != 40 {
		t.Fatalf("version mismatch recompute = %d", report.VersionMismatchRecomputed)
	}
}

func TestAnomalyReport(t *testing.T) {
	report, err := diag.InjectAnomalies(samplesDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if !report.AllStable || len(report.Cases) < 9 {
		t.Fatalf("anomaly injections failed: %+v", report)
	}
	required := map[string]bool{
		bizerr.ListenerOverlap:     false,
		bizerr.UpstreamMissing:     false,
		bizerr.RewriteEmpty:        false,
		bizerr.WeightRange:         false,
		bizerr.InflightRange:       false,
		bizerr.TimeoutOrder:        false,
		bizerr.CheckpointTruncated: false,
		bizerr.SnapshotVersion:     false,
	}
	for _, c := range report.Cases {
		if _, ok := required[c.Code]; ok {
			required[c.Code] = true
		}
	}
	for code, found := range required {
		if !found {
			t.Fatalf("missing injected code %s", code)
		}
	}
}
