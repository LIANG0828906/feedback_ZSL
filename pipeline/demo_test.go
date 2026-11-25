package pipeline

import (
	"context"
	"strings"
	"testing"
)

func TestDemoRunsEndToEnd(t *testing.T) {
	report, err := RunDemo(context.Background(), DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Versions) != 3 {
		t.Fatalf("expected three versions")
	}
	for _, version := range report.Versions {
		if !version.FeaturesMatch || !version.EventsMatch || !version.WindowBoundsMatch {
			t.Fatalf("%s structural equivalence failed", version.Version)
		}
	}
	if report.Incremental == nil || !report.Incremental.MatchesFull {
		t.Fatalf("incremental demo failed: %+v", report.Incremental)
	}
	if report.Resume == nil || !report.Resume.WindowsMatchFull || !report.Resume.DiscardsMatchFull {
		t.Fatalf("resume demo failed: %+v", report.Resume)
	}
	if report.Resume.VersionMismatchCode != CodeCheckpointVersionMismatch ||
		report.Resume.TruncatedCheckpointCode != CodeCheckpointTruncated {
		t.Fatalf("checkpoint anomaly codes missing: %+v", report.Resume)
	}
	if len(report.Anomalies) < 9 {
		t.Fatalf("anomaly matrix incomplete: %+v", report.Anomalies)
	}
	text := RenderDemoText(report)
	if !strings.Contains(text, "DISCARD") || !strings.Contains(text, "[resume]") {
		t.Fatalf("text report lacks acceptance sections")
	}
}
