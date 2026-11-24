package params

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"hdrproof/internal/apperr"
	"hdrproof/internal/model"
)

func Validate(p model.Params, route string, segment int) error {
	c := model.DefaultConstraints
	if !model.Finite(p.TargetPeakNits) {
		return apperr.New(apperr.CodePara, route, segment, -1, "target_peak_nits",
			fmt.Sprintf("%.0f..%.0f", c.TargetPeakMin, c.TargetPeakMax),
			"parameter must be finite", nil)
	}
	if p.TargetPeakNits < c.TargetPeakMin || p.TargetPeakNits > c.TargetPeakMax {
		return apperr.New(apperr.CodePara, route, segment, -1, "target_peak_nits",
			fmt.Sprintf("%.0f..%.0f", c.TargetPeakMin, c.TargetPeakMax),
			fmt.Sprintf("value %v is outside the accepted range", p.TargetPeakNits), nil)
	}
	if !model.Finite(p.ExposureEV) || p.ExposureEV < c.ExposureMin || p.ExposureEV > c.ExposureMax {
		return apperr.New(apperr.CodePara, route, segment, -1, "exposure_ev",
			fmt.Sprintf("%.0f..%.0f", c.ExposureMin, c.ExposureMax),
			fmt.Sprintf("value %v is outside the accepted range", p.ExposureEV), nil)
	}
	if p.ToneMap != "aces" && p.ToneMap != "reinhard" {
		return apperr.New(apperr.CodePara, route, segment, -1, "tone_map",
			"aces|reinhard", fmt.Sprintf("unsupported value %q", p.ToneMap), nil)
	}
	if p.GamutMapping != "desaturate" && p.GamutMapping != "clip" {
		return apperr.New(apperr.CodePara, route, segment, -1, "gamut_mapping",
			"desaturate|clip", fmt.Sprintf("unsupported value %q", p.GamutMapping), nil)
	}
	if p.OutputBits != 8 {
		return apperr.New(apperr.CodePara, route, segment, -1, "output_bits",
			"8", fmt.Sprintf("unsupported value %d", p.OutputBits), nil)
	}
	return nil
}

func WithVersion(p model.Params) model.ParamsVersioned {
	return model.ParamsVersioned{Params: p, Version: model.ParamVersion(p)}
}

// ApplyInjection modifies exactly one externally named parameter field. It is
// intentionally reflection based so injectors do not need custom code paths.
func ApplyInjection(p model.Params, field string, value interface{}) (model.Params, error) {
	v := reflect.ValueOf(&p).Elem()
	f := v.FieldByNameFunc(func(name string) bool {
		return strings.EqualFold(name, field) || snake(name) == strings.ToLower(field)
	})
	if !f.IsValid() || !f.CanSet() {
		return p, apperr.New(apperr.CodePara, "", -1, -1, field, "known parameter field",
			"unknown parameter field", nil)
	}
	if err := assign(f, value); err != nil {
		return p, apperr.New(apperr.CodePara, "", -1, -1, field, f.Type().String(),
			err.Error(), err)
	}
	return p, nil
}

func assign(dst reflect.Value, value interface{}) error {
	if value == nil {
		return fmt.Errorf("injected value is null")
	}
	rv := reflect.ValueOf(value)
	switch dst.Kind() {
	case reflect.Float32, reflect.Float64:
		switch rv.Kind() {
		case reflect.Float32, reflect.Float64:
			dst.SetFloat(rv.Convert(dst.Type()).Float())
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			dst.SetFloat(float64(rv.Int()))
		default:
			return fmt.Errorf("expected numeric value, received %T", value)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		switch rv.Kind() {
		case reflect.Float32, reflect.Float64:
			dst.SetInt(int64(rv.Float()))
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			dst.SetInt(rv.Int())
		default:
			return fmt.Errorf("expected integer value, received %T", value)
		}
	case reflect.String:
		s, ok := value.(string)
		if !ok {
			if n, ok := value.(fmt.Stringer); ok {
				s = n.String()
			} else {
				return fmt.Errorf("expected string value, received %T", value)
			}
		}
		dst.SetString(s)
	default:
		return fmt.Errorf("unsupported parameter type %s", dst.Type())
	}
	return nil
}

func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func NumberString(v interface{}) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(n), 'f', -1, 32)
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	default:
		return fmt.Sprint(v)
	}
}
