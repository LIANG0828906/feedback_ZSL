package stage

import (
	"math"
	"sort"
)

// EvaluateFixture evaluates the cue envelopes at an exact logical time. It is
// pure: the same project content, time and parameter version always produce the
// same pose and cache key.
func EvaluateFixture(p *Project, f Fixture, timeMS int64) (FixturePose, error) {
	if p == nil {
		return FixturePose{}, domainError(CodeValue, "nil project", "project")
	}
	level := f.LevelPermyriad
	rgb := f.RGBPermyriad
	pan := f.PanMicroDeg
	tilt := f.TiltMicroDeg
	var cueKeys []string
	if c := pickCue(p.Cues, f.ID, CueFade, timeMS); c != nil {
		v, err := interpolateInt(c.FromLevel, c.ToLevel, c.StartMS, c.EndMS, timeMS, "cue."+c.ID+".level")
		if err != nil {
			return FixturePose{}, err
		}
		level = v
		cueKeys = append(cueKeys, CueContentHash(*c))
	}
	if c := pickCue(p.Cues, f.ID, CueSweep, timeMS); c != nil && f.BaseAim == nil {
		v, err := interpolateInt(c.FromPan, c.ToPan, c.StartMS, c.EndMS, timeMS, "cue."+c.ID+".pan")
		if err != nil {
			return FixturePose{}, err
		}
		pan = v
		v, err = interpolateInt(c.FromTilt, c.ToTilt, c.StartMS, c.EndMS, timeMS, "cue."+c.ID+".tilt")
		if err != nil {
			return FixturePose{}, err
		}
		tilt = v
		cueKeys = append(cueKeys, CueContentHash(*c))
	}
	if c := pickCue(p.Cues, f.ID, CueColor, timeMS); c != nil {
		for ch := 0; ch < 3; ch++ {
			v, err := interpolateInt(c.FromRGB[ch], c.ToRGB[ch], c.StartMS, c.EndMS, timeMS, "cue."+c.ID+".rgb")
			if err != nil {
				return FixturePose{}, err
			}
			rgb[ch] = v
		}
		cueKeys = append(cueKeys, CueContentHash(*c))
	}

	if f.InnerHalfMicroDeg > f.OuterHalfMicroDeg {
		return FixturePose{}, &Error{
			Code: CodeParam, Message: "inner cone angle exceeds outer cone angle",
			Field: "inner_half_microdeg", FieldName: "inner_half_microdeg", FixtureID: f.ID,
			Actual: f.InnerHalfMicroDeg, Expected: f.OuterHalfMicroDeg,
		}
	}
	if f.InnerHalfMicroDeg < 0 || f.OuterHalfMicroDeg < 0 || f.OuterHalfMicroDeg >= 90_000_000 {
		return FixturePose{}, &Error{
			Code: CodeParam, Message: "cone half angle outside [0,90) degrees",
			Field: "outer_half_microdeg", FieldName: "outer_half_microdeg", FixtureID: f.ID,
		}
	}

	dir, err := directionFor(f, pan, tilt)
	if err != nil {
		return FixturePose{}, err
	}
	hash := FixtureContentHash(f)
	return FixturePose{
		FixtureID: f.ID, TimeMS: timeMS,
		PositionMM: Vec3{f.PositionUM[0] / 1000, f.PositionUM[1] / 1000, f.PositionUM[2] / 1000},
		Direction:  dir, PanMicroDeg: pan, TiltMicroDeg: tilt,
		InnerHalfMicroDeg: f.InnerHalfMicroDeg, OuterHalfMicroDeg: f.OuterHalfMicroDeg,
		LevelPermyriad: level, CCTKelvin: f.CCTKelvin, RGBPermyriad: rgb,
		FixtureHash: hash, UpstreamKeys: sortedUnique(cueKeys),
	}, nil
}

func pickCue(cues []Cue, fixtureID string, kind CueKind, t int64) *Cue {
	var best *Cue
	for i := range cues {
		c := &cues[i]
		if c.FixtureID != fixtureID || c.Kind != kind || t < c.StartMS || t > c.EndMS {
			continue
		}
		if best == nil || c.StartMS > best.StartMS || (c.StartMS == best.StartMS && c.ID < best.ID) {
			best = c
		}
	}
	return best
}

func interpolateInt(from, to, start, end, t int64, field string) (int64, error) {
	if t <= start {
		return from, nil
	}
	if t >= end || end == start {
		return to, nil
	}
	delta, rem, err := ScaleInt(to-from, t-start, end-start, RoundTowardZero, field)
	if err != nil {
		return 0, err
	}
	_ = rem
	return from + delta, nil
}

func directionFor(f Fixture, panMicroDeg, tiltMicroDeg int64) ([3]float64, error) {
	var v [3]float64
	if f.BaseAim != nil {
		v = [3]float64{float64(f.BaseAim[0]), float64(f.BaseAim[1]), float64(f.BaseAim[2])}
		if norm2(v) < 1e-24 {
			return [3]float64{}, &Error{
				Code: CodeParam, Message: "zero direction vector",
				Field: "base_aim", FieldName: "base_aim", FixtureID: f.ID,
			}
		}
		return normalize(v), nil
	}
	pan := float64(panMicroDeg) / 1_000_000 * math.Pi / 180
	tilt := float64(tiltMicroDeg) / 1_000_000 * math.Pi / 180
	v = [3]float64{
		math.Cos(tilt) * math.Cos(pan),
		math.Cos(tilt) * math.Sin(pan),
		math.Sin(tilt),
	}
	if norm2(v) < 1e-24 {
		return [3]float64{}, &Error{
			Code: CodeParam, Message: "zero direction vector",
			Field: "direction", FieldName: "direction", FixtureID: f.ID,
		}
	}
	return normalize(v), nil
}

func norm2(v [3]float64) float64 {
	return v[0]*v[0] + v[1]*v[1] + v[2]*v[2]
}

func normalize(v [3]float64) [3]float64 {
	n := math.Sqrt(norm2(v))
	if n == 0 {
		return v
	}
	return [3]float64{v[0] / n, v[1] / n, v[2] / n}
}

func sortedFixtures(p *Project) []Fixture {
	out := append([]Fixture(nil), p.Fixtures...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
