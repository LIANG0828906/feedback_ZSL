package acoustics_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"roomacoustics/pkg/acoustics"
	"roomacoustics/pkg/generator"
)

func testLevel(t *testing.T) *acoustics.Level {
	t.Helper()
	spec, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	return spec.Level
}

func TestGeneratorSampleValidatesAndCoversEntities(t *testing.T) {
	l := testLevel(t)
	if err := acoustics.Validate(l); err != nil {
		t.Fatalf("generated level must be valid: %v", err)
	}
	if len(l.Rooms) != 4 || len(l.Doors) != 3 || len(l.Sources) != 4 || len(l.Listeners) != 6 {
		t.Fatalf("unexpected sample counts: rooms=%d doors=%d sources=%d listeners=%d", len(l.Rooms), len(l.Doors), len(l.Sources), len(l.Listeners))
	}
}

func TestSchemaMissingFieldReturnsESchemaObject(t *testing.T) {
	_, err := acoustics.ParseLevel([]byte(`{"rooms":[{}]}`))
	var ve *acoustics.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T %v", err, err)
	}
	found := false
	for _, is := range ve.Issues {
		if is.Code == acoustics.CodeSchema && strings.Contains(is.ObjectID, "rooms[0]") && is.Field == "rect" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing rect did not produce object-scoped E_SCHEMA: %+v", ve.Issues)
	}
}

func TestGeometryFailuresAreObjectScopedEGeom(t *testing.T) {
	t.Run("listener outside", func(t *testing.T) {
		l := testLevel(t)
		l.Listeners[0].Position.X = 9999
		err := acoustics.Validate(l)
		de, ok := acoustics.AsDomainError(err)
		if !ok || de.Code != acoustics.CodeGeom || de.ObjectID != l.Listeners[0].ID {
			t.Fatalf("expected E_GEOM for listener, got %#v / %v", de, err)
		}
	})

	t.Run("room overlap", func(t *testing.T) {
		l := testLevel(t)
		l.Rooms[1].Rect.X1 = 9
		err := acoustics.Validate(l)
		if !hasIssue(err, acoustics.CodeGeom, "R1,R2", "overlap") {
			t.Fatalf("expected overlap issue, got %v", err)
		}
	})

	t.Run("wall overlap", func(t *testing.T) {
		l := testLevel(t)
		l.Walls = append(l.Walls, acoustics.Wall{ID: "W-DUP", RoomA: "R1", RoomB: "R2", A: l.Walls[0].A, B: l.Walls[0].B})
		err := acoustics.Validate(l)
		if !hasIssue(err, acoustics.CodeGeom, "W12,W-DUP", "overlap") {
			t.Fatalf("expected wall overlap issue, got %v", err)
		}
	})

	t.Run("door outside wall", func(t *testing.T) {
		l := testLevel(t)
		l.Doors[0].Center.Y = 0.2
		l.Doors[0].Width = 4
		err := acoustics.Validate(l)
		if !hasIssue(err, acoustics.CodeGeom, "D12", "outside wall") {
			t.Fatalf("expected door geometry issue, got %v", err)
		}
	})

	t.Run("normal not unit", func(t *testing.T) {
		l := testLevel(t)
		l.Reflectors[0].Normal = acoustics.Point{X: 0, Y: 2}
		err := acoustics.Validate(l)
		if !hasIssue(err, acoustics.CodeGeom, l.Reflectors[0].ID, "unit") {
			t.Fatalf("expected normal issue, got %v", err)
		}
	})
}

func TestDoorChainAndUnreachableSearchedChains(t *testing.T) {
	l := testLevel(t)
	idx := acoustics.NewIndex(l)
	chain, err := idx.SolveSourceListenerChain("SRC-R1", "LIS-R3-A")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(chain.RoomIDs, ","); got != "R1,R2,R3" {
		t.Fatalf("room chain = %s", got)
	}
	if got := strings.Join(chain.DoorIDs, ","); got != "D12,D23" {
		t.Fatalf("door chain = %s", got)
	}
	if chain.LengthFixed <= 0 || chain.LengthM < 20 {
		t.Fatalf("unexpected chain length %+v", chain)
	}

	deleted, err := acoustics.DeleteDoor(l, "D23")
	if err != nil {
		t.Fatal(err)
	}
	_, err = acoustics.NewIndex(deleted).SolveSourceListenerChain("SRC-R1", "LIS-R3-A")
	de, ok := acoustics.AsDomainError(err)
	if !ok || de.Code != acoustics.CodeUnreach {
		t.Fatalf("expected E_UNREACH, got %#v", err)
	}
	details, ok := de.Details.(acoustics.UnreachableDetails)
	if !ok || details.SourceRoomID != "R1" || details.ListenerRoomID != "R3" || len(details.SearchedChains) == 0 {
		t.Fatalf("missing searched chain details: %#v", de.Details)
	}
}

func TestStateMachineIllegalTransitionPreservesState(t *testing.T) {
	sm := acoustics.NewStateMachine([]string{"R1"})
	if err := sm.Transition("R1", acoustics.StateCommitted); err == nil {
		t.Fatal("expected E_STATE for pending -> committed")
	}
	st, _ := sm.State("R1")
	if st != acoustics.StatePending {
		t.Fatalf("illegal transition changed state to %s", st)
	}
	for _, to := range []acoustics.State{acoustics.StateConnecting, acoustics.StateReflecting, acoustics.StateCommitted} {
		if err := sm.Transition("R1", to); err != nil {
			t.Fatalf("legal transition to %s failed: %v", to, err)
		}
	}
	if p := sm.Progress(); p.Committed != 1 || p.Total != 1 {
		t.Fatalf("bad progress: %+v", p)
	}
}

