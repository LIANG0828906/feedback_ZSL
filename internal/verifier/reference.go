package verifier

import (
	"fmt"

	"fixedsampler/internal/model"
)

func assembleLevels(rows map[int]PersistedRow, p *model.Project) (map[int]map[string]*refPlane, error) {
	levels := map[int]map[string]*refPlane{}
	maxSeq := len(p.Config)
	for seq := 0; seq <= maxSeq; seq++ {
		dims := map[string]imageDim{}
		for _, idx := range sortedRowIndices(rows) {
			level := findLevel(rows[idx].Artifact, seq)
			if level == nil {
				continue
			}
			for _, tile := range level.Tiles {
				d := dims[tile.Plane]
				d.w = max(d.w, tile.Rect.X1)
				d.h = max(d.h, tile.Rect.Y1)
				dims[tile.Plane] = d
			}
		}
		planes := map[string]*refPlane{}
		for name, d := range dims {
			planes[name] = &refPlane{name: name, width: d.w, height: d.h, values: make([]int64, d.w*d.h)}
		}
		for _, idx := range sortedRowIndices(rows) {
			level := findLevel(rows[idx].Artifact, seq)
			if level == nil {
				continue
			}
			for _, tile := range level.Tiles {
				rp := planes[tile.Plane]
				if rp == nil {
					return nil, fmt.Errorf("verifier: missing assembled plane %q", tile.Plane)
				}
				putTile(rp, tile)
			}
		}
		levels[seq] = planes
	}
	return levels, nil
}

type imageDim struct{ w, h int }

func findLevel(chunk model.ChunkArtifact, seq int) *model.LevelArtifact {
	for i := range chunk.Levels {
		if chunk.Levels[i].Seq == seq {
			return &chunk.Levels[i]
		}
	}
	return nil
}

func putTile(rp *refPlane, tile model.Tile) {
	i := 0
	for y := tile.Rect.Y0; y < tile.Rect.Y1; y++ {
		for x := tile.Rect.X0; x < tile.Rect.X1; x++ {
			if i < len(tile.Values) && y >= 0 && x >= 0 && y < rp.height && x < rp.width {
				rp.values[y*rp.width+x] = tile.Values[i]
			}
			i++
		}
	}
}

func initialRefPlanes(p *model.Project, persisted0 map[string]*refPlane) map[string]*refPlane {
	out := map[string]*refPlane{}
	for _, desc := range p.Planes {
		rp := persisted0[desc.Name]
		if rp == nil {
			rp = &refPlane{name: desc.Name, width: desc.Width, height: desc.Height, values: append([]int64(nil), desc.Values...)}
		}
		rp.bits = desc.BitDepth
		rp.subX = desc.SubsampleX
		rp.subY = desc.SubsampleY
		rp.cumX = 1
		rp.cumY = 1
		out[desc.Name] = rp
	}
	return out
}

func cloneRef(in map[string]*refPlane) map[string]*refPlane {
	out := make(map[string]*refPlane, len(in))
	for k, v := range in {
		cp := *v
		out[k] = &cp
	}
	return out
}

