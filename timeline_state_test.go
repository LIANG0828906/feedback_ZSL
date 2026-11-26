package mirror

import (
	"reflect"
	"testing"
)

func TestArbitrationPrioritizesLeaveAndRecordsWinner(t *testing.T) {
	events := []Event{
		{ID: "join-a", Kind: EventJoin, ScreenID: "s", Domain: "d", AtTick: 10},
		{ID: "leave-a", Kind: EventLeave, ScreenID: "s", AtTick: 10},
		{ID: "join-b", Kind: EventJoin, ScreenID: "other", Domain: "d", AtTick: 10},
	}
	applied, records, err := ArbitrateEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied = %d, want 2", len(applied))
	}
	if applied[0].ID != "leave-a" || applied[1].ID != "join-b" {
		t.Fatalf("unexpected applied order: %+v", applied)
	}
	var joinRecord, leaveRecord *ArbitrationRecord
	for i := range records {
		switch records[i].EventID {
		case "join-a":
			joinRecord = &records[i]
		case "leave-a":
			leaveRecord = &records[i]
		}
	}
	if joinRecord == nil || leaveRecord == nil {
		t.Fatal("missing arbitration records")
	}
	if joinRecord.Outcome != "suppressed" || joinRecord.WinnerID != "leave-a" {
		t.Fatalf("join arbitration = %+v", joinRecord)
	}
	if len(leaveRecord.SuppressedIDs) != 1 || leaveRecord.SuppressedIDs[0] != "join-a" {
		t.Fatalf("leave suppressed = %+v", leaveRecord.SuppressedIDs)
	}
}

func TestArbitrationDuplicateIDReturnsARB(t *testing.T) {
	_, _, err := ArbitrateEvents([]Event{
		{ID: "dup", Kind: EventJoin, ScreenID: "s", AtTick: 5},
		{ID: "dup", Kind: EventLeave, ScreenID: "s", AtTick: 5},
	})
	if !IsCode(err, CodeArbitration) {
		t.Fatalf("error = %v, want E_ARB", err)
	}
	e, _ := AsError(err)
	if e.Tick != 5 || len(e.Events) != 2 {
		t.Fatalf("E_ARB context = %+v", e)
	}
}

func TestTimelineJoinStaysPendingUntilNegotiation(t *testing.T) {
	s := &Session{
		ID: "s", SourceVersion: SchemaV3, TickMillis: 10,
		MasterScreenID: "a",
		Domains:        []Domain{{ID: "d"}},
		Screens: []Screen{{
			ID: "a", Name: "a", Domain: "d", Master: true, Width: 100, Height: 100,
			ScaleCapabilities: []Scale{100}, DefaultScale: 100,
			ChromaCapabilities: []Chroma{Chroma444}, DefaultChroma: Chroma444, MaxPacketBytes: 100,
		}},
		Events: []Event{
			{ID: "j", Kind: EventJoin, ScreenID: "a", Domain: "d", AtTick: 0},
			{ID: "n", Kind: EventNegotiate, ScreenID: "a", AtTick: 5},
		},
	}
	normalizeForTest(t, s)
	timeline := NewTimeline(s)
	if err := timeline.AdvanceTo(2); err != nil {
		t.Fatal(err)
	}
	if got := timeline.EligibleScreens(); len(got) != 0 {
		t.Fatalf("pending screen became eligible: %+v", got)
	}
	snap := timeline.Snapshot("w", 2)
	if len(snap.Endpoints) != 1 || snap.Endpoints[0].Status != ScreenPending {
		t.Fatalf("snapshot = %+v", snap)
	}
	if err := timeline.AdvanceTo(5); err != nil {
		t.Fatal(err)
	}
	if got := timeline.EligibleScreens(); len(got) != 1 {
		t.Fatalf("negotiated screen not eligible: %+v", got)
	}
}

