package stage

import (
	"encoding/json"
	"fmt"
)

type v2Doc struct {
	Schema           *string     `json:"schema"`
	ProjectID        *string     `json:"project_id"`
	Seed             *int64      `json:"seed"`
	ParameterVersion *string     `json:"parameter_version"`
	Fixtures         []v2Fixture `json:"fixtures"`
	Cues             []v2Cue     `json:"cues"`
	Timeline         *v2Timeline `json:"timeline"`
}

type v2Fixture struct {
	UID         *string `json:"uid"`
	PositionMM  []int64 `json:"position_mm"`
	Aim         *v2Aim  `json:"aim"`
	Beam        *v2Beam `json:"beam"`
	CCTKelvin   *int64  `json:"cct_kelvin"`
	LevelMyriad *int64  `json:"level_myriad"`
	ColorHex    *string `json:"color_hex"`
}

type v2Aim struct {
	PanUrad  *int64 `json:"pan_urad"`
	TiltUrad *int64 `json:"tilt_urad"`
}

type v2Beam struct {
	InnerHalfUrad *int64 `json:"inner_half_urad"`
	OuterHalfUrad *int64 `json:"outer_half_urad"`
}

type v2Cue struct {
	ID           *string `json:"id"`
	Fixture      *string `json:"fixture"`
	Kind         *string `json:"kind"`
	StartUS      *int64  `json:"start_us"`
	EndUS        *int64  `json:"end_us"`
	FromLevel    *int64  `json:"from_level_myriad,omitempty"`
	ToLevel      *int64  `json:"to_level_myriad,omitempty"`
	FromPanUrad  *int64  `json:"from_pan_urad,omitempty"`
	ToPanUrad    *int64  `json:"to_pan_urad,omitempty"`
	FromTiltUrad *int64  `json:"from_tilt_urad,omitempty"`
	ToTiltUrad   *int64  `json:"to_tilt_urad,omitempty"`
	FromColorHex *string `json:"from_color_hex,omitempty"`
	ToColorHex   *string `json:"to_color_hex,omitempty"`
}

type v2Timeline struct {
	DurationUS *int64     `json:"duration_us"`
	Windows    []v2Window `json:"windows"`
}

type v2Window struct {
	Number    *int    `json:"number"`
	StartUS   *int64  `json:"start_us"`
	EndUS     *int64  `json:"end_us"`
	SamplesUS []int64 `json:"samples_us"`
}

