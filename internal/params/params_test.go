package params

import (
	"testing"

	"hdrproof/internal/apperr"
	"hdrproof/internal/model"
)

func TestValidateFieldErrors(t *testing.T) {
	p := model.DefaultParams()
	p.TargetPeakNits = 999
	err := Validate(p, "route-a", 2)
	if err == nil || apperr.CodeOf(err) != apperr.CodePara {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	info, _ := apperr.As(err)
	if info.Field != "target_peak_nits" || info.RouteID != "route-a" || info.Segment != 2 {
		t.Fatalf("missing context: %+v", info)
	}
	p = model.DefaultParams()
	p.ExposureEV = 9
	if err := Validate(p, "r", 0); err == nil {
		t.Fatal("expected exposure error")
	}
	p = model.DefaultParams()
	p.ToneMap = "bogus"
	if err := Validate(p, "r", 0); err == nil {
		t.Fatal("expected tone map error")
	}
}

func TestApplyInjection(t *testing.T) {
	p, err := ApplyInjection(model.DefaultParams(), "target_peak_nits", float64(999))
	if err != nil {
		t.Fatal(err)
	}
	if p.TargetPeakNits != 999 {
		t.Fatalf("value not injected: %+v", p)
	}
	if _, err := ApplyInjection(p, "unknown_field", 1); err == nil || apperr.CodeOf(err) != apperr.CodePara {
		t.Fatalf("expected E_PARA, got %v", err)
	}
}
