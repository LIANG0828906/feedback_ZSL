package stage

import (
	"encoding/json"
	"fmt"
)

type v3Doc struct {
	Schema           *string     `json:"schema"`
	ProjectID        *string     `json:"project_id"`
	Seed             *int64      `json:"seed"`
	ParameterVersion *string     `json:"parameter_version"`
	Rigs             []v3Rig     `json:"rigs"`
	Cues             []v3Cue     `json:"cues"`
	Timeline         *v3Timeline `json:"timeline"`
}

type v3Rig struct {
	Name   *string   `json:"name"`
	Groups []v3Group `json:"groups"`
}

type v3Group struct {
	Name     *string     `json:"name"`
	Label    *string     `json:"label,omitempty"`
	Fixtures []v3Fixture `json:"fixtures"`
}

type v3Fixture struct {
	ID           *string `json:"id"`
	PositionMM   []int64 `json:"position_mm"`
	PanMilliDeg  *int64  `json:"pan_mdeg"`
	TiltMilliDeg *int64  `json:"tilt_mdeg"`
	Cone         *v3Cone `json:"cone"`
	CCTMyriad    *int64  `json:"cct_myriad"`
	LevelMyriad  *int64  `json:"level_myriad"`
	RGBMyriad    []int64 `json:"rgb_myriad"`
}

type v3Cone struct {
	InnerHalfMilliDeg *int64 `json:"inner_half_mdeg"`
	OuterHalfMilliDeg *int64 `json:"outer_half_mdeg"`
}

type v3Cue struct {
	ID               *string `json:"id"`
	Fixture          *string `json:"fixture"`
	Kind             *string `json:"kind"`
	StartMS          *int64  `json:"start_ms"`
	EndMS            *int64  `json:"end_ms"`
	FromLevel        *int64  `json:"from_level_myriad,omitempty"`
	ToLevel          *int64  `json:"to_level_myriad,omitempty"`
	FromPanMilliDeg  *int64  `json:"from_pan_mdeg,omitempty"`
	ToPanMilliDeg    *int64  `json:"to_pan_mdeg,omitempty"`
	FromTiltMilliDeg *int64  `json:"from_tilt_mdeg,omitempty"`
	ToTiltMilliDeg   *int64  `json:"to_tilt_mdeg,omitempty"`
	FromRGB          []int64 `json:"from_rgb_myriad,omitempty"`
	ToRGB            []int64 `json:"to_rgb_myriad,omitempty"`
}

type v3Timeline struct {
	DurationMS *int64     `json:"duration_ms"`
	Windows    []v3Window `json:"windows"`
}

type v3Window struct {
	Number    *int    `json:"number"`
	StartMS   *int64  `json:"start_ms"`
	EndMS     *int64  `json:"end_ms"`
	SamplesMS []int64 `json:"samples_ms"`
}

