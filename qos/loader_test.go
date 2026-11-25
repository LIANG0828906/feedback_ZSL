package qos

import (
	"bytes"
	"context"
	"reflect"
	"testing"
)

func TestThreeSchemaLoaderNormalizesEquivalentContent(t *testing.T) {
	base := Scenario(42)
	base.SchemaVersion = 1
	baseDigest, err := base.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	var first *Project
	for version := 1; version <= 3; version++ {
		raw, err := EncodeProject(base, version)
		if err != nil {
			t.Fatalf("encode v%d: %v", version, err)
		}
		sourceCopy := append([]byte(nil), raw...)
		project, err := LoadProject(raw)
		if err != nil {
			t.Fatalf("load v%d: %v", version, err)
		}
		if !reflect.DeepEqual(raw, sourceCopy) {
			t.Fatalf("loader mutated v%d source bytes", version)
		}
		digest, _ := project.CanonicalDigest()
		if digest != baseDigest {
			t.Fatalf("v%d digest mismatch: %s != %s", version, digest, baseDigest)
		}
		if first == nil {
			first = project
		} else {
			assertNormalizedChannels(t, first, project, version)
			if !reflect.DeepEqual(first.Packets, project.Packets) {
				t.Fatalf("v%d packet stream mismatch", version)
			}
		}
		if project.SchemaVersion != version {
			t.Fatalf("schema version not retained")
		}
	}

	eng, err := NewEngine(first)
	if err != nil {
		t.Fatal(err)
	}
	result0, err := eng.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Re-load after execution; engine uses a defensive clone.
	for version := 1; version <= 3; version++ {
		raw, _ := EncodeProject(base, version)
		project, err := LoadProject(raw)
		if err != nil {
			t.Fatal(err)
		}
		eng, _ := NewEngine(project)
		result, err := eng.Run(context.Background())
		if err != nil {
			t.Fatalf("v%d run: %v", version, err)
		}
		if !stepsEquivalent(result0.Steps, result.Steps) {
			t.Fatalf("v%d per-step decisions differ", version)
		}
		if !reflect.DeepEqual(result0.Emissions, result.Emissions) {
			t.Fatalf("v%d emitted sequence differs", version)
		}
		if !reflect.DeepEqual(result0.FinalTokens, result.FinalTokens) {
			t.Fatalf("v%d final tokens differ", version)
		}
		truthRaw, err := EncodeTruth(project, result)
		if err != nil {
			t.Fatal(err)
		}
		truth, err := DecodeTruth(truthRaw, project)
		if err != nil {
			t.Fatalf("v%d truth decode: %v", version, err)
		}
		if !stepsEquivalent(truth.Steps, result.Steps) {
			t.Fatalf("v%d truth mismatch", version)
		}
	}
}

func assertNormalizedChannels(t *testing.T, a, b *Project, version int) {
	t.Helper()
	if !reflect.DeepEqual(a.ChannelOrder(), b.ChannelOrder()) {
		t.Fatalf("v%d channel order differs: %v != %v", version, a.ChannelOrder(), b.ChannelOrder())
	}
	for i := range a.Channels {
		x, y := a.Channels[i], b.Channels[i]
		if !reflect.DeepEqual(x, y) {
			t.Fatalf("v%d normalized channel %s differs:\n%#v\n%#v", version, x.ID, x, y)
		}
	}
}

func TestLoaderDeclaredUnitsAndTimeBases(t *testing.T) {
	base := Scenario(42)
	base.SchemaVersion = 1
	markers := map[int][]string{
		1: {"rate_permille", "burst_packets", "arrival_tick", "queue_age_ticks"},
		2: {"arrival_stream", "permyriad", "arrival_ms", "priority_boost"},
		3: {"shaping_table", "rate_value", "rate_scale", "remaining_ms", "at_ms"},
	}
	for version := 1; version <= 3; version++ {
		raw, _ := EncodeProject(base, version)
		for _, marker := range markers[version] {
			if !containsSubstring(raw, marker) {
				t.Fatalf("v%d source does not expose marker %q", version, marker)
			}
		}
		p, err := LoadProject(raw)
		if err != nil {
			t.Fatal(err)
		}
		if p.Channels[0].RatePermyriad != 60000 {
			t.Fatalf("v%d canonical rate=%d", version, p.Channels[0].RatePermyriad)
		}
		if p.Packets[0].ArrivalMS != 0 || p.Packets[3].ArrivalMS != 10 {
			t.Fatalf("v%d time normalization failed", version)
		}
	}
}

func containsSubstring(raw []byte, s string) bool {
	return bytes.Contains(raw, []byte(s))
}

func TestFieldConflictHasLocation(t *testing.T) {
	p := Scenario(42)
	p.SchemaVersion = 1
	raw, err := EncodeProject(p, 1)
	if err != nil {
		t.Fatal(err)
	}
	conflict, err := mutateV1(raw, func(project map[string]any) error {
		firstChannel(project)["rate_permyriad"] = 123
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadProject(conflict)
	if CodeOf(err) != EFieldConflict {
		t.Fatalf("code=%s err=%v", CodeOf(err), err)
	}
	qe := err.(*Error)
	if qe.Path == "" || qe.Context == nil {
		t.Fatalf("conflict error lacks location/context: %#v", qe)
	}
}
