package scene

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"ms-workbench/internal/apperr"
)

type object map[string]json.RawMessage

type decoder struct {
	version int
}

func Decode(data []byte) (*Scene, error) {
	raw, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}
	version, err := detectVersion(raw)
	if err != nil {
		return nil, err
	}
	d := decoder{version: version}
	switch version {
	case 1:
		return d.decodeV1(raw)
	case 2:
		return d.decodeV2(raw)
	case 3:
		return d.decodeV3(raw)
	default:
		return nil, apperr.Version(version)
	}
}

func detectVersion(raw object) (int, error) {
	for _, key := range []string{"schema_version", "schemaVersion"} {
		if token, ok := raw[key]; ok {
			var n int
			if err := json.Unmarshal(token, &n); err != nil {
				return 0, apperr.New(apperr.EVER, "schema version must be an integer")
			}
			return n, nil
		}
	}
	if token, ok := raw["schema"]; ok {
		var text string
		if err := json.Unmarshal(token, &text); err != nil {
			return 0, apperr.New(apperr.EVER, "schema identifier must be a string")
		}
		text = strings.TrimSpace(strings.ToLower(text))
		if i := strings.LastIndex(text, "/v"); i >= 0 {
			n, err := strconv.Atoi(text[i+2:])
			if err == nil {
				return n, nil
			}
		}
		if strings.HasPrefix(text, "v") {
			n, err := strconv.Atoi(text[1:])
			if err == nil {
				return n, nil
			}
		}
	}
	return 0, apperr.New(apperr.EVER, "scene schema version is missing")
}

func decodeObject(data []byte, path string) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out object
	if err := dec.Decode(&out); err != nil {
		return nil, apperr.Schema(0, path, "invalid JSON object: %v", err)
	}
	if out == nil {
		return nil, apperr.Schema(0, path, "expected object")
	}
	return out, nil
}

func (d decoder) require(obj object, key, path string) (json.RawMessage, error) {
	v, ok := obj[key]
	if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return nil, apperr.Schema(d.version, path, "required field is missing")
	}
	return v, nil
}

func (d decoder) optional(obj object, key string) (json.RawMessage, bool) {
	v, ok := obj[key]
	if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return nil, false
	}
	return v, true
}

func (d decoder) object(raw json.RawMessage, path string) (object, error) {
	var out object
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil || out == nil {
		return nil, apperr.Schema(d.version, path, "expected object")
	}
	return out, nil
}

func (d decoder) array(raw json.RawMessage, path string) ([]json.RawMessage, error) {
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, apperr.Schema(d.version, path, "expected array")
	}
	return out, nil
}

func (d decoder) string(raw json.RawMessage, path string) (string, error) {
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", apperr.Schema(d.version, path, "expected string")
	}
	return out, nil
}

func (d decoder) number(raw json.RawMessage, path string) (float64, error) {
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return 0, apperr.Schema(d.version, path, "expected number")
	}
	v, err := n.Float64()
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, apperr.Schema(d.version, path, "invalid number")
	}
	return v, nil
}

func (d decoder) integer(raw json.RawMessage, path string) (int64, error) {
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return 0, apperr.Schema(d.version, path, "expected integer")
	}
	v, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return 0, apperr.Schema(d.version, path, "expected integer")
	}
	return v, nil
}