func loadV3(data []byte) (*Project, error) {
	var doc v3Doc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, schemaError(SchemaV3, "$", "invalid v3 JSON: "+err.Error())
	}
	projectID, err := reqString(doc.ProjectID, SchemaV3, "project_id")
	if err != nil {
		return nil, err
	}
	paramVersion, err := reqString(doc.ParameterVersion, SchemaV3, "parameter_version")
	if err != nil {
		return nil, err
	}
	seed, err := reqInt(doc.Seed, SchemaV3, "seed")
	if err != nil {
		return nil, err
	}
	if len(doc.Rigs) == 0 {
		return nil, schemaError(SchemaV3, "rigs", "rig array is empty")
	}
	p := &Project{ProjectID: projectID, SourceVersion: SchemaV3, Seed: seed, ParameterVersion: paramVersion, Revision: 1}
	for ri := range doc.Rigs {
		rig := &doc.Rigs[ri]
		rigPath := fmt.Sprintf("rigs[%d]", ri)
		rigName, err := reqString(rig.Name, SchemaV3, rigPath+".name")
		if err != nil {
			return nil, err
		}
		if len(rig.Groups) == 0 {
			return nil, schemaError(SchemaV3, rigPath+".groups", "group array is empty")
		}
		for gi := range rig.Groups {
			group := &rig.Groups[gi]
			groupPath := fmt.Sprintf("%s.groups[%d]", rigPath, gi)
			groupName, err := reqString(group.Name, SchemaV3, groupPath+".name")
			if err != nil {
				return nil, err
			}
			label := ""
			if group.Label != nil {
				label = *group.Label
			}
			p.Groups = append(p.Groups, FixtureGroup{Rig: rigName, Name: groupName, Label: label})
			if len(group.Fixtures) == 0 {
				return nil, schemaError(SchemaV3, groupPath+".fixtures", "fixture array is empty")
			}
			for fi := range group.Fixtures {
				src := &group.Fixtures[fi]
				path := fmt.Sprintf("%s.fixtures[%d]", groupPath, fi)
				id, err := reqString(src.ID, SchemaV3, path+".id")
				if err != nil {
					return nil, err
				}
				posMM, err := reqVec(src.PositionMM, 3, SchemaV3, path+".position_mm")
				if err != nil {
					return nil, err
				}
				panMdeg, err := reqInt(src.PanMilliDeg, SchemaV3, path+".pan_mdeg")
				if err != nil {
					return nil, err
				}
				tiltMdeg, err := reqInt(src.TiltMilliDeg, SchemaV3, path+".tilt_mdeg")
				if err != nil {
					return nil, err
				}
				if src.Cone == nil {
					return nil, schemaError(SchemaV3, path+".cone", "missing cone")
				}
				innerMdeg, err := reqInt(src.Cone.InnerHalfMilliDeg, SchemaV3, path+".cone.inner_half_mdeg")
				if err != nil {
					return nil, err
				}
				outerMdeg, err := reqInt(src.Cone.OuterHalfMilliDeg, SchemaV3, path+".cone.outer_half_mdeg")
				if err != nil {
					return nil, err
				}
				cctMyriad, err := reqInt(src.CCTMyriad, SchemaV3, path+".cct_myriad")
				if err != nil {
					return nil, err
				}
				level, err := reqInt(src.LevelMyriad, SchemaV3, path+".level_myriad")
				if err != nil {
					return nil, err
				}
				rgb, err := reqVec(src.RGBMyriad, 3, SchemaV3, path+".rgb_myriad")
				if err != nil {
					return nil, err
				}
				pan, rem, err := MilliDegToMicroDeg(panMdeg)
				if err != nil {
					return nil, err
				}
				traceScaled(p, id, "", path+".pan_mdeg", SchemaV3, formatInt(panMdeg), formatInt(pan), "milli_deg_to_microdeg", RoundExact, rem)
				tilt, rem, err := MilliDegToMicroDeg(tiltMdeg)
				if err != nil {
					return nil, err
				}
				traceScaled(p, id, "", path+".tilt_mdeg", SchemaV3, formatInt(tiltMdeg), formatInt(tilt), "milli_deg_to_microdeg", RoundExact, rem)
				inner, rem, err := MilliDegToMicroDeg(innerMdeg)
				if err != nil {
					return nil, err
				}
				traceScaled(p, id, "", path+".cone.inner_half_mdeg", SchemaV3, formatInt(innerMdeg), formatInt(inner), "milli_deg_to_microdeg", RoundExact, rem)
				outer, rem, err := MilliDegToMicroDeg(outerMdeg)
				if err != nil {
					return nil, err
				}
				traceScaled(p, id, "", path+".cone.outer_half_mdeg", SchemaV3, formatInt(outerMdeg), formatInt(outer), "milli_deg_to_microdeg", RoundExact, rem)
				cct, rem, err := CCTPermyriadToKelvin(cctMyriad)
				if err != nil {
					return nil, err
				}
				traceScaled(p, id, "", path+".cct_myriad", SchemaV3, formatInt(cctMyriad), formatInt(cct), "cct_permyriad_to_kelvin", RoundNearest, rem)
				posUM := Vec3{posMM[0] * 1000, posMM[1] * 1000, posMM[2] * 1000}
				traceExact(p, id, "", path+".position_mm", SchemaV3, formatVec(posMM), formatVec(posUM), "mm_to_um")
				traceExact(p, id, "", path+".level_myriad", SchemaV3, formatInt(level), formatInt(level), "permyriad_identity")
				traceExact(p, id, "", path+".rgb_myriad", SchemaV3, formatRGB(rgb), formatRGB(rgb), "permyriad_identity")
				p.Fixtures = append(p.Fixtures, Fixture{
					ID: id, Rig: rigName, Group: groupName, PositionUM: posUM,
					PanMicroDeg: pan, TiltMicroDeg: tilt,
					InnerHalfMicroDeg: inner, OuterHalfMicroDeg: outer,
					CCTKelvin: cct, LevelPermyriad: level, RGBPermyriad: rgb,
				})
			}
		}
	}

	for i := range doc.Cues {
		src := &doc.Cues[i]
		path := fmt.Sprintf("cues[%d]", i)
		id, err := reqString(src.ID, SchemaV3, path+".id")
		if err != nil {
			return nil, err
		}
		fixtureID, err := reqString(src.Fixture, SchemaV3, path+".fixture")
		if err != nil {
			return nil, err
		}
		kindText, err := reqString(src.Kind, SchemaV3, path+".kind")
		if err != nil {
			return nil, err
		}
		start, err := reqInt(src.StartMS, SchemaV3, path+".start_ms")
		if err != nil {
			return nil, err
		}
		end, err := reqInt(src.EndMS, SchemaV3, path+".end_ms")
		if err != nil {
			return nil, err
		}
		cue := Cue{ID: id, FixtureID: fixtureID, Kind: CueKind(kindText), StartMS: start, EndMS: end}
		switch cue.Kind {
		case CueFade:
			from, err := reqInt(src.FromLevel, SchemaV3, path+".from_level_myriad")
			if err != nil {
				return nil, err
			}
			to, err := reqInt(src.ToLevel, SchemaV3, path+".to_level_myriad")
			if err != nil {
				return nil, err
			}
			cue.FromLevel, cue.ToLevel = from, to
			traceExact(p, fixtureID, id, path+".from_level_myriad", SchemaV3, formatInt(from), formatInt(from), "permyriad_identity")
			traceExact(p, fixtureID, id, path+".to_level_myriad", SchemaV3, formatInt(to), formatInt(to), "permyriad_identity")
		case CueSweep:
			fromPan, err := reqInt(src.FromPanMilliDeg, SchemaV3, path+".from_pan_mdeg")
			if err != nil {
				return nil, err
			}
			toPan, err := reqInt(src.ToPanMilliDeg, SchemaV3, path+".to_pan_mdeg")
			if err != nil {
				return nil, err
			}
			fromTilt, err := reqInt(src.FromTiltMilliDeg, SchemaV3, path+".from_tilt_mdeg")
			if err != nil {
				return nil, err
			}
			toTilt, err := reqInt(src.ToTiltMilliDeg, SchemaV3, path+".to_tilt_mdeg")
			if err != nil {
				return nil, err
			}
			var rem int64
			cue.FromPan, rem, err = MilliDegToMicroDeg(fromPan)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".from_pan_mdeg", SchemaV3, formatInt(fromPan), formatInt(cue.FromPan), "milli_deg_to_microdeg", RoundExact, rem)
			cue.ToPan, rem, err = MilliDegToMicroDeg(toPan)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".to_pan_mdeg", SchemaV3, formatInt(toPan), formatInt(cue.ToPan), "milli_deg_to_microdeg", RoundExact, rem)
			cue.FromTilt, rem, err = MilliDegToMicroDeg(fromTilt)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".from_tilt_mdeg", SchemaV3, formatInt(fromTilt), formatInt(cue.FromTilt), "milli_deg_to_microdeg", RoundExact, rem)
			cue.ToTilt, rem, err = MilliDegToMicroDeg(toTilt)
			if err != nil {
				return nil, err
			}
			traceScaled(p, fixtureID, id, path+".to_tilt_mdeg", SchemaV3, formatInt(toTilt), formatInt(cue.ToTilt), "milli_deg_to_microdeg", RoundExact, rem)
		case CueColor:
			fromRGB, err := reqVec(src.FromRGB, 3, SchemaV3, path+".from_rgb_myriad")
			if err != nil {
				return nil, err
			}
			toRGB, err := reqVec(src.ToRGB, 3, SchemaV3, path+".to_rgb_myriad")
			if err != nil {
				return nil, err
			}
			cue.FromRGB = [3]int64{fromRGB[0], fromRGB[1], fromRGB[2]}
			cue.ToRGB = [3]int64{toRGB[0], toRGB[1], toRGB[2]}
			traceExact(p, fixtureID, id, path+".from_rgb_myriad", SchemaV3, formatRGB(fromRGB), formatRGB(cue.FromRGB), "permyriad_identity")
			traceExact(p, fixtureID, id, path+".to_rgb_myriad", SchemaV3, formatRGB(toRGB), formatRGB(cue.ToRGB), "permyriad_identity")
		default:
			return nil, schemaError(SchemaV3, path+".kind", "unknown cue kind")
		}
		p.Cues = append(p.Cues, cue)
	}

	if doc.Timeline == nil {
		return nil, schemaError(SchemaV3, "timeline", "missing timeline")
	}
	duration, err := reqInt(doc.Timeline.DurationMS, SchemaV3, "timeline.duration_ms")
	if err != nil {
		return nil, err
	}
	p.Timeline.DurationMS = duration
	traceExact(p, "", "", "timeline.duration_ms", SchemaV3, formatInt(duration), formatInt(duration), "ms_identity")
	for i := range doc.Timeline.Windows {
		src := &doc.Timeline.Windows[i]
		path := fmt.Sprintf("timeline.windows[%d]", i)
		if src.Number == nil {
			return nil, schemaError(SchemaV3, path+".number", "missing window number")
		}
		start, err := reqInt(src.StartMS, SchemaV3, path+".start_ms")
		if err != nil {
			return nil, err
		}
		end, err := reqInt(src.EndMS, SchemaV3, path+".end_ms")
		if err != nil {
			return nil, err
		}
		if len(src.SamplesMS) == 0 {
			return nil, schemaError(SchemaV3, path+".samples_ms", "missing sample times")
		}
		w := SampleWindow{Number: *src.Number, StartMS: start, EndMS: end, Samples: append([]int64(nil), src.SamplesMS...)}
		traceExact(p, "", "", path+".start_ms", SchemaV3, formatInt(start), formatInt(start), "ms_identity")
		traceExact(p, "", "", path+".end_ms", SchemaV3, formatInt(end), formatInt(end), "ms_identity")
		p.Timeline.Windows = append(p.Timeline.Windows, w)
	}
	sortProjectCues(p)
	return p, nil
}