func TestReflectionDeterministicAcrossRepeatedCalls(t *testing.T) {
	l := testLevel(t)
	var first string
	for i := 0; i < 50; i++ {
		var b strings.Builder
		for _, rf := range l.Reflectors {
			if rf.RoomID != "R1" {
				continue
			}
			hit, err := acoustics.SolveReflection(l.Sources[0].Position, l.Listeners[0].Position, rf)
			if err != nil {
				t.Fatal(err)
			}
			if hit != nil {
				fmt.Fprintf(&b, "%s:%v:%v:%d;", hit.ReflectorID, hit.ReflectionPoint, hit.IncidenceAngleMicroRad, hit.PathLengthFixed)
			}
		}
		got := b.String()
		if i == 0 {
			first = got
			if got == "" {
				t.Fatal("expected at least one reflection")
			}
		} else if got != first {
			t.Fatalf("nondeterministic reflection result on iteration %d", i)
		}
	}
}

func TestPreciseInvalidationScopes(t *testing.T) {
	l := testLevel(t)
	alpha := 0.91
	changed, err := acoustics.ApplyEdit(l, acoustics.Edit{Type: "set_absorption", ObjectID: "RF-R1-B", Absorption: &alpha})
	if err != nil {
		t.Fatal(err)
	}
	plan := acoustics.PlanInvalidation(l, changed, nil)
	if !containsString(plan.ReusedRooms, "R2") || !containsString(plan.ReusedRooms, "R3") || !containsString(plan.ReusedRooms, "R4") {
		t.Fatalf("unexpected reused rooms: %+v", plan)
	}
	if !containsString(plan.InvalidRooms, "R1") {
		t.Fatalf("R1 must be invalid: %+v", plan.InvalidRooms)
	}

	deleted, err := acoustics.ApplyEdit(l, acoustics.Edit{Type: "delete_door", ObjectID: "D23"})
	if err != nil {
		t.Fatal(err)
	}
	doorPlan := acoustics.PlanInvalidation(l, deleted, nil)
	foundDoor := false
	foundChain := false
	for _, item := range doorPlan.Items {
		if item.Kind == "door" && item.ObjectID == "D23" {
			foundDoor = true
		}
		if item.Kind == "chain" && strings.Contains(item.ObjectID, "LIS-R3-A") {
			foundChain = true
		}
	}
	if !foundDoor || !foundChain {
		t.Fatalf("door deletion did not record door and downstream chain invalidation: %+v", doorPlan.Items)
	}
}

func TestCompareReportsLocatesFirstDiff(t *testing.T) {
	base := &acoustics.Report{
		Params:         acoustics.DefaultParams(),
		CommitSequence: []string{"R1"},
		Rooms:          []acoustics.RoomResult{{RoomID: "R1"}},
		Pairs: []acoustics.PairResult{{
			SourceID: "SRC1", ListenerID: "LIS1", ListenerRoomID: "R1", ValidPathCount: 1,
		}},
	}
	changed := &acoustics.Report{
		Params:         acoustics.DefaultParams(),
		CommitSequence: []string{"R1"},
		Rooms:          []acoustics.RoomResult{{RoomID: "R1"}},
		Pairs: []acoustics.PairResult{{
			SourceID: "SRC1", ListenerID: "LIS1", ListenerRoomID: "R1", ValidPathCount: 2,
		}},
	}
	err := acoustics.CompareReports(base, changed)
	de, ok := acoustics.AsDomainError(err)
	if !ok || de.Code != acoustics.CodeDiff || de.ObjectID != "R1" || de.Field != "valid_path_count" {
		t.Fatalf("expected object/field-scoped E_DIFF, got %#v", err)
	}
}

func TestPoolBufferExhaustionRecordsReason(t *testing.T) {
	pool := acoustics.NewPool(1, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	tasks := []acoustics.Task{
		{ID: "blocking", Fn: func() error { close(started); <-release; return nil }},
		{ID: "buffered", Fn: func() error { return nil }},
		{ID: "backpressured", Fn: func() error { return nil }},
	}
	done := make(chan []error, 1)
	go func() { done <- pool.Run(context.Background(), tasks) }()
	<-started
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(pool.QueueEvents()) == 0 {
		time.Sleep(time.Millisecond)
	}
	events := pool.QueueEvents()
	if len(events) == 0 || events[0].Reason != "buffer_full" {
		t.Fatalf("expected buffer_full queue event, got %+v", events)
	}
	close(release)
	errs := <-done
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	pool.Close()
}

func hasIssue(err error, code, objectSubstring, messageSubstring string) bool {
	var ve *acoustics.ValidationError
	if !errors.As(err, &ve) {
		return false
	}
	for _, is := range ve.Issues {
		if is.Code == code && strings.Contains(is.ObjectID, objectSubstring) && strings.Contains(is.Message, messageSubstring) {
			return true
		}
	}
	return false
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
