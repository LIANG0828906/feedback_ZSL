package scene

import (
	"encoding/json"
	"fmt"
	"reflect"

	"ms-workbench/internal/apperr"
)

func v2SettingsDefaults() map[string]any {
	d := DefaultSettings()
	return map[string]any{
		"render.interpolationOrder":    d.InterpolationOrder,
		"render.maxRegistrationOffset": d.MaxRegistrationOffset,
		"render.fusionPower":           d.FusionPower,
		"render.fusionStrength":        d.FusionStrength,
	}
}

func v3SettingsDefaults() map[string]any {
	d := DefaultSettings()
	return map[string]any{
		"render.gamutIntent": d.GamutIntent,
		"render.targetGamut": d.TargetGamut,
		"render.detailGain":  d.DetailGain,
	}
}

func settingsValue(s RenderSettings, path string) any {
	switch path {
	case "render.interpolationOrder":
		return s.InterpolationOrder
	case "render.maxRegistrationOffset":
		return s.MaxRegistrationOffset
	case "render.fusionPower":
		return s.FusionPower
	case "render.fusionStrength":
		return s.FusionStrength
	case "render.gamutIntent":
		return s.GamutIntent
	case "render.targetGamut":
		return s.TargetGamut
	case "render.detailGain":
		return s.DetailGain
	default:
		return nil
	}
}

func setSettingsValue(s *RenderSettings, path string, value any) {
	switch path {
	case "render.interpolationOrder":
		s.InterpolationOrder = int(value.(int))
	case "render.maxRegistrationOffset":
		s.MaxRegistrationOffset = value.(float64)
	case "render.fusionPower":
		s.FusionPower = value.(float64)
	case "render.fusionStrength":
		s.FusionStrength = value.(float64)
	case "render.gamutIntent":
		s.GamutIntent = value.(string)
	case "render.targetGamut":
		s.TargetGamut = value.(string)
	case "render.detailGain":
		s.DetailGain = value.(float64)
	}
}

func targetDefaults(target int) map[string]any {
	out := map[string]any{}
	if target >= 2 {
		for k, v := range v2SettingsDefaults() {
			out[k] = v
		}
	}
	if target >= 3 {
		for k, v := range v3SettingsDefaults() {
			out[k] = v
		}
	}
	return out
}

// Upgrade explicitly fills fields introduced by newer schema versions and
// records every default that was applied.
func Upgrade(s *Scene, target int) (*Scene, error) {
	if target < 1 || target > 3 {
		return nil, apperr.Version(target)
	}
	if target < s.SchemaVersion {
		return nil, apperr.New(apperr.EVER, "cannot upgrade schema v%d to v%d", s.SchemaVersion, target)
	}
	out := s.Clone()
	normalizePresence(out)
	for path, def := range targetDefaults(target) {
		if !out.Present[path] {
			old := settingsValue(out.Settings, path)
			setSettingsValue(&out.Settings, path, def)
			out.Present[path] = true
			out.Trace = append(out.Trace, ConversionTrace{
				Action:        "upgrade",
				FromVersion:   out.SchemaVersion,
				ToVersion:     target,
				Path:          path,
				OldValue:      old,
				DefaultValue:  def,
				DefaultSource: fmt.Sprintf("schema-v%d-defaults", target),
			})
		}
	}
	out.SchemaVersion = target
	if err := out.Settings.Validate(); err != nil {
		return nil, apperr.New(apperr.ESCHEMA, "%v", err)
	}
	return out, nil
}

// Downgrade removes fields that the target schema cannot express. Values that
// differ from target defaults are marked as semantic losses.
func Downgrade(s *Scene, target int) (*Scene, []apperr.Loss, error) {
	if target < 1 || target > 3 {
		return nil, nil, apperr.Version(target)
	}
	if target > s.SchemaVersion {
		return nil, nil, apperr.New(apperr.EVER, "cannot downgrade schema v%d to v%d", s.SchemaVersion, target)
	}
	out := s.Clone()
	normalizePresence(out)
	supported := targetDefaults(target)
	var losses []apperr.Loss
	for _, path := range sortedPresence(out.Present) {
		if _, ok := supported[path]; ok {
			continue
		}
		old := settingsValue(out.Settings, path)
		def := settingsValue(RenderSettings(DefaultSettings()), path)
		semantic := !reflect.DeepEqual(old, def)
		losses = append(losses, apperr.Loss{
			Path:        path,
			Value:       old,
			Target:      target,
			Disposition: "not-expressible; coerced to target-schema default",
			Semantic:    semantic,
		})
		setSettingsValue(&out.Settings, path, def)
		out.Present[path] = false
	}
	out.SchemaVersion = target
	if err := out.Settings.Validate(); err != nil {
		return nil, losses, apperr.New(apperr.ESCHEMA, "%v", err)
	}
	return out, losses, nil
}

