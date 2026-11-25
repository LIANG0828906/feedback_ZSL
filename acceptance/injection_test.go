package acceptance_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"coldkernel/internal/checkpoint"
	"coldkernel/internal/errcode"
	"coldkernel/internal/gen"
	"coldkernel/internal/loader"
	"coldkernel/internal/runner"
)

func mutateV1(t *testing.T, raw []byte, fn func(*gen.SnapshotV1)) []byte {
	t.Helper()
	var s gen.SnapshotV1
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	fn(&s)
	out, err := json.Marshal(&s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertCode(t *testing.T, raw []byte, want errcode.Code) {
	t.Helper()
	_, err := loader.Load(raw)
	code, _ := errcode.From(err)
	if code != want {
		t.Fatalf("code=%s want %s (err=%v)", code, want, err)
	}
}

func TestInjectedDeclarationErrors(t *testing.T) {
	tr := loadTriple(t)
	base := tr.RawV1

	t.Run("negative concurrency", func(t *testing.T) {
		raw := mutateV1(t, base, func(s *gen.SnapshotV1) { s.Functions[0].ConcurrencyLimit = -3 })
		assertCode(t, raw, errcode.ENegConcurrency)
	})
	t.Run("reserved exceeds limit", func(t *testing.T) {
		raw := mutateV1(t, base, func(s *gen.SnapshotV1) { s.Functions[0].ReservedSlots = 99 })
		assertCode(t, raw, errcode.EReservedExceeds)
	})
	t.Run("warm pool out of range", func(t *testing.T) {
		raw := mutateV1(t, base, func(s *gen.SnapshotV1) {
			// More warm members than the function's concurrency limit.
			for k := int64(0); k <= s.Functions[0].ConcurrencyLimit; k++ {
				s.WarmPool = append(s.WarmPool, gen.V1WarmEntry{
					FunctionID: s.Functions[0].FunctionID,
					InstanceID: "warm-overrun",
				})
			}
		})
		assertCode(t, raw, errcode.EPoolOutOfRange)
	})
	t.Run("negative cold timeout", func(t *testing.T) {
		raw := mutateV1(t, base, func(s *gen.SnapshotV1) { s.Functions[0].ColdTimeoutTicks = -1 })
		assertCode(t, raw, errcode.ENegColdTimeout)
	})
	t.Run("tick out of order", func(t *testing.T) {
		raw := mutateV1(t, base, func(s *gen.SnapshotV1) {
			s.Requests[len(s.Requests)-1].ArriveTick = 0
		})
		assertCode(t, raw, errcode.ETickOrder)
	})
	t.Run("unknown function reference", func(t *testing.T) {
		raw := mutateV1(t, base, func(s *gen.SnapshotV1) {
			s.Requests[0].FunctionID = "fn-ghost"
		})
		assertCode(t, raw, errcode.EUnknownFunction)
	})
	t.Run("bad budget ratio", func(t *testing.T) {
		raw := mutateV1(t, base, func(s *gen.SnapshotV1) {
			bad := int64(5000) // > 1000‰
			s.Functions[0].ColdBudgetPermille = &bad
		})
		assertCode(t, raw, errcode.EBadBudget)
	})
	t.Run("unknown schema", func(t *testing.T) {
		var generic map[string]any
		if err := json.Unmarshal(base, &generic); err != nil {
			t.Fatal(err)
		}
		generic["schema_version"] = "coldkernel/v9"
		raw, _ := json.Marshal(generic)
		assertCode(t, raw, errcode.ESchemaVersion)
	})
	t.Run("unknown unit", func(t *testing.T) {
		raw := mutateV1(t, base, func(s *gen.SnapshotV1) { s.TickUnit = "fortnight" })
		assertCode(t, raw, errcode.EUnitUnknown)
	})
}

func TestSourceProjectNotMutatedByRejection(t *testing.T) {
	tr := loadTriple(t)
	before := tr.ProjectV1.Functions[0].ConcurrencyLimit
	raw := mutateV1(t, tr.RawV1, func(s *gen.SnapshotV1) {
		s.Functions[0].ConcurrencyLimit = -3
	})
	if _, err := loader.Load(raw); err == nil {
		t.Fatal("expected rejection")
	}
	// The already-loaded source project and its raw bytes stay intact.
	if tr.ProjectV1.Functions[0].ConcurrencyLimit != before {
		t.Fatal("source project was mutated")
	}
	if tr.RawV1[0] == 0 {
		t.Fatal("raw source snapshot damaged")
	}
}

func TestCommittedStateSurvivesBadInput(t *testing.T) {
	tr := loadTriple(t)
	p := tr.ProjectV1
	path := filepath.Join(t.TempDir(), "cp.db")
	store, err := checkpoint.Open(path, "proj")
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(p, store)
	if err != nil {
		t.Fatal(err)
	}
	r.StopAfterTick(4)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	last, _, _, _ := store.LatestTick()

	// A corrupt, unrelated input must not invalidate the committed checkpoint.
	bad := mutateV1(t, tr.RawV1, func(s *gen.SnapshotV1) {
		s.Functions[0].ConcurrencyLimit = -1
	})
	if _, err := loader.Load(bad); err == nil {
		t.Fatal("expected bad input rejection")
	}
	last2, _, _, err := store.LatestTick()
	if err != nil || last2 != last {
		t.Fatalf("committed state changed: %d -> %d", last, last2)
	}
	store.Close()
}
