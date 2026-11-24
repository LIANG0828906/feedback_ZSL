package gen

import (
	"encoding/json"
	"os"
	"testing"

	"replaykernel/internal/model"
)

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func loadGenerated(t *testing.T, path string) *model.Replay {
	t.Helper()
	var r model.Replay
	if err := json.Unmarshal(mustReadFile(t, path), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
