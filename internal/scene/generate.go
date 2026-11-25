package scene

import (
	"fmt"
	"math/rand"
)

func Generate() *Scene {
	return GenerateWithSeed(DefaultSeed)
}

func GenerateWithSeed(seed int64) *Scene {
	rng := rand.New(rand.NewSource(seed))
	rooms := []Room{
		{
			ID: "room-a", Index: 0, Name: "West Hall",
			Bounds:     AABB{Min: Vec3{0, 0, 0}, Max: Vec3{4, 4, 3}},
			Resolution: Resolution{X: 3, Y: 3, Z: 2}, GridOffset: Vec3{},
			BoundaryBlendWidth: 0.5, BlendRule: "linear_half_mix",
			AnalyticBias: Vec3{0.36, 0.52, 0.70},
		},
		{
			ID: "room-b", Index: 1, Name: "East Gallery",
			Bounds:     AABB{Min: Vec3{4, 0, 0}, Max: Vec3{8, 4, 3}},
			Resolution: Resolution{X: 3, Y: 2, Z: 2}, GridOffset: Vec3{},
			BoundaryBlendWidth: 0.5, BlendRule: "linear_half_mix",
			AnalyticBias: Vec3{0.70, 0.43, 0.36},
		},
		{
			ID: "room-c", Index: 2, Name: "North Atrium",
			Bounds:     AABB{Min: Vec3{0, 4, 0}, Max: Vec3{4, 8, 3}},
			Resolution: Resolution{X: 2, Y: 3, Z: 2}, GridOffset: Vec3{},
			BoundaryBlendWidth: 0.5, BlendRule: "linear_half_mix",
			AnalyticBias: Vec3{0.38, 0.66, 0.46},
		},
	}

	probes := make([]Probe, 0, 64)
	for _, room := range rooms {
		probes = append(probes, GenerateProbesForRoom(room, seed)...)
	}

	grid := generatePhotonGrid(seed, rng)
	boundaries := []BoundaryRule{
		{ID: "boundary-ab", RoomA: "room-a", RoomB: "room-b", Axis: 0, Position: 4, BlendWidth: 0.5, Rule: "linear_half_mix"},
		{ID: "boundary-ac", RoomA: "room-a", RoomB: "room-c", Axis: 1, Position: 4, BlendWidth: 0.5, Rule: "linear_half_mix"},
	}
	pixels := []Pixel{
		{
			ID: "pixel-a-interior", Position: Vec3{1.2, 1.7, 1.1}, RoomID: "room-a",
			BaseColor: Vec3{0.82, 0.86, 0.90}, Ambient: Vec3{0.03, 0.03, 0.04}, Exposure: 0.82,
		},
		{
			ID: "pixel-ab-boundary", Position: Vec3{4, 2, 1.5}, RoomID: "room-a", BoundaryID: "boundary-ab",
			BaseColor: Vec3{0.88, 0.84, 0.78}, Ambient: Vec3{0.025, 0.028, 0.035}, Exposure: 0.86,
		},
		{
			ID: "pixel-ac-boundary", Position: Vec3{1.5, 4, 1.5}, RoomID: "room-a", BoundaryID: "boundary-ac",
			BaseColor: Vec3{0.78, 0.88, 0.80}, Ambient: Vec3{0.025, 0.032, 0.028}, Exposure: 0.86,
		},
		{
			ID: "pixel-b-interior", Position: Vec3{6.2, 1.4, 1.2}, RoomID: "room-b",
			BaseColor: Vec3{0.90, 0.82, 0.76}, Ambient: Vec3{0.035, 0.028, 0.025}, Exposure: 0.82,
		},
	}

	scene := &Scene{
		SchemaVersion:            SchemaVersion,
		Seed:                     seed,
		SamplingParameterVersion: SamplingParameterVersion,
		AlgorithmVersion:         AlgorithmVersion,
		Tolerance: Tolerances{
			Radiance: 1e-8,
			Color:    1e-8,
			Jump:     1e-8,
		},
		Rooms:      rooms,
		Probes:     probes,
		Boundaries: boundaries,
		PhotonGrid: grid,
		Pixels:     pixels,
	}
	scene.Truth = generateTruth(scene)
	return scene
}

func GenerateProbesForRoom(room Room, seed int64) []Probe {
	count := room.Resolution.Product()
	out := make([]Probe, 0, count)
	for i := 0; i < room.Resolution.X; i++ {
		for j := 0; j < room.Resolution.Y; j++ {
			for k := 0; k < room.Resolution.Z; k++ {
				position := ProbePosition(room, i, j, k)
				samples := 24 + int((seed+int64(room.Index*101)+int64(i*17+j*29+k*43))%97)
				out = append(out, Probe{
					ID: ProbeID(room.ID, i, j, k), RoomID: room.ID,
					I: i, J: j, K: k, Position: position,
					Radiance:          AnalyticRadiance(room, position),
					VisibilitySamples: samples,
				})
			}
		}
	}
	return out
}

