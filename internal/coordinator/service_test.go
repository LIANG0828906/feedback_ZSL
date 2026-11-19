package coordinator

import (
	"testing"

	"quota-coordinator/internal/model"
)

func TestPrefixThatFitsRespectsBothLimits(t *testing.T) {
	batch := model.Batch{
		TenantID: "tenant-01",
		BatchID:  "b1",
		Records: []model.Record{
			{RecordID: "r1", Bytes: 10},
			{RecordID: "r2", Bytes: 20},
			{RecordID: "r3", Bytes: 30},
			{RecordID: "r4", Bytes: 40},
		},
		Count: 4,
		Bytes: 100,
	}
	accepted := prefixThatFits(batch, 10, 60)
	if accepted.Count != 3 || accepted.Bytes != 60 {
		t.Fatalf("accepted = %d records/%d bytes, want 3/60", accepted.Count, accepted.Bytes)
	}
	limitedByBytes := prefixThatFits(batch, 10, 35)
	if limitedByBytes.Count != 2 || limitedByBytes.Bytes != 30 {
		t.Fatalf("byte-limited prefix = %d/%d, want 2/30", limitedByBytes.Count, limitedByBytes.Bytes)
	}
}

func TestIngestResponseCodesAreStable(t *testing.T) {
	codes := []string{
		CodeAccepted,
		CodeDuplicate,
		CodePartialDrop,
		CodeQuotaExceeded,
		CodeGlobalBackpressure,
		CodeTenantDegraded,
		CodeUnknownTenant,
		CodeInvalidBatch,
		CodeInvalidQuota,
		CodeRedisUnavailable,
		CodePartialAccept,
	}
	seen := map[string]bool{}
	for _, code := range codes {
		if code == "" || seen[code] {
			t.Fatalf("empty or duplicate code %q", code)
		}
		seen[code] = true
	}
}
