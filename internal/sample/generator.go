package sample

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"

	"screenrelay/internal/model"
	"screenrelay/internal/schema"
	"screenrelay/internal/topology"
	"screenrelay/internal/xconv"
)

const FixedSeed int64 = 20261127

func Generate(seed int64) (*model.SampleSet, error) {
	if seed == 0 {
		seed = FixedSeed
	}
	rng := rand.New(rand.NewSource(seed))
	allScales := []model.ScaleTier{model.Scale100, model.Scale125, model.Scale150, model.Scale200}
	allChromas := []model.Chroma{model.Chroma420, model.Chroma422, model.Chroma444}

	makeScreen := func(id string, rev int64, ox, oy, pw, ph int64, scale model.ScaleTier, chroma model.Chroma) model.Screen {
		return model.Screen{
			ID: id, Revision: rev,
			Origin: logicalPoint(ox, oy),
			PixelW: pw, PixelH: ph,
			Scale:            scale,
			SupportedScales:  append([]model.ScaleTier(nil), allScales...),
			Chroma:           chroma,
			SupportedChromas: append([]model.Chroma(nil), allChromas...),
		}
	}

	screenA := makeScreen("scr-a", 1, 0, 0, 1280, 720, model.Scale100, model.Chroma420)
	screenB := makeScreen("scr-b", 1, 1280, 0, 1920, 1080, model.Scale150, model.Chroma444)
	screenC := makeScreen("scr-c", 1, 0, 720, 2560, 1440, model.Scale200, model.Chroma422)
	screenD := makeScreen("scr-d", 1, 1280, 720, 1600, 1080, model.Scale125, model.Chroma420)
	screenE := makeScreen("scr-e", 1, 2560, 40, 1280, 720, model.Scale100, model.Chroma422)
	screenF := makeScreen("scr-f", 1, 3200, 760, 1280, 720, model.Scale200, model.Chroma444)
	screenCRev2 := screenC
	screenCRev2.Revision = 2
	screenCRev3 := screenC
	screenCRev3.Revision = 3
	screenDrev2 := screenD
	screenDrev2.Revision = 2
	screenDrev2.Scale = model.Scale150

	initial := []model.Screen{screenA, screenB, screenC, screenD}
	rng.Shuffle(len(initial), func(i, j int) { initial[i], initial[j] = initial[j], initial[i] })

	events := []model.TopologyEvent{
		mustEvent("evt-join-e", 1500, model.EventJoin, "scr-e", 1, &screenE),
		mustEvent("evt-f-join", 2000, model.EventJoin, "scr-f", 1, &screenF),
		mustEvent("evt-f-leave", 2000, model.EventLeave, "scr-f", 1, nil),
		mustEvent("evt-c-topo-old", 2500, model.EventTopology, "scr-c", 2, &screenCRev2),
		mustEvent("evt-c-topo-new", 2500, model.EventTopology, "scr-c", 3, &screenCRev3),
		mustEvent("evt-d-topo", 2500, model.EventTopology, "scr-d", 2, &screenDrev2),
		mustEvent("evt-c-leave", 3000, model.EventLeave, "scr-c", 4, nil),
	}
	for i := range events {
		events[i].Seq = i + 1
	}

	regions := []model.Rect{
		logicalRect(317, 271, 2411, 797),
		logicalRect(283, 307, 2473, 743),
		logicalRect(349, 251, 2381, 829),
		logicalRect(301, 329, 2459, 711),
		logicalRect(331, 289, 2417, 771),
		logicalRect(293, 311, 2477, 733),
	}
	windows := make([]model.WindowInstruction, 0, len(regions))
	for i, region := range regions {
		seq := i + 1
		ms := int64(500 * seq)
		tick, err := xconv.MillisecondsToTick(ms, "generator.window.ms")
		if err != nil {
			return nil, err
		}
		windows = append(windows, model.WindowInstruction{
			Seq: seq, ID: "window-" + string(rune('0'+seq)), Tick: tick, Region: region,
			MaxPacketBytes: 65536, ContentSeed: seed + int64(seq)*7919,
		})
	}

	project := &model.Project{
		SchemaMajor: 3, SourceVersion: "3.0", SessionID: "session-deterministic-" + hex.EncodeToString([]byte{byte(seed >> 24), byte(seed >> 16), byte(seed >> 8), byte(seed)}),
		Seed: seed, Master: logicalRect(0, 0, 3840, 1440),
		CoordBasis: model.CoordBasis{InternalUnitsPerLogical: model.InternalCoordUnit, Expression: "256 integer subunits per logical pixel"},
		TimeBases: map[string]model.TimeUnit{
			"ms":      {SourceName: "millisecond", SourceTicksPerSec: 1000, InternalPerSource: 90},
			"tick60":  {SourceName: "60hz-frame-tick", SourceTicksPerSec: 60, InternalPerSource: model.TicksPerSecond / 60},
			"tick90k": {SourceName: "90khz-pts", SourceTicksPerSec: model.TicksPerSecond, InternalPerSource: 1},
		},
		Screens: initial, Events: events, Windows: windows,
	}

	raw := map[string][]byte{}
	reports := map[string]*model.ConvertReport{}
	for _, major := range []int{1, 2, 3} {
		data, report, err := schema.Export(project, major)
		if err != nil {
			return nil, err
		}
		raw[versionKey(major)] = data
		reports[versionKey(major)] = report
	}

	truth := &model.TruthManifest{
		Seed: seed, FixedSeed: FixedSeed, Versions: []string{"v1", "v2", "v3"},
		ScreenIDs: []string{"scr-a", "scr-b", "scr-c", "scr-d", "scr-e", "scr-f"},
		EventIDs:  eventIDs(events), WindowIDs: windowIDs(windows),
		WindowCount: len(windows), EventCount: len(events), WindowTruths: buildWindowTruths(project),
		Checks: []model.TruthCheck{
			{Name: "three_schema_versions_load", Expect: "v1,v2,v3"},
			{Name: "initial_active_screens", Expect: "scr-a,scr-b,scr-c,scr-d"},
			{Name: "same_tick_join_leave", Expect: "evt-f-leave wins over evt-f-join"},
			{Name: "same_tick_topology_revision", Expect: "evt-c-topo-new rev3 wins over evt-c-topo-old rev2"},
			{Name: "join_replay", Expect: "scr-e replays window-2 in window-3"},
			{Name: "exit_stops_packets", Expect: "scr-c absent from window-6"},
			{Name: "three_paths", Expect: "realtime=recovery=full_replay"},
			{Name: "v1_lossy_enums", Expect: "125/150 scales and 4:2:2 chroma produce E_LOSSY items"},
		},
	}
	canonical, err := json.Marshal(project)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	truth.CanonicalHash = hex.EncodeToString(sum[:])

	return &model.SampleSet{Seed: seed, Project: project, Raw: raw, Reports: reports, Truth: truth}, nil
}

