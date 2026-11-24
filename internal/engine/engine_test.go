package engine_test

import (
	"testing"

	"github.com/gameaudio/workbench/internal/engine"
	"github.com/gameaudio/workbench/internal/generator"
	"github.com/gameaudio/workbench/internal/model"
)

func TestComputePairMatchesGoldenTruth(t *testing.T) {
	p := generator.Generate()
	eng, err := engine.New(p, model.GeoConfig{ScaleBits: generator.DefaultScaleBits})
	if err != nil {
		t.Fatal(err)
	}
	truth := map[string]model.Truth{}
	for _, tr := range p.Truths {
		truth[tr.SourceID+"->"+tr.ListenerID] = tr
	}
	for _, s := range p.Sources {
		for _, l := range p.Listeners {
			r := eng.ComputePair(s.ID, l.ID)
			if r.Failed {
				t.Fatalf("pair %s unexpectedly failed: %s",
					engine.PairKey(s.ID, l.ID), r.ErrorCode)
			}
			want := truth[engine.PairKey(s.ID, l.ID)]
			if r.Occluded != want.Occluded {
				t.Fatalf("occlusion mismatch %s", engine.PairKey(s.ID, l.ID))
			}
			if r.Occluded && r.DiffractionLen != want.DiffractionLen {
				t.Fatalf("diffraction mismatch %s: %.6f vs %.6f",
					engine.PairKey(s.ID, l.ID), r.DiffractionLen, want.DiffractionLen)
			}
			if r.SPL != want.SPL {
				t.Fatalf("SPL mismatch %s", engine.PairKey(s.ID, l.ID))
			}
		}
	}
}

func TestComputePairFailureIsolated(t *testing.T) {
	p := generator.Generate()
	// 找一条已存在的遮挡走廊，在其 S-L 之间插入两堵错位竖墙夹杀，
	// 仅影响经过该走廊的对，其他对应保持健康。
	probe, err := engine.New(p, model.GeoConfig{ScaleBits: generator.DefaultScaleBits})
	if err != nil {
		t.Fatal(err)
	}
	var s0 model.Source
	var l0m model.Listener
	found := false
	for _, s := range p.Sources {
		for _, l := range p.Listeners {
			if probe.ComputePair(s.ID, l.ID).Occluded {
				s0, l0m, found = s, l, true
			}
		}
	}
	if !found {
		t.Fatal("golden sample must contain an occluded pair")
	}
	midX1 := (s0.Pos.X + l0m.Pos.X) / 2
	midX2 := midX1 + 2
	yMin := mathMin(s0.Pos.Y, l0m.Pos.Y) - 3
	yMax := mathMax(s0.Pos.Y, l0m.Pos.Y) + 3
	p.Walls = append(p.Walls,
		model.Wall{ID: "BX1", A: model.Point{X: midX1, Y: yMin}, B: model.Point{X: midX1, Y: yMax - 1}},
		model.Wall{ID: "BX2", A: model.Point{X: midX2, Y: yMin + 1}, B: model.Point{X: midX2, Y: yMax}},
	)
	eng, err := engine.New(p, model.GeoConfig{ScaleBits: generator.DefaultScaleBits})
	if err != nil {
		t.Fatal(err)
	}
	failedPairs, healthyPairs := 0, 0
	for _, s := range p.Sources {
		for _, l := range p.Listeners {
			r := eng.ComputePair(s.ID, l.ID)
			if r.Failed {
				failedPairs++
				if r.ErrorCode != "E_PATH" {
					t.Fatalf("want E_PATH, got %s", r.ErrorCode)
				}
				if r.ErrorDetail == "" {
					t.Fatal("failure must carry detail with identifiers")
				}
			} else {
				healthyPairs++
			}
		}
	}
	if failedPairs == 0 {
		t.Fatal("fixture should produce at least one E_PATH pair")
	}
	if healthyPairs == 0 {
		t.Fatal("other pairs must remain unaffected (failure isolation)")
	}
}

func mathMin(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func mathMax(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
