package maskqc

import (
	"fmt"
	"math/rand"
)

const DefaultSeed int64 = 42042

// GenerateSample builds the deterministic canonical v3 project set. The seed is
// accepted for reproducibility; layout is table-driven and defect placement is
// selected from a private deterministic RNG stream.
func GenerateSample(seed int64) (*Project, error) {
	rng := rand.New(rand.NewSource(seed))
	specs := []struct {
		id       string
		w, h     int
		strokes  []*Stroke
		spill    Point
		badSpill bool
	}{
		{
			id: "glyph-ring-32", w: 32, h: 32, badSpill: true,
			strokes: []*Stroke{
				sampleStroke("ring-top", 1, 10, "casing", Point{8, 8}, Point{24, 8}, 3),
				sampleStroke("ring-right", 2, 10, "casing", Point{24, 8}, Point{24, 24}, 3),
				sampleStroke("ring-bottom", 3, 10, "casing", Point{24, 24}, Point{8, 24}, 3),
				sampleStroke("ring-left", 4, 10, "casing", Point{8, 24}, Point{8, 8}, 3),
			},
			spill: Point{29, 5},
		},
		{
			id: "glyph-bars-48", w: 48, h: 32,
			strokes: []*Stroke{
				sampleStroke("bars-top", 1, 20, "bars", Point{8, 8}, Point{40, 8}, 2),
				sampleStroke("bars-mid", 2, 20, "bars", Point{8, 16}, Point{40, 16}, 2),
				sampleStroke("bars-bottom", 3, 20, "bars", Point{8, 24}, Point{40, 24}, 2),
			},
			spill: Point{43, 8},
		},
		{
			id: "glyph-mark-40", w: 40, h: 40,
			strokes: []*Stroke{
				sampleStroke("mark-a", 1, 10, "mark", Point{9, 29}, Point{20, 10}, 3),
				sampleStroke("mark-b", 2, 10, "mark", Point{20, 10}, Point{31, 29}, 3),
				sampleStroke("mark-cross", 3, 20, "mark", Point{13, 22}, Point{27, 22}, 2),
			},
			spill: Point{6, 31},
		},
	}

	p := &Project{Schema: SchemaV3, Seed: seed, Name: fmt.Sprintf("deterministic-icon-set-%d", seed)}
	for si, spec := range specs {
		truth := NewMask(spec.w, spec.h)
		for _, st := range spec.strokes {
			st.SourceSchema = SchemaV3
			full := *st
			full.HardnessPermille = HardnessScale
			full.WeightPermyriad = WeightScale
			comp, err := RasterizeStrokeRect(&full, Rect{0, 0, spec.w, spec.h})
			if err != nil {
				return nil, err
			}
			if err := ComposeAlpha(truth.Alpha, comp.Alpha); err != nil {
				return nil, err
			}
		}

		candidate := erodeBinary(truth)
		// Deterministic in-mask voids. They are covered again by construction strokes.
		punchRect(candidate, Rect{14 + si%2, 7, 17 + si%2, 9})
		punchRect(candidate, Rect{spec.w/2 - 1, spec.h/2 + 3, spec.w/2 + 2, spec.h/2 + 5})
		// External overflow/spill. The first icon intentionally exceeds topology tolerance.
		spillRadius := 1
		if spec.badSpill {
			spillRadius = 2 + rng.Intn(1)
		}
		fillCircle(candidate, spec.spill.X, spec.spill.Y, spillRadius, 255)
		// A second RNG-derived satellite pixel keeps the stream explicit and stable.
		satX := (spec.spill.X + rng.Intn(2)) % spec.w
		satY := (spec.spill.Y + 2 + rng.Intn(2)) % spec.h
		if !spec.badSpill {
			setMaskPixel(candidate, satX, satY, 255)
		}

		icon := &Icon{
			ID: spec.id, Width: spec.w, Height: spec.h,
			TileWidth: 8, TileHeight: 8,
			Candidate: candidate, Truth: truth,
			Strokes: spec.strokes,
		}
		for tx := 0; tx < spec.w/icon.TileWidth; tx++ {
			for ty := 0; ty < spec.h/icon.TileHeight; ty++ {
				r := TileRect(icon, tx, ty)
				tile, err := CropMask(truth, r)
				if err != nil {
					return nil, err
				}
				icon.TruthTiles = append(icon.TruthTiles, TileTruth{
					ID: TileID(icon.ID, tx, ty), IconID: icon.ID, Rect: r, Hash: tile.Hash(),
				})
			}
		}
		p.Icons = append(p.Icons, icon)
	}
	return p, nil
}

func sampleStroke(id string, seq, priority int, group string, start, end Point, radius int) *Stroke {
	return &Stroke{
		ID: id, Seq: seq, Priority: priority, Group: group,
		Start: start, End: end, Radius: radius,
		HardnessPermille:  900,
		WeightPermyriad:   9600,
		DeclaredDirection: DirAlong,
		CompositionOrder:  DefaultCompositionOrder,
	}
}

const DefaultCompositionOrder = 100

func setMaskPixel(m *Mask, x, y int, v byte) {
	if x < 0 || y < 0 || x >= m.Width || y >= m.Height {
		return
	}
	m.Alpha[y*m.Width+x] = v
}

func fillCircle(m *Mask, cx, cy, r int, v byte) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			dx := x - cx
			dy := y - cy
			if dx*dx+dy*dy <= r*r {
				setMaskPixel(m, x, y, v)
			}
		}
	}
}

func punchRect(m *Mask, r Rect) {
	for y := r.MinY; y < r.MaxY; y++ {
		for x := r.MinX; x < r.MaxX; x++ {
			setMaskPixel(m, x, y, 0)
		}
	}
}

func erodeBinary(src *Mask) *Mask {
	out := NewMask(src.Width, src.Height)
	for y := 0; y < src.Height; y++ {
		for x := 0; x < src.Width; x++ {
			if src.At(x, y) < 128 {
				continue
			}
			ok := true
			for _, d := range []Point{{0, 0}, {1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				nx, ny := x+d.X, y+d.Y
				if nx < 0 || ny < 0 || nx >= src.Width || ny >= src.Height || src.At(nx, ny) < 128 {
					ok = false
					break
				}
			}
			if ok {
				out.Alpha[y*src.Width+x] = 255
			}
		}
	}
	return out
}
