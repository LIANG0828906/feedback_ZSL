package geometry_test

import (
	"testing"

	"github.com/gameaudio/workbench/internal/errors"
	"github.com/gameaudio/workbench/internal/fixedpoint"
	"github.com/gameaudio/workbench/internal/generator"
	"github.com/gameaudio/workbench/internal/geometry"
	"github.com/gameaudio/workbench/internal/model"
)

func TestScaleBitsOutOfRange(t *testing.T) {
	for _, bits := range []int{0, 9, -1} {
		_, err := geometry.New(model.GeoConfig{ScaleBits: bits}, nil)
		if errors.CodeOf(err) != errors.EGeo {
			t.Fatalf("bits=%d want E_GEO, got %v", bits, err)
		}
		if we, ok := err.(*errors.WorkError); ok && we.Field != "scaleBits" {
			t.Fatalf("error must point at scaleBits, got %s", we.Field)
		}
	}
}

func TestDegenWall(t *testing.T) {
	walls := []model.Wall{{ID: "W1",
		A: model.Point{X: 1, Y: 1}, B: model.Point{X: 1, Y: 1}}}
	_, err := geometry.New(model.GeoConfig{ScaleBits: 4}, walls)
	if errors.CodeOf(err) != errors.EDegen {
		t.Fatalf("want E_DEGEN, got %v", err)
	}
}

func TestWallOrderIndependence(t *testing.T) {
	p := generator.Generate()
	cfg := model.GeoConfig{ScaleBits: generator.DefaultScaleBits}
	eng1, err := geometry.New(cfg, p.Walls)
	if err != nil {
		t.Fatal(err)
	}
	// 反转墙体顺序。
	reversed := make([]model.Wall, len(p.Walls))
	for i, w := range p.Walls {
		reversed[len(p.Walls)-1-i] = w
	}
	eng2, err := geometry.New(cfg, reversed)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Sources {
		for _, l := range p.Listeners {
			fs1, fl1 := eng1.Quant(s.Pos), eng1.Quant(l.Pos)
			fs2, fl2 := eng2.Quant(s.Pos), eng2.Quant(l.Pos)
			b1 := eng1.BlockingWalls(fs1, fl1)
			b2 := eng2.BlockingWalls(fs2, fl2)
			if len(b1) != len(b2) {
				t.Fatalf("blocking count differs for %s->%s: %v vs %v",
					s.ID, l.ID, b1, b2)
			}
			for i := range b1 {
				if b1[i] != b2[i] {
					t.Fatalf("blocking wall order/contents differ: %v vs %v", b1, b2)
				}
			}
			if len(b1) > 0 {
				d1, e1 := eng1.Diffraction(fs1, fl1, b1)
				d2, e2 := eng2.Diffraction(fs2, fl2, b2)
				if (e1 == nil) != (e2 == nil) || d1 != d2 {
					t.Fatalf("diffraction differs: %v,%v vs %v,%v", d1, e1, d2, e2)
				}
			}
		}
	}
}

func TestGoldenTruthGeometry(t *testing.T) {
	p := generator.Generate()
	eng, err := geometry.New(model.GeoConfig{ScaleBits: generator.DefaultScaleBits}, p.Walls)
	if err != nil {
		t.Fatal(err)
	}
	truth := map[string]model.Truth{}
	for _, tr := range p.Truths {
		truth[tr.SourceID+"->"+tr.ListenerID] = tr
	}
	for _, s := range p.Sources {
		for _, l := range p.Listeners {
			fs, fl := eng.Quant(s.Pos), eng.Quant(l.Pos)
			bw := eng.BlockingWalls(fs, fl)
			got := len(bw) > 0
			want := truth[s.ID+"->"+l.ID].Occluded
			if got != want {
				t.Fatalf("occlusion mismatch %s->%s: got %v want %v",
					s.ID, l.ID, got, want)
			}
			if got {
				d, err := eng.Diffraction(fs, fl, bw)
				if err != nil {
					t.Fatal(err)
				}
				wantD := truth[s.ID+"->"+l.ID].DiffractionLen
				if abs(d-wantD) > 1e-3 {
					t.Fatalf("diffraction len mismatch %s->%s: %.6f vs %.6f",
						s.ID, l.ID, d, wantD)
				}
			}
		}
	}
}

func TestDiffractionPathNotFound(t *testing.T) {
	// 两堵几乎等高且错位的竖墙夹杀 S->L 走廊：
	// 任一墙角的绕射折线都会被另一堵墙阻断，因此无可达折线路径。
	walls := []model.Wall{
		{ID: "W1", A: model.Point{X: 4, Y: 0}, B: model.Point{X: 4, Y: 9}},
		{ID: "W2", A: model.Point{X: 8, Y: 1}, B: model.Point{X: 8, Y: 10}},
	}
	eng, err := geometry.New(model.GeoConfig{ScaleBits: 4}, walls)
	if err != nil {
		t.Fatal(err)
	}
	s := eng.Quant(model.Point{X: 0, Y: 5})
	l := eng.Quant(model.Point{X: 12, Y: 5})
	bw := eng.BlockingWalls(s, l)
	if len(bw) == 0 {
		t.Fatal("fixture must occlude")
	}
	_, err = eng.Diffraction(s, l, bw)
	if errors.CodeOf(err) != errors.EPath {
		t.Fatalf("want E_PATH, got %v (blocking=%v)", err, bw)
	}
}

func TestFixedPointQuantization(t *testing.T) {
	conv, err := fixedpoint.NewConverter(model.GeoConfig{ScaleBits: 4})
	if err != nil {
		t.Fatal(err)
	}
	f := conv.FromFloat(1.23456)
	if conv.ToFloat(f) != 1.2346 {
		t.Fatalf("quantization unexpected: %.5f", conv.ToFloat(f))
	}
	// 乘法标度还原。
	a := conv.FromFloat(2.5)
	b := conv.FromFloat(4)
	if got := conv.ToFloat(conv.Mul(a, b)); got != 10.0 {
		t.Fatalf("mul mismatch: %v", got)
	}
}

func TestPointInPolygon(t *testing.T) {
	eng, _ := geometry.New(model.GeoConfig{ScaleBits: 4}, nil)
	verts := []geometry.FPoint{
		eng.Quant(model.Point{X: 0, Y: 0}),
		eng.Quant(model.Point{X: 10, Y: 0}),
		eng.Quant(model.Point{X: 10, Y: 10}),
		eng.Quant(model.Point{X: 0, Y: 10}),
	}
	if !geometry.PointInPolygon(eng.Quant(model.Point{X: 5, Y: 5}), verts) {
		t.Fatal("interior point must be inside")
	}
	if geometry.PointInPolygon(eng.Quant(model.Point{X: 15, Y: 5}), verts) {
		t.Fatal("exterior point must be outside")
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