func TestTimelineLeaveInvalidatesInFlightWindow(t *testing.T) {
	s := &Session{
		ID: "s", SourceVersion: SchemaV3, TickMillis: 10,
		MasterScreenID: "a",
		Domains:        []Domain{{ID: "d"}},
		Screens: []Screen{{
			ID: "a", Name: "a", Domain: "d", Master: true, Width: 100, Height: 100,
			ScaleCapabilities: []Scale{100}, DefaultScale: 100,
			ChromaCapabilities: []Chroma{Chroma444}, DefaultChroma: Chroma444, MaxPacketBytes: 100,
		}},
		Events: []Event{
			{ID: "j", Kind: EventJoin, ScreenID: "a", Domain: "d", AtTick: 0},
			{ID: "n", Kind: EventNegotiate, ScreenID: "a", AtTick: 1},
			{ID: "l", Kind: EventLeave, ScreenID: "a", AtTick: 5},
		},
		Windows: []Window{{ID: "w", Index: 0, StartTick: 0, EndTick: 10,
			Region: Region{X: 0, Y: 0, W: 10, H: 10}, Scale: 100, Chroma: Chroma444, MaxPacketBytes: 100}},
	}
	normalizeForTest(t, s)
	timeline := NewTimeline(s)
	if err := timeline.AdvanceTo(10); err != nil {
		t.Fatal(err)
	}
	if _, ok := timeline.InvalidationFor("a", "w"); !ok {
		t.Fatal("in-flight window was not invalidated")
	}
	if len(timeline.EligibleScreens()) != 0 {
		t.Fatal("left screen remains in endpoint set")
	}
}

func TestStateMachineIllegalTransitionPreservesStateAndProgress(t *testing.T) {
	s := &Session{Screens: []Screen{{ID: "a"}}, Windows: []Window{{ID: "w"}}}
	sm := NewStateMachine(s)
	if err := sm.TransitionScreen("a", ScreenCommitted); !IsCode(err, CodeState) {
		t.Fatalf("illegal screen transition = %v", err)
	}
	if got, _ := sm.ScreenState("a"); got != ScreenPending {
		t.Fatalf("screen state changed to %s", got)
	}
	if err := sm.TransitionSession(SessionEnded); err != nil {
		t.Fatal(err)
	}
	if err := sm.TransitionSession(SessionMirroring); !IsCode(err, CodeState) {
		t.Fatalf("illegal session transition = %v", err)
	}
	if got := sm.SessionState(); got != SessionEnded {
		t.Fatalf("session state = %s, want ended", got)
	}
	if got := sm.Progress(); got != 0 {
		t.Fatalf("progress = %d, want 0", got)
	}

	sm2 := NewStateMachine(s)
	if err := sm2.TransitionWindow("w", WindowDistributing); err != nil {
		t.Fatal(err)
	}
	if err := sm2.TransitionWindow("w", WindowCommitted); err != nil {
		t.Fatal(err)
	}
	if err := sm2.TransitionWindow("w", WindowFailed); !IsCode(err, CodeState) {
		t.Fatalf("illegal window transition = %v", err)
	}
	if got := sm2.Progress(); got != 1 {
		t.Fatalf("progress = %d, want 1", got)
	}
}

func normalizeForTest(t *testing.T, s *Session) {
	t.Helper()
	if s.AlgorithmVersion == "" {
		s.AlgorithmVersion = AlgorithmVersion
	}
	if s.TickMillis == 0 {
		s.TickMillis = 10
	}
	if len(s.Screens) > 0 && s.Screens[0].Name == "" {
		s.Screens[0].Name = s.Screens[0].ID
	}
	if err := NormalizeSession(s); err != nil {
		t.Fatal(err)
	}
}

func TestMappingResultIndependentOfScreenOrder(t *testing.T) {
	a := testScreen()
	b := a
	b.ID = "screen-z"
	window := testWindow(Scale125, Chroma420)
	ma, err := MapScreen(a, window)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := MapScreen(b, window)
	if err != nil {
		t.Fatal(err)
	}
	ma.ScreenID, mb.ScreenID = "", ""
	ma.MappingSignature, mb.MappingSignature = "", ""
	if !reflect.DeepEqual(ma, mb) {
		t.Fatalf("mapping differs by screen order/identity: %+v != %+v", ma, mb)
	}
}
