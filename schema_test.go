package grid

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestThreeSchemaExportDecodeRoundTrip(t *testing.T) {
	level := Generate(DefaultSeed)
	for _, version := range []int{1, 2, 3} {
		art, err := ExportVersion(level, version)
		if err != nil {
			t.Fatalf("export v%d: %v", version, err)
		}
		if err := VerifyExportRoundTrip(art); err != nil {
			t.Fatalf("v%d round trip: %v", version, err)
		}
		decoded, report, err := DecodeVersion(art.Data)
		if err != nil {
			t.Fatalf("decode v%d: %v", version, err)
		}
		if diff := FirstContentDifference(level, decoded); diff != nil {
			t.Fatalf("seed v%d content round trip: %v", version, diff)
		}
		if decoded.Seed != DefaultSeed {
			t.Fatalf("v%d seed = %d", version, decoded.Seed)
		}
		if len(decoded.Obstacles) != 7 || len(decoded.Launches) != 5 {
			t.Fatalf("v%d counts = %d/%d", version, len(decoded.Obstacles), len(decoded.Launches))
		}
		if version == 1 && len(report.Fills) != 14 {
			t.Fatalf("v1 fills = %d, want 14 normal+penetration", len(report.Fills))
		}
	}
}

func TestSchemaSpecificIntegerDialects(t *testing.T) {
	level := Generate(DefaultSeed)
	v1, err := ExportVersion(level, 1)
	if err != nil {
		t.Fatal(err)
	}
	var doc1 map[string]any
	if err := json.Unmarshal(v1.Data, &doc1); err != nil {
		t.Fatal(err)
	}
	if doc1["time_step_tick"].(float64) != 2 {
		t.Fatalf("v1 tick = %v", doc1["time_step_tick"])
	}
	ob01 := doc1["obstacles"].([]any)[0].(map[string]any)
	if ob01["restitution_pm"].(float64) != 850 {
		t.Fatalf("v1 restitution = %v", ob01["restitution_pm"])
	}

	v3, err := ExportVersion(level, 3)
	if err != nil {
		t.Fatal(err)
	}
	var doc3 struct {
		Barriers []struct {
			Key         string `json:"key"`
			AnchorDelta struct {
				X int64 `json:"x"`
			} `json:"anchorDelta"`
			BounceBPS int64 `json:"bounceBasisPoints"`
		} `json:"barriers"`
	}
	if err := json.Unmarshal(v3.Data, &doc3); err != nil {
		t.Fatal(err)
	}
	if doc3.Barriers[1].AnchorDelta.X != 0 || doc3.Barriers[0].BounceBPS != 8500 {
		t.Fatalf("unexpected v3 dialect: %+v", doc3.Barriers)
	}
}

func TestDecodeMissingDuplicateAndNonUnit(t *testing.T) {
	level := Generate(DefaultSeed)
	art, err := ExportVersion(level, 1)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(art.Data, &root); err != nil {
		t.Fatal(err)
	}
	obs := root["obstacles"].([]any)
	delete(obs[0].(map[string]any), "obstacle_id")
	root["obstacles"] = obs
	data, _ := json.Marshal(root)
	_, _, err = DecodeVersion(data)
	ge, ok := AsError(err, CodeDecode)
	if !ok || !strings.Contains(ge.Field, "obstacle_id") {
		t.Fatalf("missing id error = %#v", err)
	}

	dup := InjectDuplicateFault(level)
	if dup == nil || dup.Code != CodeDecode || dup.ObjectID != "OB01" {
		t.Fatalf("duplicate fault = %#v", dup)
	}

	v2, err := ExportVersion(level, 2)
	if err != nil {
		t.Fatal(err)
	}
	var badNormal map[string]any
	_ = json.Unmarshal(v2.Data, &badNormal)
	list := badNormal["obstacleList"].([]any)
	list[0].(map[string]any)["normal"] = map[string]int64{"x": 1000, "y": 1000}
	badNormal["obstacleList"] = list
	bad, _ := json.Marshal(badNormal)
	_, _, err = DecodeVersion(bad)
	ge, ok = AsError(err, CodeDecode)
	if !ok || ge.ObjectID != "OB01" || !strings.Contains(ge.Field, "normal") {
		t.Fatalf("non-unit normal error = %#v", err)
	}
}

func TestIntegerScaleRoundTripAndOverflow(t *testing.T) {
	for _, bps := range []int64{6000, 7500, 8500} {
		pm := BPSToPermille("OB", "f", bps, nil)
		back, err := PermilleToBPS("OB", "f", pm)
		if err != nil || back != bps {
			t.Fatalf("coefficient round trip %d -> %d -> %d err=%v", bps, pm, back, err)
		}
	}
	if ms, err := TicksToMS("OB", "f", MSToTicks("OB", "f", 20, nil)); err != nil || ms != 20 {
		t.Fatalf("time round trip ms=%d err=%v", ms, err)
	}
	_, err := MMToUnits("OB09", "obstacles[9].end_mm.x_mm", math.MaxInt64)
	ge, ok := AsError(err, CodeOVF)
	if !ok || ge.ObjectID != "OB09" || ge.Field != "obstacles[9].end_mm.x_mm" {
		t.Fatalf("overflow error = %#v", err)
	}
	if ge.Operands[0] != math.MaxInt64 || ge.Operands[1] != PositionScale {
		t.Fatalf("overflow operands = %v", ge.Operands)
	}

	var traces []FieldTrace
	pm := BPSToPermille("OB", "restitution", 8505, &traces)
	if pm != 850 || len(traces) != 1 || traces[0].Remainder != 5 {
		t.Fatalf("truncation traces = %d %+v", pm, traces)
	}
}

func TestLossyOriginalRoundTripReportsERound(t *testing.T) {
	level := Generate(DefaultSeed)
	level.ObstacleByID("OB07").Penetrable = true
	art, err := ExportVersion(level, 2)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := DecodeVersion(art.Data)
	if err != nil {
		t.Fatal(err)
	}
	ge := FirstContentDifference(level, decoded)
	if ge == nil || ge.Code != CodeRound || !strings.Contains(ge.Field, "allowsPenetration") {
		t.Fatalf("expected E_ROUND penetration field, got %#v", ge)
	}
	if err := VerifyExportRoundTrip(art); err != nil {
		t.Fatalf("normalized artifact should round trip: %v", err)
	}
}