func buildWindowTruths(project *model.Project) []model.WindowTruth {
	state := topology.NewState(project.Screens)
	applied := map[string]bool{}
	out := make([]model.WindowTruth, 0, len(project.Windows))
	for i, window := range project.Windows {
		pending := topology.PendingThrough(project.Events, window.Tick, applied)
		_, winners, err := topology.Adjudicate(pending)
		if err != nil {
			panic(err)
		}
		replayIDs := []string{}
		winnerIDs := make([]string, 0, len(winners))
		for _, winner := range winners {
			winnerIDs = append(winnerIDs, winner.ID)
			if err := state.Apply(winner); err != nil {
				panic(err)
			}
			if winner.Kind == model.EventJoin && i > 0 {
				replayIDs = append(replayIDs, winner.ScreenID)
			}
		}
		topology.MarkApplied(applied, pending...)
		out = append(out, model.WindowTruth{
			Seq: window.Seq, Tick: window.Tick, ActiveScreenIDs: state.IDs(),
			WinnerEventIDs: winnerIDs, ReplayScreenIDs: replayIDs,
		})
	}
	return out
}

func logicalPoint(x, y int64) model.Point {
	return model.Point{X: x * model.InternalCoordUnit, Y: y * model.InternalCoordUnit}
}

func logicalRect(x, y, w, h int64) model.Rect {
	return model.Rect{X: x * model.InternalCoordUnit, Y: y * model.InternalCoordUnit, W: w * model.InternalCoordUnit, H: h * model.InternalCoordUnit}
}

func mustEvent(id string, ms int64, kind model.EventKind, screenID string, rev int64, screen *model.Screen) model.TopologyEvent {
	tick, err := xconv.MillisecondsToTick(ms, "generator.event.ms")
	if err != nil {
		panic(err)
	}
	return model.TopologyEvent{ID: id, Tick: tick, Kind: kind, ScreenID: screenID, Revision: rev, Screen: screen}
}

func versionKey(major int) string {
	switch major {
	case 1:
		return "v1"
	case 2:
		return "v2"
	default:
		return "v3"
	}
}

func eventIDs(events []model.TopologyEvent) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.ID)
	}
	return out
}

func windowIDs(windows []model.WindowInstruction) []string {
	out := make([]string, 0, len(windows))
	for _, window := range windows {
		out = append(out, window.ID)
	}
	return out
}

func V1MissingDefaultsFixture(sample *model.SampleSet) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(sample.Raw["v1"], &doc); err != nil {
		return nil, err
	}
	panels := doc["panels"].([]any)
	panel := panels[0].(map[string]any)
	delete(panel, "zoom")
	delete(panel, "color")
	panels[0] = panel
	doc["panels"] = panels
	return json.MarshalIndent(doc, "", "  ")
}