// Export encodes the scene in the requested schema version. When fields cannot
// be represented, the bytes are still returned and err is an E_LOSSY error.
// Callers that intentionally accept loss can use ExportAllowLossy.
func Export(s *Scene, target int) ([]byte, []apperr.Loss, error) {
	converted, losses, err := prepareExport(s, target)
	if err != nil {
		return nil, losses, err
	}
	data, err := Encode(converted, target)
	if err != nil {
		return nil, losses, err
	}
	if len(losses) > 0 {
		return data, losses, &apperr.Error{
			Code:    apperr.ELOSSY,
			Message: fmt.Sprintf("target schema v%d cannot express %d field(s)", target, len(losses)),
			Losses:  losses,
		}
	}
	return data, nil, nil
}

func ExportAllowLossy(s *Scene, target int) ([]byte, []apperr.Loss, error) {
	converted, losses, err := prepareExport(s, target)
	if err != nil {
		return nil, losses, err
	}
	data, err := Encode(converted, target)
	return data, losses, err
}

func prepareExport(s *Scene, target int) (*Scene, []apperr.Loss, error) {
	switch {
	case target == s.SchemaVersion:
		return s.Clone(), nil, nil
	case target > s.SchemaVersion:
		out, err := Upgrade(s, target)
		return out, nil, err
	case target < s.SchemaVersion:
		return Downgrade(s, target)
	default:
		return nil, nil, apperr.Version(target)
	}
}

func normalizePresence(s *Scene) {
	if s.Present != nil {
		return
	}
	s.Present = map[string]bool{}
	for path := range targetDefaults(s.SchemaVersion) {
		s.Present[path] = true
	}
}

func Encode(s *Scene, target int) ([]byte, error) {
	if target < 1 || target > 3 {
		return nil, apperr.Version(target)
	}
	if err := s.Validate(); err != nil {
		return nil, apperr.Schema(s.SourceVersion, "$", "%v", err)
	}
	var payload map[string]any
	switch target {
	case 1:
		payload = encodeV1(s)
	case 2:
		payload = encodeV2(s)
	case 3:
		payload = encodeV3(s)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, apperr.New(apperr.EIO, "encode schema v%d: %v", target, err)
	}
	return data, nil
}

