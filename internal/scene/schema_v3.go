package scene

import (
	"fmt"

	"ms-workbench/internal/apperr"
)

func (d decoder) decodeV3(raw object) (*Scene, error) {
	s := &Scene{Present: map[string]bool{}, Settings: DefaultSettings()}
	var err error
	sceneObj, err := d.objField(raw, "scene", "$.scene")
	if err != nil {
		return nil, err
	}
	s.SceneID, err = d.strField(sceneObj, "id", "$.scene.id")
	if err != nil {
		return nil, err
	}
	s.Name, err = d.strField(sceneObj, "name", "$.scene.name")
	if err != nil {
		return nil, err
	}
	sizeRaw, err := d.require(raw, "size", "$.size")
	if err != nil {
		return nil, err
	}
	size, err := d.array(sizeRaw, "$.size")
	if err != nil {
		return nil, err
	}
	if len(size) != 2 {
		return nil, apperr.Schema(d.version, "$.size", "expected [width,height]")
	}
	w, err := d.integer(size[0], "$.size[0]")
	if err != nil {
		return nil, err
	}
	h, err := d.integer(size[1], "$.size[1]")
	if err != nil {
		return nil, err
	}
	s.Width, s.Height = int(w), int(h)
	s.PixelPitchUM, err = d.numField(raw, "pixelPitchUm", "$.pixelPitchUm")
	if err != nil {
		return nil, err
	}
	bits, err := d.intField(raw, "bitsPerSample", "$.bitsPerSample")
	if err != nil {
		return nil, err
	}
	s.BitDepth = int(bits)
	whiteRaw, err := d.require(raw, "white", "$.white")
	if err != nil {
		return nil, err
	}
	s.WhitePoint, err = d.vec3(whiteRaw, "$.white")
	if err != nil {
		return nil, err
	}
	camera, err := d.objField(raw, "cameraResponse", "$.cameraResponse")
	if err != nil {
		return nil, err
	}
	s.Response.InverseGamma, err = d.numField(camera, "inverseExponent", "$.cameraResponse.inverseExponent")
	if err != nil {
		return nil, err
	}
	s.Response.FullWellRadiance, err = d.numField(camera, "fullWell", "$.cameraResponse.fullWell")
	if err != nil {
		return nil, err
	}
	s.Response.BlackLevel, err = d.numField(camera, "blackLevel", "$.cameraResponse.blackLevel")
	if err != nil {
		return nil, err
	}
	render, err := d.objField(raw, "render", "$.render")
	if err != nil {
		return nil, err
	}
	if v, err := d.numField(render, "interpolationOrder", "$.render.interpolationOrder"); err == nil {
		s.Settings.InterpolationOrder = int(v)
		s.Present["render.interpolationOrder"] = true
	} else {
		return nil, err
	}
	if v, err := d.numField(render, "maxRegistrationOffset", "$.render.maxRegistrationOffset"); err == nil {
		s.Settings.MaxRegistrationOffset = v
		s.Present["render.maxRegistrationOffset"] = true
	} else {
		return nil, err
	}
	if v, err := d.numField(render, "fusionPower", "$.render.fusionPower"); err == nil {
		s.Settings.FusionPower = v
		s.Present["render.fusionPower"] = true
	} else {
		return nil, err
	}
	if v, err := d.numField(render, "fusionStrength", "$.render.fusionStrength"); err == nil {
		s.Settings.FusionStrength = v
		s.Present["render.fusionStrength"] = true
	} else {
		return nil, err
	}
	s.Settings.GamutIntent, err = d.strField(render, "gamutIntent", "$.render.gamutIntent")
	if err != nil {
		return nil, err
	}
	s.Present["render.gamutIntent"] = true
	s.Settings.TargetGamut, err = d.strField(render, "targetGamut", "$.render.targetGamut")
	if err != nil {
		return nil, err
	}
	s.Present["render.targetGamut"] = true
	s.Settings.DetailGain, err = d.numField(render, "detailGain", "$.render.detailGain")
	if err != nil {
		return nil, err
	}
	s.Present["render.detailGain"] = true
	waveRaw, err := d.arrField(raw, "wavebands", "$.wavebands")
	if err != nil {
		return nil, err
	}
	s.Bands = make([]Band, len(waveRaw))
	index := map[string]int{}
	for i, item := range waveRaw {
		bp := fmt.Sprintf("$.wavebands[%d]", i)
		bo, err := d.object(item, bp)
		if err != nil {
			return nil, err
		}
		b := Band{}
		b.ID, err = d.strField(bo, "id", bp+".id")
		if err != nil {
			return nil, err
		}
		wave, err := d.objField(bo, "centerWavelength", bp+".centerWavelength")
		if err != nil {
			return nil, err
		}
		b.CenterNM, err = d.numField(wave, "value", bp+".centerWavelength.value")
		if err != nil {
			return nil, err
		}
		unit, err := d.strField(wave, "unit", bp+".centerWavelength.unit")
		if err != nil {
			return nil, err
		}
		if unit != "nm" {
			return nil, apperr.Schema(d.version, bp+".centerWavelength.unit", "expected nm")
		}
		respRaw, err := d.require(bo, "xyzResponse", bp+".xyzResponse")
		if err != nil {
			return nil, err
		}
		b.ResponseXYZ, err = d.vec3(respRaw, bp+".xyzResponse")
		if err != nil {
			return nil, err
		}
		reg, err := d.objField(bo, "registrationOffset", bp+".registrationOffset")
		if err != nil {
			return nil, err
		}
		b.OffsetX, err = d.numField(reg, "x", bp+".registrationOffset.x")
		if err != nil {
			return nil, err
		}
		b.OffsetY, err = d.numField(reg, "y", bp+".registrationOffset.y")
		if err != nil {
			return nil, err
		}
		detailRaw, err := d.require(bo, "noiseDetail", bp+".noiseDetail")
		if err != nil {
			return nil, err
		}
		b.Detail, err = d.floatArray(detailRaw, bp+".noiseDetail")
		if err != nil {
			return nil, err
		}
		if err := checkLength(s, b.ID, "noiseDetail", len(b.Detail), d.version); err != nil {
			return nil, err
		}
		index[b.ID] = i
		s.Bands[i] = b
	}
	captures, err := d.arrField(raw, "captures", "$.captures")
	if err != nil {
		return nil, err
	}
	for i, item := range captures {
		cp := fmt.Sprintf("$.captures[%d]", i)
		co, err := d.object(item, cp)
		if err != nil {
			return nil, err
		}
		bandID, err := d.strField(co, "band", cp+".band")
		if err != nil {
			return nil, err
		}
		bi, ok := index[bandID]
		if !ok {
			return nil, apperr.Schema(d.version, cp+".band", "unknown band %q", bandID)
		}
		exp, err := d.objField(co, "exposure", cp+".exposure")
		if err != nil {
			return nil, err
		}
		secRaw, err := d.require(exp, "seconds", cp+".exposure.seconds")
		if err != nil {
			return nil, err
		}
		us, err := d.secondsMicros(secRaw, cp+".exposure.seconds")
		if err != nil {
			return nil, err
		}
		codesRaw, err := d.require(co, "quantized", cp+".quantized")
		if err != nil {
			return nil, err
		}
		codes, err := d.codes(codesRaw, cp+".quantized")
		if err != nil {
			return nil, err
		}
		if err := checkLength(s, bandID, "quantized", len(codes), d.version); err != nil {
			return nil, err
		}
		s.Bands[bi].Exposures = append(s.Bands[bi].Exposures, Exposure{DurationUS: us, Codes: codes})
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
	return d.finish(s)
}
