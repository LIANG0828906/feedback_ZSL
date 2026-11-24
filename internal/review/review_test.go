package review_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"screenverify/internal/model"
	"screenverify/internal/review"
	"screenverify/internal/sample"
	"screenverify/internal/store"
)

func TestReviewDetectsPayloadDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	truth, err := sample.GenerateClip(filepath.Join(dir, "clean"), "clean", "clean", 77, 8)
	if err != nil {
		t.Fatal(err)
	}
	truthPath := filepath.Join(dir, "clean", "truth.json")
	dbPath := filepath.Join(dir, "review.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := model.FingerprintTruth(truth)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.SetRunMeta(ctx, "truth_fingerprint", fp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SubmitBatch(ctx, store.Batch{
		BatchID: "b1", ParamVersion: "p", AlgoVersion: model.AlgorithmVersion,
		TruthFP: fp, ContentRoot: "c",
		Entries: []store.Entry{{
			CacheKey: "decision-key", FrameNo: 1, EvidenceType: "decision",
			AlgoVersion: model.AlgorithmVersion, ParamVersion: "p",
			Payload: map[string]any{
				"tampered": false, "score": 0.0, "confidence": 0.0, "threshold": 0.55,
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	before, err := review.Run(dbPath, truthPath)
	if err != nil {
		t.Fatal(err)
	}
	if !before.OK || before.Violation != nil {
		t.Fatalf("minimal valid evidence should pass: %+v", before.Violation)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE evidence SET payload_json='{"tampered":false}' WHERE cache_key='decision-key'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := review.Run(dbPath, truthPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.OK || after.Violation == nil {
		t.Fatal("expected digest violation")
	}
	if after.Violation.Frame != 1 || after.Violation.EvidenceType != "decision" || after.Violation.Item != "payload_sha256" {
		t.Fatalf("unexpected violation: %+v", after.Violation)
	}
}
