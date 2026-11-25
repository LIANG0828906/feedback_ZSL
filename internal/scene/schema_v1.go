package scene

import (
	"encoding/json"
	"fmt"

	"ms-workbench/internal/apperr"
)

func (d decoder) decodeV1(raw object) (*Scene, error) {
	s := &Scene{Present: map[string]bool{}, Settings: DefaultSettings()}
	var err error
	s.SceneID, err = d.strField(raw, "scene_id", "$.scene_id")
	if err != nil {
		return nil, err
	}
	s.Name, err = d.strField(raw, "name", "$.name")
	if err != nil {
		return nil, err
	}
	size, err := d.objField(raw, "pixel_size", "$.pixel_size")
	if err != nil {
		return nil, err
	}
	w, err := d.intField(size, "w", "$.pixel_size.w")
	if err != nil {
		return nil, err
	}
	h, err := d.intField(size, "h", "$.pixel_size.h")
	if err != nil {
		return nil, err
	}
	s.Width, s.Height = int(w), int(h)
	s.PixelPitchUM, err = d.numField(raw, "pixel_pitch_um", "$.pixel_pitch_um")
	if err != nil {
		return nil, err
	}
	bits, err := d.intField(raw, "bit_depth", "$.bit_depth")
	if err != nil {
		return nil, err
	}
	s.BitDepth = int(bits)
	white, err := d.objField(raw, "white_point", "$.white_point")
	if err != nil {
		return nil, err
	}
	s.WhitePoint[0], err = d.numField(white, "x", "$.white_point.x")
	if err != nil {
		return nil, err
	}
	s.WhitePoint[1], err = d.numField(white, "y", "$.white_point.y")
	if err != nil {
		return nil, err
	}
	s.WhitePoint[2], err = d.numField(white, "z", "$.white_point.z")
	if err != nil {
		return nil, err
	}
	camera, err := d.objField(raw, "camera_response", "$.camera_response")
	if err != nil {
		return nil, err
	}
	s.Response.InverseGamma, err = d.numField(camera, "inverse_gamma", "$.camera_response.inverse_gamma")
	if err != nil {
		return nil, err
	}
	s.Response.FullWellRadiance, err = d.numField(camera, "full_well_radiance", "$.camera_response.full_well_radiance")
	if err != nil {
		return nil, err
	}
	s.Response.BlackLevel, err = d.numField(camera, "black_level", "$.camera_response.black_level")
	if err != nil {
		return nil, err
	}
	bandsRaw, err := d.arrField(raw, "bands", "$.bands")
	if err != nil {
		return nil, err
	}
	s.Bands = make([]Band, len(bandsRaw))
	for i, item := range bandsRaw {
		bp := fmt.Sprintf("$.bands[%d]", i)
		bo, err := d.object(item, bp)
		if err != nil {
			return nil, err
		}
		b := Band{}
		b.ID, err = d.strField(bo, "id", bp+".id")
		if err != nil {
			return nil, err
		}
		b.CenterNM, err = d.numField(bo, "center_nm", bp+".center_nm")
		if err != nil {
			return nil, err
		}
		rr, err := d.require(bo, "response_xyz", bp+".response_xyz")
		if err != nil {
			return nil, err
		}
		b.ResponseXYZ, err = d.vec3(rr, bp+".response_xyz")
		if err != nil {
			return nil, err
		}
		off, err := d.require(bo, "offset_px", bp+".offset_px")
		if err != nil {
			return nil, err
		}
		offVals, err := d.floatArray(off, bp+".offset_px")
		if err != nil {
			return nil, err
		}
		if len(offVals) != 2 {
			return nil, apperr.Schema(d.version, bp+".offset_px", "expected two values")
		}
		b.OffsetX, b.OffsetY = offVals[0], offVals[1]
		detailRaw, err := d.require(bo, "detail", bp+".detail")
		if err != nil {
			return nil, err
		}
		b.Detail, err = d.floatArray(detailRaw, bp+".detail")
		if err != nil {
			return nil, err
		}
		if err := checkLength(s, b.ID, "detail", len(b.Detail), d.version); err != nil {
			return nil, err
		}
		expRaw, err := d.arrField(bo, "exposures", bp+".exposures")
		if err != nil {
			return nil, err
		}
		b.Exposures = make([]Exposure, len(expRaw))
		for j, expItem := range expRaw {
			ep := fmt.Sprintf("%s.exposures[%d]", bp, j)
			eo, err := d.object(expItem, ep)
			if err != nil {
				return nil, err
			}
			b.Exposures[j].DurationUS, err = d.timeMicros(eo, "duration_us", ep+".duration_us")
			if err != nil {
				return nil, err
			}
			rawCodes, err := d.require(eo, "codes", ep+".codes")
			if err != nil {
				return nil, err
			}
			b.Exposures[j].Codes, err = d.codes(rawCodes, ep+".codes")
			if err != nil {
				return nil, err
			}
			if err := checkLength(s, b.ID, fmt.Sprintf("exposures[%d].codes", j), len(b.Exposures[j].Codes), d.version); err != nil {
				return nil, err
			}
		}
		s.Bands[i] = b
	}
	patchRaw, err := d.require(raw, "patches", "$.patches")
	if err != nil {
		return nil, err
	}
	s.Patches, err = parsePatchArray(d, patchRaw, "$.patches")
	if err != nil {
		return nil, err
	}
	truthRaw, err := d.require(raw, "truth", "$.truth")
	if err != nil {
		return nil, err
	}
	s.Truth, err = parseTruth(d, truthRaw, "$.truth")
	if err != nil {
		return nil, err
	}
	for path := range v2SettingsDefaults() {
		s.Present[path] = false
	}
	for path := range v3SettingsDefaults() {
		s.Present[path] = false
	}
	return d.finish(s)
}

func marshalJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