func generatePhotonGrid(seed int64, rng *rand.Rand) PhotonGrid {
	dims := [3]int{2, 2, 1}
	grid := PhotonGrid{
		GridMin:  Vec3{0, 0, 0},
		CellSize: Vec3{4, 4, 3},
		Dims:     dims,
		Scale:    DefaultFixedPointScale,
		Cells:    make([]PhotonCell, 0, dims[0]*dims[1]*dims[2]),
	}
	for x := 0; x < dims[0]; x++ {
		for y := 0; y < dims[1]; y++ {
			for z := 0; z < dims[2]; z++ {
				cell := [3]int{x, y, z}
				center := Vec3{
					grid.GridMin[0] + (float64(x)+0.5)*grid.CellSize[0],
					grid.GridMin[1] + (float64(y)+0.5)*grid.CellSize[1],
					grid.GridMin[2] + (float64(z)+0.5)*grid.CellSize[2],
				}
				pc := PhotonCell{Cell: cell, Center: center}
				for band := 0; band < SHCoefficientCount; band++ {
					for channel := 0; channel < ColorChannelCount; channel++ {
						base := int64(900 + rng.Intn(6200))
						pc.Coeff[band][channel] = base +
							int64(x*137+y*251+z*419+band*53+channel*17)
					}
				}
				grid.Cells = append(grid.Cells, pc)
			}
		}
	}
	return grid
}

func generateTruth(scene *Scene) Truth {
	truth := Truth{
		Probes: make([]ProbeTruth, 0, len(scene.Probes)),
		Pixels: make([]PixelTruth, 0, len(scene.Pixels)),
		Jumps:  make([]JumpTruth, 0, len(scene.Boundaries)),
	}
	for _, probe := range scene.Probes {
		room, _, ok := scene.RoomByID(probe.RoomID)
		if !ok {
			continue
		}
		truth.Probes = append(truth.Probes, ProbeTruth{
			ProbeID: probe.ID, Radiance: AnalyticRadiance(room, probe.Position),
		})
	}
	for _, pixel := range scene.Pixels {
		room, _, ok := scene.RoomByID(pixel.RoomID)
		if !ok {
			continue
		}
		radiance, _, _ := ApplyAnalyticBoundaryBlend(scene, room, pixel.Position)
		truth.Pixels = append(truth.Pixels, PixelTruth{
			PixelID: pixel.ID, Color: Shade(pixel, radiance),
		})
	}
	for _, boundary := range scene.Boundaries {
		a, _, okA := scene.RoomByID(boundary.RoomA)
		b, _, okB := scene.RoomByID(boundary.RoomB)
		if !okA || !okB {
			continue
		}
		position := boundaryMidpoint(boundary, a, b)
		ra := AnalyticRadiance(a, position)
		rbPosition := position
		rbPosition[boundary.Axis] = boundary.Position
		rb := AnalyticRadiance(b, rbPosition)
		var jump Vec3
		for c := 0; c < 3; c++ {
			jump[c] = rb[c] - ra[c]
		}
		truth.Jumps = append(truth.Jumps, JumpTruth{
			BoundaryID: boundary.ID, Position: position, Jump: jump,
		})
	}
	return truth
}

func boundaryMidpoint(boundary BoundaryRule, a, b Room) Vec3 {
	var p Vec3
	for axis := 0; axis < 3; axis++ {
		if axis == boundary.Axis {
			p[axis] = boundary.Position
			continue
		}
		lo := maxFloat(a.Bounds.Min[axis], b.Bounds.Min[axis])
		hi := minFloat(a.Bounds.Max[axis], b.Bounds.Max[axis])
		p[axis] = (lo + hi) / 2
	}
	return p
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func PhotonCellIndex(grid PhotonGrid, p Vec3) ([3]int, string) {
	var cell [3]int
	for axis := 0; axis < 3; axis++ {
		raw := int((p[axis] - grid.GridMin[axis]) / grid.CellSize[axis])
		cell[axis] = raw
		if cell[axis] < 0 {
			cell[axis] = 0
		}
		if cell[axis] >= grid.Dims[axis] {
			cell[axis] = grid.Dims[axis] - 1
		}
	}
	id := fmt.Sprintf("photon:%d:%d:%d", cell[0], cell[1], cell[2])
	return cell, id
}

func (g PhotonGrid) CellAt(cell [3]int) (PhotonCell, bool) {
	index := 0
	strideY := g.Dims[2]
	strideX := g.Dims[1] * g.Dims[2]
	index = cell[0]*strideX + cell[1]*strideY + cell[2]
	if index < 0 || index >= len(g.Cells) {
		return PhotonCell{}, false
	}
	return g.Cells[index], true
}
