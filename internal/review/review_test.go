package review_test

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"integritykernel/internal/core"
	"integritykernel/internal/pipeline"
	"integritykernel/internal/review"
)

func TestIndependentReviewDetectsPersistedVerdictTampering(t *testing.T) {
	sampleDir := t.TempDir()
	artifactDir := t.TempDir()
	cross, err := pipeline.RunCrossCheck(sampleDir, artifactDir)
	if err != nil || !cross.OK {
		t.Fatalf("crosscheck setup failed: %v %#v", err, cross)
	}
	dbPath := filepath.Join(artifactDir, "recovery-checkpoint.db")
	fingerprintPath := filepath.Join(sampleDir, "truth.fingerprint.json")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var cacheKey, payload string
	err = db.QueryRow(`
SELECT c.cache_key, e.payload_json
FROM current_evidence c JOIN evidence e ON e.cache_key=c.cache_key
WHERE c.frame_index=13 AND c.evidence_type='verdict'`).Scan(&cacheKey, &payload)
	if err != nil {
		t.Fatal(err)
	}
	var verdict map[string]any
	if err := json.Unmarshal([]byte(payload), &verdict); err != nil {
		t.Fatal(err)
	}
	verdict["tampered"] = false
	forged, err := json.Marshal(verdict)
	if err != nil {
		t.Fatal(err)
	}
	digest := core.SHA256Hex(forged)
	if _, err := db.Exec(`UPDATE evidence SET payload_json=?, digest=? WHERE cache_key=?`, string(forged), digest, cacheKey); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := review.Run(dbPath, fingerprintPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.FirstViolation == nil {
		t.Fatalf("expected review violation, got %#v", result)
	}
	v := result.FirstViolation
	if v.Frame == nil || *v.Frame != 13 || v.EvidenceType != core.EvidenceVerdict || v.Field != "tampered" {
		t.Fatalf("unexpected first violation: %#v", v)
	}
}

func TestValidReviewPasses(t *testing.T) {
	sampleDir := t.TempDir()
	artifactDir := t.TempDir()
	cross, err := pipeline.RunCrossCheck(sampleDir, artifactDir)
	if err != nil {
		t.Fatal(err)
	}
	if cross.Review == nil || !cross.Review.OK {
		t.Fatalf("expected valid crosscheck review, got %#v", cross.Review)
	}
}
