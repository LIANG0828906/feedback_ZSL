package lightprobe

import (
	"testing"
)

func TestAcceptanceThreePathsAndBounds(t *testing.T) {
	report, err := RunAcceptance(t.TempDir())
	if err != nil {
		t.Fatalf("acceptance run failed: %v", err)
	}
	if !report.AllConsistent {
		if report.FullDiff != nil {
			t.Fatalf("full vs realtime diff: %v", report.FullDiff)
		}
		t.Fatalf("full vs resume diff: %v", report.ResumeDiff)
	}
	if len(report.BoundViolations) != 0 {
		t.Fatalf("error upper-bound violations: %v", report.BoundViolations)
	}
	if len(report.Full.CommitOrder) != 3 || len(report.Resume.CommitOrder) != 3 {
		t.Fatalf("commit orders full=%v resume=%v", report.Full.CommitOrder, report.Resume.CommitOrder)
	}
	t.Logf("queries=%d max_actual=%.3e max_upper=%.3e uncertain=%d queue_waits=%d restored=%v",
		len(report.Full.QueryResults), report.BoundMaxActual, report.BoundMaxUpper,
		countUncertain(report.Full), report.Realtime.Queue.WaitCount, report.Resume.RestoredRooms)
}

func countUncertain(pr *PipelineResult) int {
	n := 0
	for _, q := range pr.QueryResults {
		if q.Uncertain {
			n++
		}
	}
	return n
}