func (d decoder) floatArray(raw json.RawMessage, path string) ([]float64, error) {
	arr, err := d.array(raw, path)
	if err != nil {
		return nil, err
	}
	out := make([]float64, len(arr))
	for i, item := range arr {
		out[i], err = d.number(item, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d decoder) vec3(raw json.RawMessage, path string) (Vec3, error) {
	vals, err := d.floatArray(raw, path)
	if err != nil {
		return Vec3{}, err
	}
	if len(vals) != 3 {
		return Vec3{}, apperr.Schema(d.version, path, "expected three coordinates")
	}
	return Vec3{vals[0], vals[1], vals[2]}, nil
}

func (d decoder) codes(raw json.RawMessage, path string) ([]uint16, error) {
	arr, err := d.array(raw, path)
	if err != nil {
		return nil, err
	}
	out := make([]uint16, len(arr))
	for i, item := range arr {
		v, err := d.integer(item, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		if v < 0 || v > 65535 {
			return nil, apperr.Schema(d.version, fmt.Sprintf("%s[%d]", path, i), "code value out of range")
		}
		out[i] = uint16(v)
	}
	return out, nil
}

func (d decoder) objField(obj object, key, path string) (object, error) {
	raw, err := d.require(obj, key, path)
	if err != nil {
		return nil, err
	}
	return d.object(raw, path)
}

func (d decoder) arrField(obj object, key, path string) ([]json.RawMessage, error) {
	raw, err := d.require(obj, key, path)
	if err != nil {
		return nil, err
	}
	return d.array(raw, path)
}

func (d decoder) strField(obj object, key, path string) (string, error) {
	raw, err := d.require(obj, key, path)
	if err != nil {
		return "", err
	}
	return d.string(raw, path)
}

func (d decoder) numField(obj object, key, path string) (float64, error) {
	raw, err := d.require(obj, key, path)
	if err != nil {
		return 0, err
	}
	return d.number(raw, path)
}

func (d decoder) optionalNumField(obj object, key, path string) (float64, bool, error) {
	raw, ok := d.optional(obj, key)
	if !ok {
		return 0, false, nil
	}
	v, err := d.number(raw, path)
	return v, true, err
}

func (d decoder) intField(obj object, key, path string) (int64, error) {
	raw, err := d.require(obj, key, path)
	if err != nil {
		return 0, err
	}
	return d.integer(raw, path)
}

func (d decoder) timeMicros(obj object, key, path string) (int64, error) {
	raw, err := d.require(obj, key, path)
	if err != nil {
		return 0, err
	}
	return d.integer(raw, path)
}

func (d decoder) secondsMicros(raw json.RawMessage, path string) (int64, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		v, err := ParseSecondsToMicros(text)
		if err != nil {
			return 0, apperr.Schema(d.version, path, "%v", err)
		}
		return v, nil
	}
	// Numeric seconds still pass through an exact decimal token.
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return 0, apperr.Schema(d.version, path, "expected seconds value")
	}
	v, err := ParseSecondsToMicros(n.String())
	if err != nil {
		return 0, apperr.Schema(d.version, path, "%v", err)
	}
	return v, nil
}

func (d decoder) finish(s *Scene) (*Scene, error) {
	if err := s.Validate(); err != nil {
		return nil, apperr.Schema(d.version, "$", "%v", err)
	}
	s.SourceVersion = d.version
	s.SchemaVersion = d.version
	if s.Present == nil {
		s.Present = map[string]bool{}
	}
	return s, nil
}

func checkLength(s *Scene, bandID string, kind string, got int, version int) error {
	want := s.Width * s.Height
	if got != want {
		return apperr.Schema(version, fmt.Sprintf("bands[%s].%s", bandID, kind), "length %d, want %d", got, want)
	}
	return nil
}

func parsePatchArray(d decoder, raw json.RawMessage, path string) ([]Patch, error) {
	arr, err := d.array(raw, path)
	if err != nil {
		return nil, err
	}
	out := make([]Patch, len(arr))
	for i, item := range arr {
		obj, err := d.object(item, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		p := Patch{}
		p.Name, err = d.strField(obj, "name", fmt.Sprintf("%s[%d].name", path, i))
		if err != nil {
			return nil, err
		}
		rectObj, err := d.objField(obj, "rect", fmt.Sprintf("%s[%d].rect", path, i))
		if err != nil {
			return nil, err
		}
		x, err := d.intField(rectObj, "x", fmt.Sprintf("%s[%d].rect.x", path, i))
		if err != nil {
			return nil, err
		}
		y, err := d.intField(rectObj, "y", fmt.Sprintf("%s[%d].rect.y", path, i))
		if err != nil {
			return nil, err
		}
		w, err := d.intField(rectObj, "w", fmt.Sprintf("%s[%d].rect.w", path, i))
		if err != nil {
			return nil, err
		}
		h, err := d.intField(rectObj, "h", fmt.Sprintf("%s[%d].rect.h", path, i))
		if err != nil {
			return nil, err
		}
		p.Rect = Rect{int(x), int(y), int(w), int(h)}
		rawBR, err := d.require(obj, "bandRadiance", fmt.Sprintf("%s[%d].bandRadiance", path, i))
		if err != nil {
			return nil, err
		}
		p.BandRadiance, err = d.floatArray(rawBR, fmt.Sprintf("%s[%d].bandRadiance", path, i))
		if err != nil {
			return nil, err
		}
		rawRGB, err := d.require(obj, "expectedLinearRGB", fmt.Sprintf("%s[%d].expectedLinearRGB", path, i))
		if err != nil {
			return nil, err
		}
		p.ExpectedLinearRGB, err = d.vec3(rawRGB, fmt.Sprintf("%s[%d].expectedLinearRGB", path, i))
		if err != nil {
			return nil, err
		}
		if intentRaw, ok := d.optional(obj, "expectedByIntent"); ok {
			intentObj, err := d.object(intentRaw, fmt.Sprintf("%s[%d].expectedByIntent", path, i))
			if err != nil {
				return nil, err
			}
			p.ExpectedByIntent = map[string]Vec3{}
			for intent, valueRaw := range intentObj {
				v, err := d.vec3(valueRaw, fmt.Sprintf("%s[%d].expectedByIntent.%s", path, i, intent))
				if err != nil {
					return nil, err
				}
				p.ExpectedByIntent[intent] = v
			}
		}
		rawRaw, err := d.require(obj, "rawLinearRGB", fmt.Sprintf("%s[%d].rawLinearRGB", path, i))
		if err != nil {
			return nil, err
		}
		p.RawLinearRGB, err = d.vec3(rawRaw, fmt.Sprintf("%s[%d].rawLinearRGB", path, i))
		if err != nil {
			return nil, err
		}
		rawOOG, err := d.require(obj, "outOfGamut", fmt.Sprintf("%s[%d].outOfGamut", path, i))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(rawOOG, &p.OutOfGamut); err != nil {
			return nil, apperr.Schema(d.version, fmt.Sprintf("%s[%d].outOfGamut", path, i), "expected boolean")
		}
		out[i] = p
	}
	return out, nil
}

func parseTruth(d decoder, raw json.RawMessage, path string) (Truth, error) {
	var out Truth
	obj, err := d.object(raw, path)
	if err != nil {
		return out, err
	}
	out.PatchTolerance, err = d.numField(obj, "patchTolerance", path+".patchTolerance")
	if err != nil {
		return out, err
	}
	out.PeakRadiance, err = d.numField(obj, "peakRadiance", path+".peakRadiance")
	if err != nil {
		return out, err
	}
	regArr, err := d.arrField(obj, "registration", path+".registration")
	if err != nil {
		return out, err
	}
	out.Registration = make([]RegistrationTruth, len(regArr))
	for i, item := range regArr {
		ip := fmt.Sprintf("%s.registration[%d]", path, i)
		ro, err := d.object(item, ip)
		if err != nil {
			return out, err
		}
		out.Registration[i].Reference, err = d.strField(ro, "reference", ip+".reference")
		if err != nil {
			return out, err
		}
		out.Registration[i].Band, err = d.strField(ro, "band", ip+".band")
		if err != nil {
			return out, err
		}
		out.Registration[i].OffsetX, err = d.numField(ro, "offsetX", ip+".offsetX")
		if err != nil {
			return out, err
		}
		out.Registration[i].OffsetY, err = d.numField(ro, "offsetY", ip+".offsetY")
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
