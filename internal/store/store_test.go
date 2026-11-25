package store

import (
	"path/filepath"
	"testing"

	"maintenanceorchestrator/internal/domain"
)

func TestUncommittedLeaseRollsBackAndCheckpointRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(`INSERT INTO lease_states
		(lease_id,lot_id,lender_node_id,borrower_node_id,amount_milli,start_tick,expiry_tick,status,returned_tick)
		VALUES ('uncommitted','lot-x','m-fast','t-1',1000,10,12,'active',0)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	count := 0
	if err := s.db.QueryRow(`SELECT count(*) FROM lease_states WHERE lease_id='uncommitted'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("uncommitted lease persisted: count=%d", count)
	}
	if err := s.SaveCommit(Checkpoint{StateVersion: CurrentStateVersion, ProjectID: "p", CommittedWave: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	cp, found, err := reopened.LoadRecovery()
	if err != nil || !found || cp.CommittedWave != 1 {
		t.Fatalf("cp=%+v found=%v err=%v", cp, found, err)
	}
}

func TestEnvelopeTruncationHasStableCode(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveEnvelope("cp", []byte(`{"hello":"world-and-more"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.TruncateEnvelope("cp"); err != nil {
		t.Fatal(err)
	}
	_, err = s.LoadEnvelope("cp")
	if !domain.IsCode(err, domain.ECheckpointTruncated) {
		t.Fatalf("err=%v", err)
	}
}
