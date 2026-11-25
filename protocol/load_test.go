package protocol_test

import (
	"strings"
	"testing"

	"example.com/multiscreen-cast/protocol"
	"example.com/multiscreen-cast/samplegen"
)

func sampleSession(t *testing.T) *protocol.Session {
	t.Helper()
	set, err := samplegen.Generate(samplegen.Options{Seed: 7, Screens: 3, Windows: 6, Sessions: 1})
	if err != nil {
		t.Fatalf("generate sample: %v", err)
	}
	return &set.Sessions[0]
}

func TestValidateGeneratedSession(t *testing.T) {
	session := sampleSession(t)
	if err := protocol.ValidateSession(session, "test", "$"); err != nil {
		t.Fatalf("valid session rejected: %v", err)
	}
}

func TestValidationErrorsAreLocated(t *testing.T) {
	tests := []struct {
		name   string
		code   protocol.Code
		mutate func(*protocol.Session)
		field  string
	}{
		{
			name: "missing session id",
			code: protocol.CodeSchema,
			mutate: func(session *protocol.Session) {
				session.ID = ""
			},
			field: "session_id",
		},
		{
			name: "duplicate endpoint",
			code: protocol.CodeSchema,
			mutate: func(session *protocol.Session) {
				session.Displays[1].ID = session.Displays[0].ID
			},
			field: "id",
		},
		{
			name: "unknown initial display",
			code: protocol.CodeRef,
			mutate: func(session *protocol.Session) {
				session.InitialDisplays[0] = "missing"
			},
			field: "initial_displays",
		},
		{
			name: "capability resolution mismatch",
			code: protocol.CodeCap,
			mutate: func(session *protocol.Session) {
				session.Displays[0].Capability.Resolution.Width++
			},
			field: "capability.resolution",
		},
		{
			name: "capability scale missing",
			code: protocol.CodeSchema,
			mutate: func(session *protocol.Session) {
				session.Displays[0].Capability.Scale = protocol.Ratio{}
			},
			field: "capability.scale",
		},
		{
			name: "event time reversal",
			code: protocol.CodeOrder,
			mutate: func(session *protocol.Session) {
				session.Events[0].Time = 10
				session.Events[1].Time = 1
			},
			field: "time",
		},
		{
			name: "target not joined",
			code: protocol.CodeRef,
			mutate: func(session *protocol.Session) {
				session.Windows[0].Targets = []string{session.Displays[1].ID}
			},
			field: "targets",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := sampleSession(t)
			test.mutate(session)
			err := protocol.ValidateSession(session, "sample.json", "$")
			if err == nil {
				t.Fatalf("expected %s, got nil", test.code)
			}
			if err.Code != test.code {
				t.Fatalf("expected %s, got %s (%v)", test.code, err.Code, err)
			}
			if err.Source != "sample.json" || err.Path == "" {
				t.Fatalf("error is not located: source=%q path=%q", err.Source, err.Path)
			}
			if !strings.Contains(err.Path, test.field) {
				t.Fatalf("path %q does not contain %q", err.Path, test.field)
			}
		})
	}
}

func TestDecodeError(t *testing.T) {
	_, err := protocol.DecodeSampleSet(strings.NewReader(`{"sessions":[`), "broken.json")
	if err == nil || err.Code != protocol.CodeDecode {
		t.Fatalf("expected E_DECODE, got %v", err)
	}
	if err.Source != "broken.json" {
		t.Fatalf("unexpected source %q", err.Source)
	}
}

func TestRatioJSON(t *testing.T) {
	var ratio protocol.Ratio
	if err := ratio.UnmarshalJSON([]byte(`"3/2"`)); err != nil {
		t.Fatal(err)
	}
	if ratio.Num != 3 || ratio.Den != 2 {
		t.Fatalf("unexpected ratio: %+v", ratio)
	}
	if err := ratio.UnmarshalJSON([]byte(`1.5`)); err != nil {
		t.Fatal(err)
	}
	if ratio.Num != 3 || ratio.Den != 2 {
		t.Fatalf("unexpected decimal ratio: %+v", ratio)
	}
}

func TestTopologyCapabilityInheritanceAcrossEvents(t *testing.T) {
	session := sampleSession(t)
	displayID := session.Displays[1].ID
	resolution := protocol.Resolution{Width: 960, Height: 540}
	scale := protocol.NewRatio(1, 4)
	first := protocol.Topology{
		X: 0, Y: 0, Width: 3840, Height: 2160,
		Resolution: resolution,
		Scale:      scale,
	}
	second := first
	second.X = 32
	second.Y = 16
	capability := protocol.Capability{Resolution: resolution, Scale: scale, CropMode: protocol.CropFloor}
	session.InitialDisplays = []string{session.Displays[0].ID}
	session.Events = []protocol.Event{
		{Seq: 1, Time: 0, Kind: protocol.EventNegotiate},
		{Seq: 2, Time: 1, Kind: protocol.EventStart},
		{Seq: 3, Time: 5, Kind: protocol.EventJoin, DisplayID: displayID},
		{Seq: 4, Time: 6, Kind: protocol.EventTopology, DisplayID: displayID, Topology: &first, Capability: &capability},
		{Seq: 5, Time: 7, Kind: protocol.EventTopology, DisplayID: displayID, Topology: &second},
		{Seq: 6, Time: 30, Kind: protocol.EventEnd},
	}
	session.Windows = session.Windows[:1]
	session.Windows[0].Index = 1
	session.Windows[0].Time = 10
	session.Windows[0].Targets = []string{session.Displays[0].ID}
	session.TotalWindows = 1
	if err := protocol.ValidateSession(session, "test", "$"); err != nil {
		t.Fatalf("chained topology update rejected: %v", err)
	}
}
