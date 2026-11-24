package engine

import (
	"encoding/json"
	"os"
	"testing"

	"replaykernel/internal/model"
)

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustTruth(t *testing.T, raw []byte) *model.MatchTruth {
	t.Helper()
	var tr model.MatchTruth
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatal(err)
	}
	return &tr
}
