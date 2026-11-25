package terrainpipeline_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"terrainpipeline/internal/verify"
)

func TestLocalSampleAcceptance(t *testing.T) {
	report, err := verify.RunAll(filepath.Join(t.TempDir(), "verification"))
	if err != nil {
		t.Fatalf("RunAll returned error: %v", err)
	}
	if !report.Passed {
		data, _ := json.MarshalIndent(report, "", "  ")
		t.Fatalf("acceptance report failed:\n%s", data)
	}
}
