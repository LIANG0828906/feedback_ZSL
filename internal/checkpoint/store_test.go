package checkpoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"msworkbench/internal/domain"
	"msworkbench/internal/e"
)

func TestRecoverRejectsContentHashMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")
	store, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CommitStage(StageRecord{RunID: "r1", Block: "0,0", Stage: StageQuant, OutputDigest: "out", CacheKey: "key", ContentHash: "hash-a", ParamVersion: domain.ParamVersion, AlgoVersion: domain.AlgorithmVersion}, nil)
	if err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var data fileData
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	data.Transactions[0].Record.ContentHash = "tampered"
	b, _ = json.MarshalIndent(&data, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reopened.RecoverRun("r1", map[string]string{"0,0": "hash-a"})
	ce, ok := e.As(err)
	if !ok || ce.Code != e.Ver || ce.Block != "0,0" || ce.Field != "content_hash" {
		t.Fatalf("expected E_VER content_hash, got %#v", err)
	}
}

func TestRecoverRejectsVersionMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")
	store, _ := OpenFile(path)
	_, err := store.CommitStage(StageRecord{RunID: "r1", Block: "1,0", Stage: StageRecon, ContentHash: "h", ParamVersion: domain.ParamVersion, AlgoVersion: "old-algo"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reopened, _ := OpenFile(path)
	_, err = reopened.RecoverRun("r1", nil)
	ce, ok := e.As(err)
	if !ok || ce.Code != e.Ver || ce.Field != "version" {
		t.Fatalf("expected E_VER version, got %#v", err)
	}
}
