package samplegen

import (
	"fmt"

	"fixedsampler/internal/fixedpoint"
	"fixedsampler/internal/model"
)

type idealGeom struct {
	width  int
	height int
	bits   int
}

type idealSim struct {
	project *model.Project
	geom    map[string]idealGeom
	values  map[string][]int64
}

type idealSnapshot struct {
	geom   map[string]idealGeom
	values map[string][]int64
}

func newIdealSim(p *model.Project) (*idealSim, error) {
	s := &idealSim{project: p, geom: map[string]idealGeom{}, values: map[string][]int64{}}
	for _, plane := range p.Planes {
		s.geom[plane.Name] = idealGeom{width: plane.Width, height: plane.Height, bits: plane.BitDepth}
		s.values[plane.Name] = append([]int64(nil), plane.Values...)
	}
	return s, nil
}

func (s *idealSim) snapshot() idealSnapshot {
	outVals := make(map[string][]int64, len(s.values))
	for k, v := range s.values {
		outVals[k] = append([]int64(nil), v...)
	}
	outGeom := make(map[string]idealGeom, len(s.geom))
	for k, g := range s.geom {
		outGeom[k] = g
	}
	return idealSnapshot{geom: outGeom, values: outVals}
}

func (s *idealSim) runAll() error {
	for i := range s.project.Config {
		if err := s.runStage(i + 1); err != nil {
			return err
		}
	}
	return nil
}

func (s *idealSim) runStage(seq int) error {
	if seq < 1 || seq > len(s.project.Config) {
		return fmt.Errorf("truth sim: stage %d out of range", seq)
	}
	stage := s.project.Config[seq-1]
	switch stage.Type {
	case "resample":
		return s.resample(stage)
	case "color":
		return s.color(stage)
	case "quantize":
		return s.quantize(stage)
	default:
		return fmt.Errorf("truth sim: unknown stage type %q", stage.Type)
	}
}

func (s *idealSim) resample(stage model.StageConfig) error {
	fx, fy := stage.FactorX, stage.FactorY
	if fx == 0 {
		fx = stage.Factor
		fy = stage.Factor
	}
	for _, name := range stage.Planes {
		g := s.geom[name]
		in := s.values[name]
		nw, nh := g.width/fx, g.height/fy
		out := make([]int64, nw*nh)
		for y := 0; y < nh; y++ {
			for x := 0; x < nw; x++ {
				var v int64
				if stage.Kernel == "nearest" {
					sx := x*fx + fx/2
					sy := y*fy + fy/2
					v = in[sy*g.width+sx]
				} else {
					var sum int64
					for yy := y * fy; yy < (y+1)*fy; yy++ {
						for xx := x * fx; xx < (x+1)*fx; xx++ {
							sum += in[yy*g.width+xx]
						}
					}
					v = fixedpoint.RoundNearest(sum, int64(fx*fy))
				}
				out[y*nw+x] = v
			}
		}
		g.width, g.height = nw, nh
		s.geom[name] = g
		s.values[name] = out
	}
	return nil
}

func (s *idealSim) color(stage model.StageConfig) error {
	m := stage.Matrix
	if m == nil {
		return fmt.Errorf("truth sim: missing matrix at stage %d", stage.Seq)
	}
	in0 := s.geom[stage.InputNames[0]]
	out := make([][]int64, 3)
	for i := range out {
		out[i] = make([]int64, in0.width*in0.height)
	}
	for y := 0; y < in0.height; y++ {
		for x := 0; x < in0.width; x++ {
			idx := y*in0.width + x
			var in [3]int64
			for i, name := range stage.InputNames {
				in[i] = s.values[name][idx]
			}
			for row := 0; row < 3; row++ {
				var num int64 = m.Offsets[row] * m.Scale
				for col := 0; col < 3; col++ {
					num += m.Rows[row][col] * in[col]
				}
				out[row][idx] = fixedpoint.RoundNearest(num, m.Scale)
			}
		}
	}
	for i, name := range stage.OutputNames {
		s.values[name] = out[i]
		s.geom[name] = idealGeom{width: in0.width, height: in0.height, bits: in0.bits}
	}
	return nil
}

func (s *idealSim) quantize(stage model.StageConfig) error {
	q := s.project.ValueScale
	for _, name := range stage.Planes {
		g := s.geom[name]
		in := s.values[name]
		inMax := fixedpoint.MaxCode(g.bits)
		outMax := fixedpoint.MaxCode(stage.BitDepth)
		out := make([]int64, len(in))
		den := q * inMax
		for i, v := range in {
			code := fixedpoint.RoundNearest(v*outMax, den)
			if code < 0 {
				code = 0
			}
			if code > outMax {
				code = outMax
			}
			out[i] = code * q
		}
		g.bits = stage.BitDepth
		s.geom[name] = g
		s.values[name] = out
	}
	return nil
}

func (s *idealSim) valueAt(snap *idealSnapshot, plane string, x, y int) (int64, error) {
	values := s.values
	geoms := s.geom
	if snap != nil {
		values = snap.values
		geoms = snap.geom
	}
	v, ok := values[plane]
	if !ok {
		return 0, fmt.Errorf("truth sim: plane %q missing", plane)
	}
	g := geoms[plane]
	if x < 0 || y < 0 || x >= g.width || y >= g.height {
		return 0, fmt.Errorf("truth sim: point (%d,%d) outside %s %dx%d", x, y, plane, g.width, g.height)
	}
	return v[y*g.width+x], nil
}
