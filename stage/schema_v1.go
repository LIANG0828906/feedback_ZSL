package stage

import (
	"encoding/json"
	"fmt"
)

type v1Doc struct {
	Schema           *string     `json:"schema"`
	ProjectID        *string     `json:"project_id"`
	Seed             *int64      `json:"seed"`
	ParameterVersion *string     `json:"parameter_version"`
	Timebase         *v1Timebase `json:"timebase"`
	Fixtures         []v1Fixture `json:"fixtures"`
	Cues             []v1Cue     `json:"cues"`
	Timeline         *v1Timeline `json:"timeline"`
}

type v1Timebase struct {
	TickHz *int64 `json:"tick_hz"`
}

type v1Fixture struct {
	ID            *string        `json:"id"`
	PositionMM    []int64        `json:"position_mm"`
	Orientation   *v1Orientation `json:"orientation"`
	Cone          *v1Cone        `json:"cone"`
	CCTPermille   *int64         `json:"cct_permille"`
	LevelPermille *int64         `json:"level_permille"`
	RGBPermille   []int64        `json:"rgb_permille"`
}

type v1Orientation struct {
	PanDeciDeg  *int64 `json:"pan_ddeg"`
	TiltDeciDeg *int64 `json:"tilt_ddeg"`
}

type v1Cone struct {
	InnerDeciDeg *int64 `json:"inner_ddeg"`
	// OuterDeciDeg is intentionally optional in v1. Loading applies the
	// declared default and records a default-application trace.
	OuterDeciDeg *int64 `json:"outer_ddeg,omitempty"`
}

type v1Cue struct {
	ID              *string `json:"id"`
	Fixture         *string `json:"fixture"`
	Kind            *string `json:"kind"`
	StartTick       *int64  `json:"start_tick"`
	EndTick         *int64  `json:"end_tick"`
	FromLevel       *int64  `json:"from_level_permille,omitempty"`
	ToLevel         *int64  `json:"to_level_permille,omitempty"`
	FromRGB         []int64 `json:"from_rgb_permille,omitempty"`
	ToRGB           []int64 `json:"to_rgb_permille,omitempty"`
	FromPanDeciDeg  *int64  `json:"from_pan_ddeg,omitempty"`
	ToPanDeciDeg    *int64  `json:"to_pan_ddeg,omitempty"`
	FromTiltDeciDeg *int64  `json:"from_tilt_ddeg,omitempty"`
	ToTiltDeciDeg   *int64  `json:"to_tilt_ddeg,omitempty"`
}

type v1Timeline struct {
	DurationTick *int64     `json:"duration_tick"`
	Windows      []v1Window `json:"windows"`
}

type v1Window struct {
	Number      *int    `json:"number"`
	StartTick   *int64  `json:"start_tick"`
	EndTick     *int64  `json:"end_tick"`
	SamplesTick []int64 `json:"samples_tick"`
}

