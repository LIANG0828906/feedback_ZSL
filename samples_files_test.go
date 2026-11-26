package acceptance

import (
	"os"
	"testing"

	"quota-cluster/internal/engine"
	"quota-cluster/internal/model"
	"quota-cluster/internal/projectio"
)

func TestGeneratedSampleFilesOnDisk(t *testing.T) {
	versions := []string{"v1", "v2", "v3"}
	var first *model.Report
	for _, version := range versions {
		path := "samples/project-" + version + ".json"
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s (run `go run ./cmd/quota-workbench generate`): %v", path, err)
		}
		project, err := projectio.Load(data)
		if err != nil {
			t.Fatalf("load %s: %v", path, err)
		}
		report, err := engine.Run(project, engine.Options{Workers: 4, BatchSize: 2})
		if err != nil {
			t.Fatalf("run %s: %v", path, err)
		}
		if first == nil {
			first = report
			continue
		}
		if len(first.Ticks) != len(report.Ticks) || len(first.Decisions) != len(report.Decisions) {
			t.Fatalf("%s produced different report shape", version)
		}
	}
	for _, name := range []string{"truth.json", "critical-cases.json"} {
		if _, err := os.Stat("samples/" + name); err != nil {
			t.Fatalf("sample artifact %s missing: %v", name, err)
		}
	}
}
