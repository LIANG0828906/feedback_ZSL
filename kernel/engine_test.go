package kernel

import (
	"context"
	"testing"

	"example.com/multiscreen-cast/protocol"
	"example.com/multiscreen-cast/samplegen"
)

func generatedSession(t *testing.T, windows int) *protocol.Session {
	t.Helper()
	set, err := samplegen.Generate(samplegen.Options{Seed: 19, Screens: 4, Windows: windows, Sessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	return &set.Sessions[0]
}

func TestIllegalInstructionIsNotConsumed(t *testing.T) {
	session := generatedSession(t, 1)
	session.Events = []protocol.Event{
		{Seq: 1, Time: 0, Kind: protocol.EventNegotiate},
		{Seq: 2, Time: 1, Kind: protocol.EventStart},
		{Seq: 3, Time: 5, Kind: protocol.EventPause},
		{Seq: 4, Time: 15, Kind: protocol.EventResume},
		{Seq: 5, Time: 16, Kind: protocol.EventEnd},
	}
	session.Windows[0].Time = 10
	session.Windows[0].Targets = []string{session.InitialDisplays[0]}
	engine, err := NewEngine(session, Config{Mapping: DefaultMappingConfig()})
	if err != nil {
		t.Fatal(err)
	}
	for engine.State() != protocol.StatePaused {
		if _, advanceErr := engine.Advance(context.Background()); advanceErr != nil {
			t.Fatal(advanceErr)
		}
	}
	before := engine.Progress()
	_, stateErr := engine.Advance(context.Background())
	if stateErr == nil || stateErr.Code != protocol.CodeState {
		t.Fatalf("expected E_STATE, got %v", stateErr)
	}
	if engine.State() != protocol.StatePaused {
		t.Fatalf("state changed after rejected instruction: %s", engine.State())
	}
	if engine.Progress() != before {
		t.Fatalf("progress consumed rejected instruction: before=%+v after=%+v", before, engine.Progress())
	}
	_, repeatErr := engine.Advance(context.Background())
	if repeatErr == nil || repeatErr.Code != protocol.CodeState {
		t.Fatalf("expected same instruction to remain pending, got %v", repeatErr)
	}
}

func TestEndedSessionRejectsFurtherInstructions(t *testing.T) {
	session := generatedSession(t, 1)
	session.Events = []protocol.Event{
		{Seq: 1, Time: 0, Kind: protocol.EventNegotiate},
		{Seq: 2, Time: 1, Kind: protocol.EventStart},
		{Seq: 3, Time: 5, Kind: protocol.EventEnd},
	}
	session.Windows[0].Time = 10
	session.Windows[0].Targets = []string{session.InitialDisplays[0]}
	engine, err := NewEngine(session, Config{Mapping: DefaultMappingConfig()})
	if err != nil {
		t.Fatal(err)
	}
	for !engine.Done() && engine.State() != protocol.StateEnded {
		if _, advanceErr := engine.Advance(context.Background()); advanceErr != nil {
			t.Fatal(advanceErr)
		}
	}
	_, stateErr := engine.Advance(context.Background())
	if stateErr == nil || stateErr.Code != protocol.CodeState || stateErr.State != string(protocol.StateEnded) {
		t.Fatalf("expected ended E_STATE, got %v", stateErr)
	}
	if engine.Progress().Committed != 0 {
		t.Fatalf("unexpected committed progress %+v", engine.Progress())
	}
}

func TestJoinReplaysLatestCommittedFrameAndAdjudicates(t *testing.T) {
	session := generatedSession(t, 8)
	engine, err := NewEngine(session, Config{Mapping: DefaultMappingConfig()})
	if err != nil {
		t.Fatal(err)
	}
	var joinEffect *EventEffect
	for {
		step, advanceErr := engine.Advance(context.Background())
		if advanceErr != nil {
			t.Fatal(advanceErr)
		}
		if step == nil {
			break
		}
		if step.Event != nil && step.Event.Kind == protocol.EventJoin && step.Event.Time == 15 {
			copyEffect := *step.Event
			joinEffect = &copyEffect
			break
		}
	}
	if joinEffect == nil {
		t.Fatal("concurrent join effect not found")
	}
	if joinEffect.Replay == nil || joinEffect.ReplayWindow != 1 {
		t.Fatalf("join did not replay latest committed frame: %+v", joinEffect)
	}
	trace := engine.Trace()
	found := false
	for _, adjudication := range trace.Adjudications {
		if adjudication.Time == 15 && len(adjudication.Items) >= 2 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing same-time adjudication details: %+v", trace.Adjudications)
	}
}

func TestSameDisplayConcurrentOperationsConflict(t *testing.T) {
	session := generatedSession(t, 8)
	for i := range session.Events {
		if session.Events[i].Time == 15 && session.Events[i].Kind == protocol.EventJoin {
			session.Events[i].DisplayID = session.InitialDisplays[0]
			break
		}
	}
	removed := session.Displays[1].ID
	for i := range session.Windows {
		filtered := session.Windows[i].Targets[:0]
		for _, target := range session.Windows[i].Targets {
			if target != removed {
				filtered = append(filtered, target)
			}
		}
		session.Windows[i].Targets = filtered
	}
	engine, err := NewEngine(session, Config{Mapping: DefaultMappingConfig()})
	if err != nil {
		t.Fatal(err)
	}
	var conflict *protocol.KernelError
	for !engine.Done() {
		_, advanceErr := engine.Advance(context.Background())
		if advanceErr != nil {
			conflict = advanceErr
			break
		}
	}
	if conflict == nil || conflict.Code != protocol.CodeConflict {
		t.Fatalf("expected E_CONFLICT, got %v", conflict)
	}
	if conflict.Time == nil || *conflict.Time != 15 || conflict.DisplayID == "" {
		t.Fatalf("conflict is not located: %+v", conflict)
	}
}

func TestSegmentFailureRetryMatchesCleanRun(t *testing.T) {
	session := generatedSession(t, 8)
	cleanEngine, err := NewEngine(session, Config{Mapping: DefaultMappingConfig()})
	if err != nil {
		t.Fatal(err)
	}
	cleanTrace, err := cleanEngine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	failedEngine, err := NewEngine(session, Config{
		Mapping: DefaultMappingConfig(),
		FailureInjector: func(window int) (string, string, bool) {
			if window == 2 {
				return session.Displays[1].ID, "injected", true
			}
			return "", "", false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var failure *protocol.KernelError
	for !failedEngine.Done() {
		_, advanceErr := failedEngine.Advance(context.Background())
		if advanceErr != nil {
			failure = advanceErr
			break
		}
	}
	if failure == nil || failure.Code != protocol.CodeSeg || failure.Window != 2 || failure.DisplayID == "" {
		t.Fatalf("expected located E_SEG, got %v", failure)
	}
	if failedEngine.Progress().Committed != 1 {
		t.Fatalf("failed window polluted progress: %+v", failedEngine.Progress())
	}
	failedEngine.SetFailureInjector(nil)
	retriedTrace, err := failedEngine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if diff := CompareTraces(*cleanTrace, *retriedTrace); diff != nil {
		t.Fatalf("retry mismatch: %v", diff)
	}
	if len(failedEngine.Failures()) != 1 {
		t.Fatalf("expected one isolated failure record, got %+v", failedEngine.Failures())
	}
}

func TestCompareTracesLocatesFirstWindowScreenAndField(t *testing.T) {
	session := generatedSession(t, 6)
	engine, err := NewEngine(session, Config{Mapping: DefaultMappingConfig()})
	if err != nil {
		t.Fatal(err)
	}
	trace, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Windows) < 3 || len(trace.Windows[2].Screens) == 0 {
		t.Fatal("sample does not provide enough screens for comparison test")
	}
	altered := cloneTrace(*trace)
	altered.Windows[2].Screens[0].Pixel.X++
	diff := CompareTraces(*trace, altered)
	if diff == nil || diff.Code != protocol.CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", diff)
	}
	if diff.Window != 3 || diff.DisplayID == "" || diff.Field != "pixel.x" {
		t.Fatalf("difference is not precisely located: %+v", diff)
	}
}
