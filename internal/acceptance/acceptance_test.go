package acceptance_test

import (
	"testing"

	"channelpipe/internal/acceptance"
)

func TestTinyAcceptancePasses(t *testing.T) {
	report, err := acceptance.Run(acceptance.Config{
		OutputDir:  t.TempDir(),
		Tiny:       true,
		Workers:    2,
		TileWidth:  32,
		TileHeight: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("acceptance report not OK: %+v", report.Errors)
	}
	if report.IndependentVerify == nil || !report.IndependentVerify.OK {
		t.Fatalf("independent verification failed: %+v", report.IndependentVerify)
	}
	if report.ThreePaths == nil || !report.ThreePaths.OK {
		t.Fatalf("three paths failed: %+v", report.ThreePaths)
	}
	if report.MainFailure == nil || !report.MainFailure.Isolated || !report.MainFailure.Recovered {
		t.Fatalf("failure isolation failed: %+v", report.MainFailure)
	}
}
