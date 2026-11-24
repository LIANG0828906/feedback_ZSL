package demo_test

import (
	"os"
	"path/filepath"
	"testing"

	"diffrender/demo"
)

func TestEndToEndDemo(t *testing.T) {
	cfg := demo.DefaultConfig()
	cfg.Tasks = 1
	cfg.OutDir = t.TempDir()
	cfg.Optim.Render.MaxWorkers = 2
	cfg.Optim.Render.QueueCapacity = 2
	report, err := demo.Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !report.AllThreePathsEqual || !report.AllConverged || !report.AllChecksPassed {
		t.Fatalf("demo report: paths=%v converged=%v checks=%v",
			report.AllThreePathsEqual, report.AllConverged, report.AllChecksPassed)
	}
	if len(report.Tasks) != 1 {
		t.Fatalf("tasks=%d", len(report.Tasks))
	}
	task := report.Tasks[0]
	if len(task.Versions) != 3 || len(task.Exports) != 3 || len(task.Windows) == 0 {
		t.Fatalf("incomplete task report: versions=%d exports=%d windows=%d", len(task.Versions), len(task.Exports), len(task.Windows))
	}
	if !task.ThreePathsEqual || !task.ParallelSerialEqual || !task.RecoveryChecked {
		t.Fatalf("task equality flags: %+v", task)
	}
	for _, c := range task.Checks {
		if !c.Passed {
			t.Fatalf("failed check: %+v", c)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.OutDir, "report.json")); err != nil {
		t.Fatal(err)
	}
}
