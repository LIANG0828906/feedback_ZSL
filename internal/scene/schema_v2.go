package scene

import (
	"fmt"

	"ms-workbench/internal/apperr"
)

func (d decoder) decodeV2(raw object) (*Scene, error) {
	s := &Scene{Present: map[string]bool{}, Settings: DefaultSettings()}
	var err error
	if _, err := d.strField(raw, "schema", "$.schema"); err != nil {
		return nil, err
	}
	s.SceneID, err = d.strField(raw, "sceneId", "$.sceneId")
	if err != nil {
		return nil, err
	}
	s.Name, err = d.strField(raw, "displayName", "$.displayName")
	if err != nil {
		return nil, err
	}
	dim, err := d.objField(raw, "dimensions", "$.dimensions")
	if err != nil {
		return nil, err
	}
	w, err := d.intField(dim, "width", "$.dimensions.width")
	if err != nil {
		return nil, err
	}
	h, err := d.intField(dim, "height", "$.dimensions.height")
	if err != nil {
		return nil, err
	}
	s.Width, s.Height = int(w), int(h)
	s.PixelPitchUM, err = d.numField(raw, "pixelPitchMicrons", "$.pixelPitchMicrons")
	if err != nil {
		return nil, err
	}
	quant, err := d.objField(raw, "quantization", "$.quantization")
	if err != nil {
		return nil, err
	}
	bits, err := d.intField(quant, "bits", "$.quantization.bits")
	if err != nil {
		return nil, err
	}
	s.BitDepth = int(bits)
	white, err := d.objField(raw, "whitePoint", "$.whitePoint")
	if err != nil {
		return nil, err
	}
	s.WhitePoint[0], err = d.numField(white, "X", "$.whitePoint.X")
	if err != nil {
		return nil, err
	}
	s.WhitePoint[1], err = d.numField(white, "Y", "$.whitePoint.Y")
	if err != nil {
		return nil, err
	}
	s.WhitePoint[2], err = d.numField(white, "Z", "$.whitePoint.Z")
	if err != nil {
		return nil, err
	}
	camera, err := d.objField(raw, "camera", "$.camera")
	if err != nil {
		return nil, err
	}
	s.Response.InverseGamma, err = d.numField(camera, "inverseGamma", "$.camera.inverseGamma")
	if err != nil {
		return nil, err
	}
	s.Response.FullWellRadiance, err = d.numField(camera, "fullWellRadiance", "$.camera.fullWellRadiance")
	if err != nil {
		return nil, err
	}
	s.Response.BlackLevel, err = d.numField(camera, "blackLevel", "$.camera.blackLevel")
	if err != nil {
		return nil, err
	}
	if renderRaw, ok := d.optional(raw, "render"); ok {
		render, err := d.object(renderRaw, "$.render")
		if err != nil {
			return nil, err
		}
		if v, present, err := d.optionalNumField(render, "interpolationOrder", "$.render.interpolationOrder"); err != nil {
			return nil, err
		} else if present {
			s.Settings.InterpolationOrder = int(v)
			s.Present["render.interpolationOrder"] = true
		}
		if v, present, err := d.optionalNumField(render, "maxRegistrationOffset", "$.render.maxRegistrationOffset"); err != nil {
			return nil, err
		} else if present {
			s.Settings.MaxRegistrationOffset = v
			s.Present["render.maxRegistrationOffset"] = true
		}
		if v, present, err := d.optionalNumField(render, "fusionPower", "$.render.fusionPower"); err != nil {
			return nil, err
		} else if present {
			s.Settings.FusionPower = v
			s.Present["render.fusionPower"] = true
		}
		if v, present, err := d.optionalNumField(render, "fusionStrength", "$.render.fusionStrength"); err != nil {
			return nil, err
		} else if present {
			s.Settings.FusionStrength = v
			s.Present["render.fusionStrength"] = true
		}
	}
	bandRaw, err := d.arrField(raw, "bandMeta", "$.bandMeta")
	if err != nil {
		return nil, err
	}
	s.Bands = make([]Band, len(bandRaw))
	index := map[string]int{}
	for i, item := range bandRaw {
		bp := fmt.Sprintf("$.bandMeta[%d]", i)
		bo, err := d.object(item, bp)
		if err != nil {
			return nil, err
		}
		b := Band{}
		b.ID, err = d.strField(bo, "id", bp+".id")
		if err != nil {
			return nil, err
		}
		meta, err := d.objField(bo, "metadata", bp+".metadata")
		if err != nil {
			return nil, err
		}
		wave, err := d.objField(meta, "centerWavelength", bp+".metadata.centerWavelength")
		if err != nil {
			return nil, err
		}
		center, err := d.numField(wave, "value", bp+".metadata.centerWavelength.value")
		if err != nil {
			return nil, err
		}
		unit, err := d.strField(wave, "unit", bp+".metadata.centerWavelength.unit")
		if err != nil {
			return nil, err
		}
		if unit != "nm" {
			return nil, apperr.Schema(d.version, bp+".metadata.centerWavelength.unit", "expected nm")
		}
		b.CenterNM = center
		resp, err := d.objField(meta, "responseXYZ", bp+".metadata.responseXYZ")
		if err != nil {
			return nil, err
		}
		b.ResponseXYZ[0], err = d.numField(resp, "X", bp+".metadata.responseXYZ.X")
		if err != nil {
			return nil, err
		}
		b.ResponseXYZ[1], err = d.numField(resp, "Y", bp+".metadata.responseXYZ.Y")
		if err != nil {
			return nil, err
		}
		b.ResponseXYZ[2], err = d.numField(resp, "Z", bp+".metadata.responseXYZ.Z")
		if err != nil {
			return nil, err
		}
		reg, err := d.objField(meta, "registration", bp+".metadata.registration")
		if err != nil {
			return nil, err
		}
		b.OffsetX, err = d.numField(reg, "dx", bp+".metadata.registration.dx")
		if err != nil {
			return nil, err
		}
		b.OffsetY, err = d.numField(reg, "dy", bp+".metadata.registration.dy")
		if err != nil {
			return nil, err
		}
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
		index[b.ID] = i
		s.Bands[i] = b
	}
	capture, err := d.objField(raw, "capture", "$.capture")
	if err != nil {
		return nil, err
	}
	frames, err := d.arrField(capture, "frames", "$.capture.frames")
	if err != nil {
		return nil, err
	}
	for i, item := range frames {
		fp := fmt.Sprintf("$.capture.frames[%d]", i)
		fo, err := d.object(item, fp)
		if err != nil {
			return nil, err
		}
		bandID, err := d.strField(fo, "bandId", fp+".bandId")
		if err != nil {
			return nil, err
		}
		bi, ok := index[bandID]
		if !ok {
			return nil, apperr.Schema(d.version, fp+".bandId", "unknown band %q", bandID)
		}
		duration, err := d.objField(fo, "duration", fp+".duration")
		if err != nil {
			return nil, err
		}
		valueRaw, err := d.require(duration, "value", fp+".duration.value")
		if err != nil {
			return nil, err
		}
		unit, err := d.strField(duration, "unit", fp+".duration.unit")
		if err != nil {
			return nil, err
		}
		var us int64
		switch unit {
		case "us":
			us, err = d.integer(valueRaw, fp+".duration.value")
		case "s":
			us, err = d.secondsMicros(valueRaw, fp+".duration.value")
		default:
			return nil, apperr.Schema(d.version, fp+".duration.unit", "expected us or s")
		}
		if err != nil {
			return nil, err
		}
		rawCodes, err := d.require(fo, "codes", fp+".codes")
		if err != nil {
			return nil, err
		}
		codes, err := d.codes(rawCodes, fp+".codes")
		if err != nil {
			return nil, err
		}
		if err := checkLength(s, bandID, "codes", len(codes), d.version); err != nil {
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
	for path := range v3SettingsDefaults() {
		if _, ok := s.Present[path]; !ok {
			s.Present[path] = false
		}
	}
	return d.finish(s)
}