func sortedPresence(present map[string]bool) []string {
	keys := make([]string, 0, len(present))
	for path, ok := range present {
		if ok {
			keys = append(keys, path)
		}
	}
	sortStrings(keys)
	return keys
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func patchPayload(patches []Patch) []any {
	out := make([]any, len(patches))
	for i, p := range patches {
		byIntent := map[string]any{}
		for intent, value := range p.ExpectedByIntent {
			byIntent[intent] = value[:]
		}
		out[i] = map[string]any{
			"name":              p.Name,
			"rect":              map[string]any{"x": p.Rect.X, "y": p.Rect.Y, "w": p.Rect.W, "h": p.Rect.H},
			"bandRadiance":      p.BandRadiance,
			"expectedLinearRGB": p.ExpectedLinearRGB[:],
			"expectedByIntent":  byIntent,
			"rawLinearRGB":      p.RawLinearRGB[:],
			"outOfGamut":        p.OutOfGamut,
		}
	}
	return out
}

func truthPayload(t Truth) map[string]any {
	reg := make([]any, len(t.Registration))
	for i, r := range t.Registration {
		reg[i] = map[string]any{
			"reference": r.Reference,
			"band":      r.Band,
			"offsetX":   r.OffsetX,
			"offsetY":   r.OffsetY,
		}
	}
	return map[string]any{
		"patchTolerance": t.PatchTolerance,
		"peakRadiance":   t.PeakRadiance,
		"registration":   reg,
	}
}

func encodeV1(s *Scene) map[string]any {
	bands := make([]any, len(s.Bands))
	for i, b := range s.Bands {
		exposures := make([]any, len(b.Exposures))
		for j, e := range b.Exposures {
			exposures[j] = map[string]any{
				"duration_us": e.DurationUS,
				"codes":       e.Codes,
			}
		}
		bands[i] = map[string]any{
			"id":           b.ID,
			"center_nm":    b.CenterNM,
			"response_xyz": b.ResponseXYZ[:],
			"offset_px":    []float64{b.OffsetX, b.OffsetY},
			"detail":       b.Detail,
			"exposures":    exposures,
		}
	}
	return map[string]any{
		"schema_version": 1,
		"scene_id":       s.SceneID,
		"name":           s.Name,
		"pixel_size":     map[string]any{"w": s.Width, "h": s.Height},
		"pixel_pitch_um": s.PixelPitchUM,
		"bit_depth":      s.BitDepth,
		"white_point":    map[string]any{"x": s.WhitePoint[0], "y": s.WhitePoint[1], "z": s.WhitePoint[2]},
		"camera_response": map[string]any{
			"inverse_gamma":      s.Response.InverseGamma,
			"full_well_radiance": s.Response.FullWellRadiance,
			"black_level":        s.Response.BlackLevel,
		},
		"bands":   bands,
		"patches": patchPayload(s.Patches),
		"truth":   truthPayload(s.Truth),
	}
}

func encodeV2(s *Scene) map[string]any {
	bands := make([]any, len(s.Bands))
	for i, b := range s.Bands {
		bands[i] = map[string]any{
			"id": b.ID,
			"metadata": map[string]any{
				"centerWavelength": map[string]any{"value": b.CenterNM, "unit": "nm"},
				"responseXYZ":      map[string]any{"X": b.ResponseXYZ[0], "Y": b.ResponseXYZ[1], "Z": b.ResponseXYZ[2]},
				"registration":     map[string]any{"dx": b.OffsetX, "dy": b.OffsetY},
			},
			"detail": b.Detail,
		}
	}
	var frames []any
	for _, b := range s.Bands {
		for _, e := range b.Exposures {
			frames = append(frames, map[string]any{
				"bandId": b.ID,
				"duration": map[string]any{
					"value": FormatMicrosAsSeconds(e.DurationUS),
					"unit":  "s",
				},
				"codes": e.Codes,
			})
		}
	}
	return map[string]any{
		"schema":            "scene/v2",
		"sceneId":           s.SceneID,
		"displayName":       s.Name,
		"dimensions":        map[string]any{"width": s.Width, "height": s.Height},
		"pixelPitchMicrons": s.PixelPitchUM,
		"quantization":      map[string]any{"bits": s.BitDepth},
		"whitePoint":        map[string]any{"X": s.WhitePoint[0], "Y": s.WhitePoint[1], "Z": s.WhitePoint[2]},
		"camera": map[string]any{
			"inverseGamma":     s.Response.InverseGamma,
			"fullWellRadiance": s.Response.FullWellRadiance,
			"blackLevel":       s.Response.BlackLevel,
		},
		"render": map[string]any{
			"interpolationOrder":    s.Settings.InterpolationOrder,
			"maxRegistrationOffset": s.Settings.MaxRegistrationOffset,
			"fusionPower":           s.Settings.FusionPower,
			"fusionStrength":        s.Settings.FusionStrength,
		},
		"bandMeta": bands,
		"capture":  map[string]any{"frames": frames},
		"patches":  patchPayload(s.Patches),
		"truth":    truthPayload(s.Truth),
	}
}

func encodeV3(s *Scene) map[string]any {
	bands := make([]any, len(s.Bands))
	for i, b := range s.Bands {
		bands[i] = map[string]any{
			"id":                 b.ID,
			"centerWavelength":   map[string]any{"value": b.CenterNM, "unit": "nm"},
			"xyzResponse":        b.ResponseXYZ[:],
			"registrationOffset": map[string]any{"x": b.OffsetX, "y": b.OffsetY},
			"noiseDetail":        b.Detail,
		}
	}
	var captures []any
	for _, b := range s.Bands {
		for _, e := range b.Exposures {
			captures = append(captures, map[string]any{
				"band": b.ID,
				"exposure": map[string]any{
					"seconds": FormatMicrosAsSeconds(e.DurationUS),
				},
				"quantized": e.Codes,
			})
		}
	}
	return map[string]any{
		"schemaVersion": 3,
		"scene":         map[string]any{"id": s.SceneID, "name": s.Name},
		"size":          []int{s.Width, s.Height},
		"pixelPitchUm":  s.PixelPitchUM,
		"bitsPerSample": s.BitDepth,
		"white":         s.WhitePoint[:],
		"cameraResponse": map[string]any{
			"inverseExponent": s.Response.InverseGamma,
			"fullWell":        s.Response.FullWellRadiance,
			"blackLevel":      s.Response.BlackLevel,
		},
		"render": map[string]any{
			"interpolationOrder":    s.Settings.InterpolationOrder,
			"maxRegistrationOffset": s.Settings.MaxRegistrationOffset,
			"fusionPower":           s.Settings.FusionPower,
			"fusionStrength":        s.Settings.FusionStrength,
			"gamutIntent":           s.Settings.GamutIntent,
			"targetGamut":           s.Settings.TargetGamut,
			"detailGain":            s.Settings.DetailGain,
		},
		"wavebands": bands,
		"captures":  captures,
		"patches":   patchPayload(s.Patches),
		"truth":     truthPayload(s.Truth),
	}
}
