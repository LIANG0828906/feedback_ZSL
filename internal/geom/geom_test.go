package geom_test

import (
	"testing"

	"glyphbrush/internal/geom"
)

func TestPolygonSelfIntersection(t *testing.T) {
	cases := []struct {
		name string
		poly []geom.Point
		want bool
	}{
		{
			name: "simple square",
			poly: []geom.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}},
			want: false,
		},
		{
			name: "concave",
			poly: []geom.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 5, Y: 3}, {X: 0, Y: 10}},
			want: false,
		},
		{
			name: "bow tie",
			poly: []geom.Point{{X: 0, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 10, Y: 0}},
			want: true,
		},
		{
			name: "vertex touch",
			poly: []geom.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 5, Y: 5}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 5, Y: 5}},
			want: true,
		},
		{
			name: "collinear overlap",
			poly: []geom.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 2, Y: 0}, {X: 0, Y: 10}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := geom.PolygonSelfIntersects(tc.poly)
			if got != tc.want {
				t.Fatalf("PolygonSelfIntersects=%v want %v", got, tc.want)
			}
		})
	}
}

func TestResamplePolylineEndpoints(t *testing.T) {
	points := []geom.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}}
	out := geom.ResamplePolyline(points, 3)
	if len(out) < 2 || out[0].Distance(points[0]) > 1e-9 || out[len(out)-1].Distance(points[2]) > 1e-9 {
		t.Fatalf("resampling lost endpoints: %+v", out)
	}
	// Chords cut the corner, so Euclidean length is slightly below 20 while
	// the arc-length parameterization still spans [0,20].
	if got := geom.PolylineLength(out); got < 19 || got > 20.01 {
		t.Fatalf("resampled length %f", got)
	}
	if len(out) != 8 {
		t.Fatalf("resampled point count = %d, want 8", len(out))
	}
}

func TestPolylineSelfIntersection(t *testing.T) {
	self := []geom.Point{{X: 0, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 10, Y: 0}}
	if got, _ := geom.PolylineSelfIntersects(self); !got {
		t.Fatal("crossing polyline was not detected")
	}
	open := []geom.Point{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}}
	if got, _ := geom.PolylineSelfIntersects(open); got {
		t.Fatal("simple open polyline reported as self-intersecting")
	}
}