func loadV2(data []byte) (*Project, error) {
	var doc v2Doc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, schemaError(SchemaV2, "$", "invalid v2 JSON: "+err.Error())
	}
	projectID, err := reqString(doc.ProjectID, SchemaV2, "project_id")
	if err != nil {
		return nil, err
	}
	paramVersion, err := reqString(doc.ParameterVersion, SchemaV2, "parameter_version")
	if err != nil {
		return nil, err
	}
	seed, err := reqInt(doc.Seed, SchemaV2, "seed")
	if err != nil {
		return nil, err
	}
	if len(doc.Fixtures) == 0 {
		return nil, schemaError(SchemaV2, "fixtures", "fixture array is empty")
	}
	p := &Project{ProjectID: projectID, SourceVersion: SchemaV2, Seed: seed, ParameterVersion: paramVersion, Revision: 1}

	for i := range doc.Fixtures {
		src := &doc.Fixtures[i]
		path := fmt.Sprintf("fixtures[%d]", i)
		id, err := reqString(src.UID, SchemaV2, path+".uid")
		if err != nil {
			return nil, err
		}
		posMM, err := reqVec(src.PositionMM, 3, SchemaV2, path+".position_mm")
		if err != nil {
			return nil, err
		}
		if src.Aim == nil {
			return nil, schemaError(SchemaV2, path+".aim", "missing aim")
		}
		panUrad, err := reqInt(src.Aim.PanUrad, SchemaV2, path+".aim.pan_urad")
		if err != nil {
			return nil, err
		}
		tiltUrad, err := reqInt(src.Aim.TiltUrad, SchemaV2, path+".aim.tilt_urad")
		if err != nil {
			return nil, err
		}
		if src.Beam == nil {
			return nil, schemaError(SchemaV2, path+".beam", "missing beam")
		}
		innerUrad, err := reqInt(src.Beam.InnerHalfUrad, SchemaV2, path+".beam.inner_half_urad")
		if err != nil {
			return nil, err
		}
		outerUrad, err := reqInt(src.Beam.OuterHalfUrad, SchemaV2, path+".beam.outer_half_urad")
		if err != nil {
			return nil, err
		}
		cct, err := reqInt(src.CCTKelvin, SchemaV2, path+".cct_kelvin")
		if err != nil {
			return nil, err
		}
		level, err := reqInt(src.LevelMyriad, SchemaV2, path+".level_myriad")
		if err != nil {
			return nil, err
		}
		colorText, err := reqString(src.ColorHex, SchemaV2, path+".color_hex")
		if err != nil {
			return nil, err
		}
		rgb, err := parseHexRGB(colorText, SchemaV2, path+".color_hex")
		if err != nil {
			return nil, err
		}

		pan, rem, err := MicroRadToMicroDeg(panUrad)
		if err != nil {
			return nil, err
		}
		traceScaled(p, id, "", path+".aim.pan_urad", SchemaV2, formatInt(panUrad), formatInt(pan), "urad_to_microdeg", RoundNearest, rem)
		tilt, rem, err := MicroRadToMicroDeg(tiltUrad)
		if err != nil {
			return nil, err
		}
		traceScaled(p, id, "", path+".aim.tilt_urad", SchemaV2, formatInt(tiltUrad), formatInt(tilt), "urad_to_microdeg", RoundNearest, rem)
		inner, rem, err := MicroRadToMicroDeg(innerUrad)
		if err != nil {
			return nil, err
		}
		traceScaled(p, id, "", path+".beam.inner_half_urad", SchemaV2, formatInt(innerUrad), formatInt(inner), "urad_to_microdeg", RoundNearest, rem)
		outer, rem, err := MicroRadToMicroDeg(outerUrad)
		if err != nil {
			return nil, err
		}
		traceScaled(p, id, "", path+".beam.outer_half_urad", SchemaV2, formatInt(outerUrad), formatInt(outer), "urad_to_microdeg", RoundNearest, rem)
		traceExact(p, id, "", path+".cct_kelvin", SchemaV2, formatInt(cct), formatInt(cct), "kelvin_identity")
		traceExact(p, id, "", path+".level_myriad", SchemaV2, formatInt(level), formatInt(level), "permyriad_identity")
		traceExact(p, id, "", path+".color_hex", SchemaV2, colorText, formatRGB(rgb), "rgb_hex_to_permyriad")
		posUM := Vec3{posMM[0] * 1000, posMM[1] * 1000, posMM[2] * 1000}
		traceExact(p, id, "", path+".position_mm", SchemaV2, formatVec(posMM), formatVec(posUM), "mm_to_um")
		p.Fixtures = append(p.Fixtures, Fixture{
			ID: id, PositionUM: posUM, PanMicroDeg: pan, TiltMicroDeg: tilt,
			InnerHalfMicroDeg: inner, OuterHalfMicroDeg: outer,
			CCTKelvin: cct, LevelPermyriad: level, RGBPermyriad: rgb,
		})
	}

	for i := range doc.Cues {
		src := &doc.Cues[i]
		path := fmt.Sprintf("cues[%d]", i)
		id, err := reqString(src.ID, SchemaV2, path+".id")
		if err != nil {
			return nil, err
		}
		fixtureID, err := reqString(src.Fixture, SchemaV2, path+".fixture")
		if err != nil {
			return nil, err
		}
		kindText, err := reqString(src.Kind, SchemaV2, path+".kind")
		if err != nil {
			return nil, err
		}
		startUS, err := reqInt(src.StartUS, SchemaV2, path+".start_us")
		if err != nil {
			return nil, err
		}
		endUS, err := reqInt(src.EndUS, SchemaV2, path+".end_us")
		if err != nil {
			return nil, err
		}
		startMS, rem, err := MicrosecondToMS(startUS)
		if err != nil {
			return nil, err
		}
		traceScaled(p, fixtureID, id, path+".start_us", SchemaV2, formatInt(startUS), formatInt(startMS), "us_to_ms", RoundNearest, rem)
		endMS, rem, err := MicrosecondToMS(endUS)
		if err != nil {
			return nil, err
		}
		traceScaled(p, fixtureID, id, path+".end_us", SchemaV2, formatInt(endUS), formatInt(endMS), "us_to_ms", RoundNearest, rem)
		cue := Cue{ID: id, FixtureID: fixtureID, Kind: CueKind(kindText), StartMS: startMS, EndMS: endMS}
		switch cue.Kind {
		case CueFade:
			from, err := reqInt(src.FromLevel, SchemaV2, path+".from_level_myriad")
			if err != nil {
				return nil, err
			}
			to, err := reqInt(src.ToLevel, SchemaV2, path+".to_level_myriad")
			if err != nil {
				return nil, err
			}
			cue.FromLevel, cue.ToLevel = from, to
			traceExact(p, fixtureID, id, path+".from_level_myriad", SchemaV2, formatInt(from), formatInt(from), "permyriad_identity")
			traceExact(p, fixtureID, id, path+".to_level_myriad", SchemaV2, formatInt(to), formatInt(to), "permyriad_identity")
		case CueSweep:
			fromPan, err := reqInt(src.FromPanUrad, SchemaV2, path+".from_pan_urad")
			if err != nil {
				return nil, err
			}
			toPan, err := reqInt(src.ToPanUrad, SchemaV2, path+".to_pan_urad")
			if err != nil {
				return nil, err
			}
			fromTilt, err := reqInt(src.FromTiltUrad, SchemaV2, path+".from_tilt_urad")
			if err != nil {
				return nil, err
			}
			toTilt, err := reqInt(src.ToTiltUrad, SchemaV2, path+".to_tilt_urad")
			if err != nil {
				return nil, err
			}
			cue.FromPan, rem, err = MicroRadToMicroDeg(fromPan)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".from_pan_urad", SchemaV2, formatInt(fromPan), formatInt(cue.FromPan), "urad_to_microdeg", RoundNearest, rem)
			cue.ToPan, rem, err = MicroRadToMicroDeg(toPan)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".to_pan_urad", SchemaV2, formatInt(toPan), formatInt(cue.ToPan), "urad_to_microdeg", RoundNearest, rem)
			cue.FromTilt, rem, err = MicroRadToMicroDeg(fromTilt)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".from_tilt_urad", SchemaV2, formatInt(fromTilt), formatInt(cue.FromTilt), "urad_to_microdeg", RoundNearest, rem)
			cue.ToTilt, rem, err = MicroRadToMicroDeg(toTilt)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".to_tilt_urad", SchemaV2, formatInt(toTilt), formatInt(cue.ToTilt), "urad_to_microdeg", RoundNearest, rem)
		case CueColor:
			fromText, err := reqString(src.FromColorHex, SchemaV2, path+".from_color_hex")
			if err != nil {
				return nil, err
			}
			toText, err := reqString(src.ToColorHex, SchemaV2, path+".to_color_hex")
			if err != nil {
				return nil, err
			}
			from, err := parseHexRGB(fromText, SchemaV2, path+".from_color_hex")
			if err != nil {
				return nil, err
			}
			to, err := parseHexRGB(toText, SchemaV2, path+".to_color_hex")
			if err != nil {
				return nil, err
			}
			cue.FromRGB, cue.ToRGB = from, to
			traceExact(p, fixtureID, id, path+".from_color_hex", SchemaV2, fromText, formatRGB(from), "rgb_hex_to_permyriad")
			traceExact(p, fixtureID, id, path+".to_color_hex", SchemaV2, toText, formatRGB(to), "rgb_hex_to_permyriad")
		default:
			return nil, schemaError(SchemaV2, path+".kind", "unknown cue kind")
		}
		p.Cues = append(p.Cues, cue)
	}

	if doc.Timeline == nil {
		return nil, schemaError(SchemaV2, "timeline", "missing timeline")
	}
	durationUS, err := reqInt(doc.Timeline.DurationUS, SchemaV2, "timeline.duration_us")
	if err != nil {
		return nil, err
	}
	var rem int64
	p.Timeline.DurationMS, rem, err = MicrosecondToMS(durationUS)
	if err != nil {
		return nil, err
	}
	traceScaled(p, "", "", "timeline.duration_us", SchemaV2, formatInt(durationUS), formatInt(p.Timeline.DurationMS), "us_to_ms", RoundNearest, rem)
	for i := range doc.Timeline.Windows {
		src := &doc.Timeline.Windows[i]
		path := fmt.Sprintf("timeline.windows[%d]", i)
		if src.Number == nil {
			return nil, schemaError(SchemaV2, path+".number", "missing window number")
		}
		startUS, err := reqInt(src.StartUS, SchemaV2, path+".start_us")
		if err != nil {
			return nil, err
		}
		endUS, err := reqInt(src.EndUS, SchemaV2, path+".end_us")
		if err != nil {
			return nil, err
		}
		w := SampleWindow{Number: *src.Number}
		w.StartMS, rem, err = MicrosecondToMS(startUS)
		if err != nil {
			return nil, err
		}
		traceScaled(p, "", "", path+".start_us", SchemaV2, formatInt(startUS), formatInt(w.StartMS), "us_to_ms", RoundNearest, rem)
		w.EndMS, rem, err = MicrosecondToMS(endUS)
		if err != nil {
			return nil, err
		}
		traceScaled(p, "", "", path+".end_us", SchemaV2, formatInt(endUS), formatInt(w.EndMS), "us_to_ms", RoundNearest, rem)
		if len(src.SamplesUS) == 0 {
			return nil, schemaError(SchemaV2, path+".samples_us", "missing sample times")
		}
		for j, us := range src.SamplesUS {
			ms, rem, err := MicrosecondToMS(us)
			if err != nil {
				return nil, err
			}
			traceScaled(p, "", "", fmt.Sprintf("%s.samples_us[%d]", path, j), SchemaV2, formatInt(us), formatInt(ms), "us_to_ms", RoundNearest, rem)
			w.Samples = append(w.Samples, ms)
		}
		p.Timeline.Windows = append(p.Timeline.Windows, w)
	}
	sortProjectCues(p)
	return p, nil
}