func loadV1(data []byte) (*Project, error) {
	var doc v1Doc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, schemaError(SchemaV1, "$", "invalid v1 JSON: "+err.Error())
	}
	projectID, err := reqString(doc.ProjectID, SchemaV1, "project_id")
	if err != nil {
		return nil, err
	}
	paramVersion, err := reqString(doc.ParameterVersion, SchemaV1, "parameter_version")
	if err != nil {
		return nil, err
	}
	seed, err := reqInt(doc.Seed, SchemaV1, "seed")
	if err != nil {
		return nil, err
	}
	if doc.Timebase == nil {
		return nil, schemaError(SchemaV1, "timebase", "missing timebase")
	}
	tickHz, err := reqInt(doc.Timebase.TickHz, SchemaV1, "timebase.tick_hz")
	if err != nil {
		return nil, err
	}
	if tickHz <= 0 {
		return nil, schemaError(SchemaV1, "timebase.tick_hz", "tick rate must be positive")
	}
	if len(doc.Fixtures) == 0 {
		return nil, schemaError(SchemaV1, "fixtures", "fixture array is empty")
	}
	p := &Project{
		ProjectID: projectID, SourceVersion: SchemaV1, Seed: seed,
		ParameterVersion: paramVersion, Revision: 1,
	}
	for i := range doc.Fixtures {
		src := &doc.Fixtures[i]
		path := fmt.Sprintf("fixtures[%d]", i)
		id, err := reqString(src.ID, SchemaV1, path+".id")
		if err != nil {
			return nil, err
		}
		posMM, err := reqVec(src.PositionMM, 3, SchemaV1, path+".position_mm")
		if err != nil {
			return nil, err
		}
		if src.Orientation == nil {
			return nil, schemaError(SchemaV1, path+".orientation", "missing orientation")
		}
		panDeci, err := reqInt(src.Orientation.PanDeciDeg, SchemaV1, path+".orientation.pan_ddeg")
		if err != nil {
			return nil, err
		}
		tiltDeci, err := reqInt(src.Orientation.TiltDeciDeg, SchemaV1, path+".orientation.tilt_ddeg")
		if err != nil {
			return nil, err
		}
		if src.Cone == nil {
			return nil, schemaError(SchemaV1, path+".cone", "missing cone")
		}
		innerDeci, err := reqInt(src.Cone.InnerDeciDeg, SchemaV1, path+".cone.inner_ddeg")
		if err != nil {
			return nil, err
		}
		var (
			cct int64
			rem int64
		)
		if src.CCTPermille == nil {
			cct = 5600
			addTrace(p, ConversionTrace{
				FixtureID: id, Field: path + ".cct_permille", SourceVersion: SchemaV1,
				SourceValue: "<missing>", InternalValue: formatInt(cct), Operation: "apply_default",
				Rounding: string(RoundExact), DefaultSource: "v1 default: 5600 K",
			})
		} else {
			cct, rem, err = CCTPermilleToKelvin(*src.CCTPermille)
			if err != nil {
				return nil, err
			}
			traceScaled(p, id, "", path+".cct_permille", SchemaV1, formatInt(*src.CCTPermille), formatInt(cct), "cct_permille_to_kelvin", RoundNearest, rem)
		}
		levelPermille, err := reqInt(src.LevelPermille, SchemaV1, path+".level_permille")
		if err != nil {
			return nil, err
		}
		rgbPermille, err := reqVec(src.RGBPermille, 3, SchemaV1, path+".rgb_permille")
		if err != nil {
			return nil, err
		}

		pan, rem, err := DeciDegToMicroDeg(panDeci)
		if err != nil {
			return nil, err
		}
		traceScaled(p, id, "", path+".orientation.pan_ddeg", SchemaV1, formatInt(panDeci), formatInt(pan), "deci_deg_to_microdeg", RoundExact, rem)
		tilt, rem, err := DeciDegToMicroDeg(tiltDeci)
		if err != nil {
			return nil, err
		}
		traceScaled(p, id, "", path+".orientation.tilt_ddeg", SchemaV1, formatInt(tiltDeci), formatInt(tilt), "deci_deg_to_microdeg", RoundExact, rem)
		inner, rem, err := DeciDegToMicroDeg(innerDeci)
		if err != nil {
			return nil, err
		}
		traceScaled(p, id, "", path+".cone.inner_ddeg", SchemaV1, formatInt(innerDeci), formatInt(inner), "deci_deg_to_microdeg", RoundExact, rem)

		var outer int64
		if src.Cone.OuterDeciDeg == nil {
			outer, rem, err = ScaleInt(inner, 5, 2, RoundExact, path+".cone.outer_ddeg")
			if err != nil {
				return nil, err
			}
			addTrace(p, ConversionTrace{
				FixtureID: id, Field: path + ".cone.outer_ddeg", SourceVersion: SchemaV1,
				SourceValue: "<missing>", InternalValue: formatInt(outer), Operation: "apply_default",
				Rounding: string(RoundExact), Remainder: rem,
				DefaultSource: "v1 default: inner half angle * 2.5",
			})
		} else {
			outer, rem, err = DeciDegToMicroDeg(*src.Cone.OuterDeciDeg)
			if err != nil {
				return nil, err
			}
			traceScaled(p, id, "", path+".cone.outer_ddeg", SchemaV1, formatInt(*src.Cone.OuterDeciDeg), formatInt(outer), "deci_deg_to_microdeg", RoundExact, rem)
		}
		level, rem, err := PermilleToPermyriad(levelPermille)
		if err != nil {
			return nil, err
		}
		traceScaled(p, id, "", path+".level_permille", SchemaV1, formatInt(levelPermille), formatInt(level), "permille_to_permyriad", RoundExact, rem)
		rgb := Vec3{rgbPermille[0] * 10, rgbPermille[1] * 10, rgbPermille[2] * 10}
		traceExact(p, id, "", path+".rgb_permille", SchemaV1, formatRGB(rgbPermille), formatRGB(rgb), "rgb_permille_to_permyriad")
		posUM := Vec3{posMM[0] * 1000, posMM[1] * 1000, posMM[2] * 1000}
		traceExact(p, id, "", path+".position_mm", SchemaV1, formatVec(posMM), formatVec(posUM), "mm_to_um")

		p.Fixtures = append(p.Fixtures, Fixture{
			ID: id, PositionUM: posUM, PanMicroDeg: pan, TiltMicroDeg: tilt,
			InnerHalfMicroDeg: inner, OuterHalfMicroDeg: outer,
			CCTKelvin: cct, LevelPermyriad: level, RGBPermyriad: rgb,
		})
	}

	for i := range doc.Cues {
		src := &doc.Cues[i]
		path := fmt.Sprintf("cues[%d]", i)
		id, err := reqString(src.ID, SchemaV1, path+".id")
		if err != nil {
			return nil, err
		}
		fixtureID, err := reqString(src.Fixture, SchemaV1, path+".fixture")
		if err != nil {
			return nil, err
		}
		kindText, err := reqString(src.Kind, SchemaV1, path+".kind")
		if err != nil {
			return nil, err
		}
		kind := CueKind(kindText)
		startTick, err := reqInt(src.StartTick, SchemaV1, path+".start_tick")
		if err != nil {
			return nil, err
		}
		endTick, err := reqInt(src.EndTick, SchemaV1, path+".end_tick")
		if err != nil {
			return nil, err
		}
		startMS, rem, err := TickToMS(startTick, tickHz)
		if err != nil {
			return nil, err
		}
		traceScaled(p, fixtureID, id, path+".start_tick", SchemaV1, formatInt(startTick), formatInt(startMS), "tick_to_ms", RoundNearest, rem)
		endMS, rem, err := TickToMS(endTick, tickHz)
		if err != nil {
			return nil, err
		}
		traceScaled(p, fixtureID, id, path+".end_tick", SchemaV1, formatInt(endTick), formatInt(endMS), "tick_to_ms", RoundNearest, rem)
		cue := Cue{ID: id, FixtureID: fixtureID, Kind: kind, StartMS: startMS, EndMS: endMS}
		switch kind {
		case CueFade:
			from, err := reqInt(src.FromLevel, SchemaV1, path+".from_level_permille")
			if err != nil {
				return nil, err
			}
			to, err := reqInt(src.ToLevel, SchemaV1, path+".to_level_permille")
			if err != nil {
				return nil, err
			}
			cue.FromLevel, rem, err = PermilleToPermyriad(from)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".from_level_permille", SchemaV1, formatInt(from), formatInt(cue.FromLevel), "permille_to_permyriad", RoundExact, rem)
			cue.ToLevel, rem, err = PermilleToPermyriad(to)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".to_level_permille", SchemaV1, formatInt(to), formatInt(cue.ToLevel), "permille_to_permyriad", RoundExact, rem)
		case CueSweep:
			fromPan, err := reqInt(src.FromPanDeciDeg, SchemaV1, path+".from_pan_ddeg")
			if err != nil {
				return nil, err
			}
			toPan, err := reqInt(src.ToPanDeciDeg, SchemaV1, path+".to_pan_ddeg")
			if err != nil {
				return nil, err
			}
			fromTilt, err := reqInt(src.FromTiltDeciDeg, SchemaV1, path+".from_tilt_ddeg")
			if err != nil {
				return nil, err
			}
			toTilt, err := reqInt(src.ToTiltDeciDeg, SchemaV1, path+".to_tilt_ddeg")
			if err != nil {
				return nil, err
			}
			cue.FromPan, rem, err = DeciDegToMicroDeg(fromPan)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".from_pan_ddeg", SchemaV1, formatInt(fromPan), formatInt(cue.FromPan), "deci_deg_to_microdeg", RoundExact, rem)
			cue.ToPan, rem, err = DeciDegToMicroDeg(toPan)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".to_pan_ddeg", SchemaV1, formatInt(toPan), formatInt(cue.ToPan), "deci_deg_to_microdeg", RoundExact, rem)
			cue.FromTilt, rem, err = DeciDegToMicroDeg(fromTilt)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".from_tilt_ddeg", SchemaV1, formatInt(fromTilt), formatInt(cue.FromTilt), "deci_deg_to_microdeg", RoundExact, rem)
			cue.ToTilt, rem, err = DeciDegToMicroDeg(toTilt)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".to_tilt_ddeg", SchemaV1, formatInt(toTilt), formatInt(cue.ToTilt), "deci_deg_to_microdeg", RoundExact, rem)
		case CueColor:
			fromRGB, err := reqVec(src.FromRGB, 3, SchemaV1, path+".from_rgb_permille")
			if err != nil {
				return nil, err
			}
			toRGB, err := reqVec(src.ToRGB, 3, SchemaV1, path+".to_rgb_permille")
			if err != nil {
				return nil, err
			}
			cue.FromRGB = [3]int64{fromRGB[0] * 10, fromRGB[1] * 10, fromRGB[2] * 10}
			cue.ToRGB = [3]int64{toRGB[0] * 10, toRGB[1] * 10, toRGB[2] * 10}
			traceExact(p, fixtureID, id, path+".from_rgb_permille", SchemaV1, formatRGB(fromRGB), formatRGB(cue.FromRGB), "rgb_permille_to_permyriad")
			traceExact(p, fixtureID, id, path+".to_rgb_permille", SchemaV1, formatRGB(toRGB), formatRGB(cue.ToRGB), "rgb_permille_to_permyriad")
		default:
			return nil, schemaError(SchemaV1, path+".kind", "unknown cue kind")
		}
		p.Cues = append(p.Cues, cue)
	}

	if doc.Timeline == nil {
		return nil, schemaError(SchemaV1, "timeline", "missing timeline")
	}
	durationTick, err := reqInt(doc.Timeline.DurationTick, SchemaV1, "timeline.duration_tick")
	if err != nil {
		return nil, err
	}
	var rem int64
	p.Timeline.DurationMS, rem, err = TickToMS(durationTick, tickHz)
	if err != nil {
		return nil, err
	}
	traceScaled(p, "", "", "timeline.duration_tick", SchemaV1, formatInt(durationTick), formatInt(p.Timeline.DurationMS), "tick_to_ms", RoundNearest, rem)
	for i := range doc.Timeline.Windows {
		src := &doc.Timeline.Windows[i]
		path := fmt.Sprintf("timeline.windows[%d]", i)
		if src.Number == nil {
			return nil, schemaError(SchemaV1, path+".number", "missing window number")
		}
		startTick, err := reqInt(src.StartTick, SchemaV1, path+".start_tick")
		if err != nil {
			return nil, err
		}
		endTick, err := reqInt(src.EndTick, SchemaV1, path+".end_tick")
		if err != nil {
			return nil, err
		}
		w := SampleWindow{Number: *src.Number}
		w.StartMS, rem, err = TickToMS(startTick, tickHz)
		if err != nil {
			return nil, err
		}
		traceScaled(p, "", "", path+".start_tick", SchemaV1, formatInt(startTick), formatInt(w.StartMS), "tick_to_ms", RoundNearest, rem)
		w.EndMS, rem, err = TickToMS(endTick, tickHz)
		if err != nil {
			return nil, err
		}
		traceScaled(p, "", "", path+".end_tick", SchemaV1, formatInt(endTick), formatInt(w.EndMS), "tick_to_ms", RoundNearest, rem)
		if len(src.SamplesTick) == 0 {
			return nil, schemaError(SchemaV1, path+".samples_tick", "missing sample times")
		}
		for j, tick := range src.SamplesTick {
			ms, rem, err := TickToMS(tick, tickHz)
			if err != nil {
				return nil, err
			}
			traceScaled(p, "", "", fmt.Sprintf("%s.samples_tick[%d]", path, j), SchemaV1, formatInt(tick), formatInt(ms), "tick_to_ms", RoundNearest, rem)
			w.Samples = append(w.Samples, ms)
		}
		p.Timeline.Windows = append(p.Timeline.Windows, w)
	}
	sortProjectCues(p)
	return p, nil
}
