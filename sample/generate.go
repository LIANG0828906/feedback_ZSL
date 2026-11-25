// Package sample contains the deterministic local texture generator.
package sample

import (
	ct "colortrace"
	"colortrace/reference"
)

const (
	Width     = 24
	Height    = 16
	ChunkSize = 4
	BaseBits  = 10
)

// Generate creates the fixed-seed first acceptance sample.
func Generate() (*ct.Project, error) {
	return GenerateSeed(ct.DefaultSeed)
}

// GenerateSeed creates a deterministic sample for an explicit seed.
func GenerateSeed(seed int64) (*ct.Project, error) {
	rng := splitMix64(uint64(seed))
	planes := []*ct.Plane{
		gradientPlane("Y", false, seed, 470, 5, 1, rng),
		gradientPlane("Cb", true, seed+17, 520, 2, -1, rng),
		gradientPlane("Cr", true, seed+31, 505, -2, 2, rng),
	}

	chromaKernel := ct.Kernel{
		Name: "chroma-center-3", RadiusX: 1, RadiusY: 0, NormScale: 64,
		Taps: []ct.Tap{{DX: -1, DY: 0, W: 16}, {DX: 0, DY: 0, W: 32}, {DX: 1, DY: 0, W: 16}},
	}
	stages := []ct.StageConfig{
		{
			Index: 0, ID: "half-box", Description: "2x downsample with rounded fixed weights",
			FactorX: 2, FactorY: 2, PhaseX: 0, PhaseY: 0, AccumScale: 256, Bits: 8,
			Kernel: ct.Kernel{
				Name: "box-2x2-rounded", RadiusX: 1, RadiusY: 1, NormScale: 256,
				Taps: []ct.Tap{
					{DX: 0, DY: 0, W: 64}, {DX: 1, DY: 0, W: 64},
					{DX: 0, DY: 1, W: 64}, {DX: 1, DY: 1, W: 63},
				},
			},
			ChromaKernel: &chromaKernel,
			ParamVersion: "half-box-p1",
		},
		{
			Index: 1, ID: "quarter-gaussian", Description: "second 2x level with a 3x3 support",
			FactorX: 2, FactorY: 2, PhaseX: 0, PhaseY: 0, AccumScale: 256, Bits: 6,
			Kernel: ct.Kernel{
				Name: "triangle-3x3-rounded", RadiusX: 1, RadiusY: 1, NormScale: 256,
				Taps: []ct.Tap{
					{DX: -1, DY: -1, W: 16}, {DX: 0, DY: -1, W: 32}, {DX: 1, DY: -1, W: 16},
					{DX: -1, DY: 0, W: 32}, {DX: 0, DY: 0, W: 63}, {DX: 1, DY: 0, W: 32},
					{DX: -1, DY: 1, W: 16}, {DX: 0, DY: 1, W: 32}, {DX: 1, DY: 1, W: 16},
				},
			},
			ChromaKernel: &chromaKernel,
			ParamVersion: "quarter-tri-p2",
		},
	}
	queries := []ct.TraceQuery{}
	coords := []ct.Coord{{X: 4, Y: 4}, {X: 8, Y: 4}, {X: 12, Y: 8}, {X: 16, Y: 8}, {X: 20, Y: 12}}
	for _, name := range []string{"Y", "Cb", "Cr"} {
		for _, coord := range coords {
			queries = append(queries, ct.TraceQuery{Plane: name, X: coord.X, Y: coord.Y})
		}
	}
	project := &ct.Project{
		SchemaVersion:          ct.SchemaVersion,
		ProjectID:              "seeded-color-band-trace",
		Seed:                   seed,
		BaseBits:               BaseBits,
		Width:                  Width,
		Height:                 Height,
		ChunkSize:              ChunkSize,
		BandTolerance:          20 * 256,
		DominantShareThreshold: 0.60,
		Planes:                 planes,
		Stages:                 stages,
		TracePoints:            queries,
	}
	truth, samples, err := reference.BuildTruth(project)
	if err != nil {
		return nil, err
	}
	project.Truth = truth
	final := samples[len(samples)-1]
	for _, desc := range planes {
		ps := final.Planes[desc.Name]
		target := &ct.Plane{
			PlaneDesc: ct.PlaneDesc{
				Name: desc.Name, Width: ps.Width, Height: ps.Height, Chroma: desc.Chroma,
				SubsampleX: 1, SubsampleY: 1,
			},
			Bits:   stages[len(stages)-1].Bits,
			Values: append([]int64{}, ps.Codes...),
		}
		project.Targets = append(project.Targets, target)
	}
	if err := ct.ValidateProject(project); err != nil {
		return nil, err
	}
	return project, nil
}

func gradientPlane(name string, chroma bool, seed int64, center, slopeX, slopeY int64, rng func() uint64) *ct.Plane {
	phase := int64(rng()%5) - 2
	values := make([]int64, 0, Width*Height)
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			value := center + phase + slopeX*int64(x) + slopeY*int64(y)
			if value < 0 {
				value = 0
			}
			if max := int64(1<<BaseBits - 1); value > max {
				value = max
			}
			values = append(values, value)
		}
	}
	return &ct.Plane{
		PlaneDesc: ct.PlaneDesc{Name: name, Width: Width, Height: Height, Chroma: chroma, SubsampleX: 1, SubsampleY: 1},
		Bits:      BaseBits,
		Values:    values,
	}
}

func splitMix64(seed uint64) func() uint64 {
	state := seed
	return func() uint64 {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		return z ^ (z >> 31)
	}
}

func hash01(seed uint64, x, y int) uint64 {
	h := seed ^ uint64(x)*0x9e3779b97f4a7c15 ^ uint64(y)*0xbf58476d1ce4e5b9
	h = (h ^ (h >> 30)) * 0xbf58476d1ce4e5b9
	h = (h ^ (h >> 27)) * 0x94d049bb133111eb
	return h ^ (h >> 31)
}
