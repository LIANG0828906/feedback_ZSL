package verify_test

import (
	"context"
	"path/filepath"
	"testing"

	"lipsync/domain"
	"lipsync/generate"
	"lipsync/pipeline"
	"lipsync/schema"
	"lipsync/store"
	"lipsync/truth"
	"lipsync/verify"
)

func setupRun(t *testing.T, dbPath string) (*domain.Project, *truth.Manifest) {
	t.Helper()
	rep, err := generate.Generate(generate.Config{OutDir: t.TempDir(), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	p, m, err := schema.LoadBundle(rep.Projects["v3"])
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Run(context.Background(), p, st, pipeline.Options{
		Mode: pipeline.ModeFull, TruthFingerprint: m.Fingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	return p, m
}

func TestVerifyDBLocatesFirstFrameDifference(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "verify.db")
	_, m := setupRun(t, dbPath)
	if _, err := verify.VerifyDB(context.Background(), dbPath, m); err != nil {
		t.Fatalf("clean verification: %v", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var minFrame int64
	if err := st.DB().QueryRow(`SELECT MIN(frame) FROM frames`).Scan(&minFrame); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE frames SET package_ids_json='["tampered"]' WHERE frame=?`, minFrame); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	_, err = verify.VerifyDB(context.Background(), dbPath, m)
	if domain.ErrorCode(err) != domain.CodeDiff {
		t.Fatalf("code = %s: %v", domain.ErrorCode(err), err)
	}
	de, _ := domain.AsError(err)
	if de.Frame != minFrame || de.Field != "frames.package_ids" || de.DialogueID == "" {
		t.Fatalf("diff location = %+v", de)
	}
}

func TestCompareDBsDetectsSinglePathDifference(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.db")
	b := filepath.Join(dir, "b.db")
	_, m := setupRun(t, a)
	rep, err := generate.Generate(generate.Config{OutDir: filepath.Join(dir, "samples"), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := schema.LoadBundle(rep.Projects["v3"])
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Run(context.Background(), p, st, pipeline.Options{
		Mode: pipeline.ModeFull, TruthFingerprint: m.Fingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if _, err := verify.CompareDBs(context.Background(), a, b); err != nil {
		t.Fatalf("identical DBs differ: %v", err)
	}

	st, err = store.Open(b)
	if err != nil {
		t.Fatal(err)
	}
	var frame int64
	_ = st.DB().QueryRow(`SELECT MIN(frame) FROM frames`).Scan(&frame)
	if _, err := st.DB().Exec(`UPDATE frames SET package_ids_json='[]' WHERE frame=?`, frame); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	_, err = verify.CompareDBs(context.Background(), a, b)
	if domain.ErrorCode(err) != domain.CodeDiff {
		t.Fatalf("code = %s: %v", domain.ErrorCode(err), err)
	}
	de, _ := domain.AsError(err)
	if de.Frame != frame || de.Field != "frames.package_ids" {
		t.Fatalf("comparison diff = %+v", de)
	}
}
