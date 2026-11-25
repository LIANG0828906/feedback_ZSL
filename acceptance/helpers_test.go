package acceptance_test

import (
	"encoding/json"
	"os"
	"testing"

	"coldkernel/internal/checkpoint"
	"coldkernel/internal/model"
)

func decodeTick(t *testing.T, payload []byte) *model.TickResult {
	t.Helper()
	var tr model.TickResult
	if err := json.Unmarshal(payload, &tr); err != nil {
		t.Fatalf("decode tick: %v", err)
	}
	if tr.Functions == nil {
		tr.Functions = map[string]*model.FunctionTickState{}
	}
	return &tr
}

func storePath(s *checkpoint.Store) string {
	// Tests create stores inside t.TempDir; this helper is only used to
	// reopen the same path, which we thread through a small registry.
	return storeRegistry[s]
}

var storeRegistry = map[*checkpoint.Store]string{}

func openStoreAt(t *testing.T, path, source string) *checkpoint.Store {
	t.Helper()
	store, err := checkpoint.Open(path, source)
	if err != nil {
		t.Fatalf("reopen checkpoint: %v", err)
	}
	storeRegistry[store] = path
	return store
}

func stat(path string) (os.FileInfo, error) { return os.Stat(path) }

func writeTruncated(path string, size int64) {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	if size > int64(len(data)) {
		size = int64(len(data))
	}
	if err := os.WriteFile(path, data[:size], 0o644); err != nil {
		panic(err)
	}
}
