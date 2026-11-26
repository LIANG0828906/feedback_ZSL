package mirrorcast_test

import (
	"encoding/json"
	"testing"

	"mirrorcast/internal/acceptance"
)

func TestFixedSeedAcceptance(t *testing.T) {
	report, err := acceptance.Run(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		data, _ := json.MarshalIndent(report, "", "  ")
		t.Fatalf("acceptance report not OK:\n%s", data)
	}
}