func referenceStage(stage model.StageConfig, planes map[string]*refPlane, valueScale int64) error {
	switch stage.Type {
	case "resample":
		fx, fy := stage.FactorX, stage.FactorY
		for _, name := range stage.Planes {
			old := planes[name]
			nw, nh := old.width/fx, old.height/fy
			out := &refPlane{
				name: name, width: nw, height: nh, bits: old.bits, subX: old.subX, subY: old.subY,
				cumX: old.cumX * fx, cumY: old.cumY * fy, values: make([]int64, nw*nh),
			}
			taps := int64(fx * fy)
			w := stage.FixedScale / taps
			for y := 0; y < nh; y++ {
				for x := 0; x < nw; x++ {
					var value int64
					if stage.Kernel == "nearest" {
						sx := x*fx + fx/2
						sy := y*fy + fy/2
						value = old.values[sy*old.width+sx]
					} else {
						var acc int64
						for ty := 0; ty < fy; ty++ {
							for tx := 0; tx < fx; tx++ {
								sx, sy := x*fx+tx, y*fy+ty
								acc += old.values[sy*old.width+sx] * w
							}
						}
						value, _ = floorDiv(acc, stage.FixedScale)
					}
					out.values[y*nw+x] = value
				}
			}
			planes[name] = out
		}
	case "color":
		first := planes[stage.InputNames[0]]
		for i, outName := range stage.OutputNames {
			out := &refPlane{
				name: outName, width: first.width, height: first.height, bits: first.bits,
				subX: first.subX, subY: first.subY, cumX: first.cumX, cumY: first.cumY,
				values: make([]int64, first.width*first.height),
			}
			for y := 0; y < first.height; y++ {
				for x := 0; x < first.width; x++ {
					idx := y*first.width + x
					num := stage.Matrix.Offsets[i] * stage.Matrix.Scale
					for c, inName := range stage.InputNames {
						num += stage.Matrix.Rows[i][c] * planes[inName].values[idx]
					}
					out.values[idx], _ = floorDiv(num, stage.Matrix.Scale)
				}
			}
			planes[outName] = out
		}
	case "quantize":
		outMax := maxCode(stage.BitDepth)
		for _, name := range stage.Planes {
			old := planes[name]
			inMax := maxCode(old.bits)
			den := valueScale * inMax
			out := &refPlane{
				name: name, width: old.width, height: old.height, bits: stage.BitDepth,
				subX: old.subX, subY: old.subY, cumX: old.cumX, cumY: old.cumY,
				values: make([]int64, len(old.values)),
			}
			for i, v := range old.values {
				num := v * outMax
				code, rem := floorDiv(num, den)
				if 2*rem >= den {
					code++
				}
				if code < 0 {
					code = 0
				}
				if code > outMax {
					code = outMax
				}
				out.values[i] = code * valueScale
			}
			planes[name] = out
		}
	default:
		return fmt.Errorf("verifier: unsupported stage type %q", stage.Type)
	}
	return nil
}

func compareTiles(rows map[int]PersistedRow, seq int, ref map[string]*refPlane) *model.DiffLocation {
	for _, idx := range sortedRowIndices(rows) {
		chunk := rows[idx].Artifact
		level := findLevel(chunk, seq)
		if level == nil {
			loc := diffFor(chunk, seq, "level", "present", "missing")
			return loc
		}
		for _, tile := range level.Tiles {
			rp := ref[tile.Plane]
			if rp == nil {
				loc := diffFor(chunk, seq, "tiles."+tile.Plane, "reference plane", "missing")
				return loc
			}
			wantRect := mapRect(chunk.FullRect, rp.subX, rp.subY, rp.cumX, rp.cumY)
			if tile.Rect != wantRect {
				loc := diffFor(chunk, seq, "tiles."+tile.Plane+".rect", wantRect, tile.Rect)
				return loc
			}
			i := 0
			for y := tile.Rect.Y0; y < tile.Rect.Y1; y++ {
				for x := tile.Rect.X0; x < tile.Rect.X1; x++ {
					field := fmt.Sprintf("levels[%d].tiles.%s.values[%d]", seq, tile.Plane, i)
					if y < 0 || x < 0 || y >= rp.height || x >= rp.width {
						return diffFor(chunk, seq, field, "in-bounds", fmt.Sprintf("(%d,%d)", x, y))
					}
					want := rp.values[y*rp.width+x]
					got := tile.Values[i]
					if want != got {
						loc := diffFor(chunk, seq, field, want, got)
						return loc
					}
					i++
				}
			}
		}
	}
	return nil
}

func mapRect(r model.Rect, subX, subY, cumX, cumY int) model.Rect {
	dx := max(1, subX*cumX)
	dy := max(1, subY*cumY)
	return model.Rect{X0: floorDivInt(r.X0, dx), Y0: floorDivInt(r.Y0, dy), X1: floorDivInt(r.X1, dx), Y1: floorDivInt(r.Y1, dy)}
}

func floorDiv(a, b int64) (int64, int64) {
	q := a / b
	r := a % b
	if r < 0 {
		q--
		r += b
	}
	return q, r
}

func floorDivInt(a, b int) int {
	q := a / b
	r := a % b
	if r < 0 {
		q--
	}
	return q
}

func maxCode(bits int) int64 { return int64((uint64(1) << bits) - 1) }

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
