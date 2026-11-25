package yuv_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"uvenhance/internal/apperr"
	"uvenhance/internal/samplegen"
	"uvenhance/internal/yuv"
)

func makeSample(t *testing.T, w, h int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sample")
	if _, err := samplegen.Generate(context.Background(), dir,
		samplegen.Config{ID: "test", Width: w, Height: h}, 991); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadAndTruthValidation(t *testing.T) {
	dir := makeSample(t, 96, 64)
	s, err := yuv.LoadSample(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Y.Width != 96 || s.U.Width != 48 || s.V.Height != 32 {
		t.Fatalf("unexpected 4:2:0 geometry")
	}
	if err := yuv.ValidateTruth(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func TestMissingMetadataReturnsEMeta(t *testing.T) {
	_, err := yuv.LoadSample(t.TempDir())
	assertCode(t, err, apperr.CodeMeta)
}

func TestPlaneSizeMismatchReturnsEPlaneWithMeasuredValue(t *testing.T) {
	dir := makeSample(t, 96, 64)
	path := filepath.Join(dir, "U.plane")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, st.Size()-1); err != nil {
		t.Fatal(err)
	}
	_, err = yuv.LoadSample(dir)
	assertCode(t, err, apperr.CodePlane)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *apperr.Error, got %T", err)
	}
	if appErr.Plane != "U" || appErr.Observed == "" {
		t.Fatalf("expected U plane measured value, got %+v", appErr)
	}
}

func TestPlaneBitDepthMismatchReturnsEPlane(t *testing.T) {
	dir := makeSample(t, 96, 64)
	path := filepath.Join(dir, "metadata.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var md map[string]any
	if err := json.Unmarshal(raw, &md); err != nil {
		t.Fatal(err)
	}
	planes := md["planes"].(map[string]any)
	u := planes["U"].(map[string]any)
	u["bit_depth"] = float64(10)
	raw, _ = json.Marshal(md)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = yuv.LoadSample(dir)
	assertCode(t, err, apperr.CodePlane)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Plane != "U" {
		t.Fatalf("expected U plane bit-depth error, got %v", err)
	}
}

func TestOddLumaDimensionReturnsEPlane(t *testing.T) {
	dir := makeSample(t, 96, 64)
	path := filepath.Join(dir, "metadata.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var md map[string]any
	if err := json.Unmarshal(raw, &md); err != nil {
		t.Fatal(err)
	}
	md["width"] = float64(95)
	raw, _ = json.Marshal(md)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = yuv.LoadSample(dir)
	assertCode(t, err, apperr.CodePlane)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Plane != "Y" {
		t.Fatalf("expected Y plane dimension error, got %v", err)
	}
}

func TestMissingPlaneBitDepthReturnsEMeta(t *testing.T) {
	dir := makeSample(t, 96, 64)
	path := filepath.Join(dir, "metadata.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var md map[string]any
	if err := json.Unmarshal(raw, &md); err != nil {
		t.Fatal(err)
	}
	u := md["planes"].(map[string]any)["U"].(map[string]any)
	delete(u, "bit_depth")
	raw, _ = json.Marshal(md)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = yuv.LoadSample(dir)
	assertCode(t, err, apperr.CodeMeta)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Field != "planes.U.bit_depth" {
		t.Fatalf("wrong missing-field error: %v", err)
	}
}

func TestParameterRangeReturnsEParaField(t *testing.T) {
	p := yuv.DefaultParams()
	p.Luma.SharpenAmount = 2.5
	err := p.Validate()
	assertCode(t, err, apperr.CodePara)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Field != "luma.sharpen_amount" {
		t.Fatalf("expected luma.sharpen_amount, got %v", err)
	}
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	if got := apperr.Code(err); got != code {
		t.Fatalf("expected %s, got %s: %v", code, got, err)
	}
}
