package ingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"audiobench/internal/model"
	"audiobench/internal/sample"
)

func generateFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := sample.GenerateAll(sample.Options{
		OutDir: dir, Seed: 7, SampleRate: 16000,
		MaterialSecs: 4, EventCount: 4, WindowSize: 512,
	}); err != nil {
		t.Fatalf("生成样本失败: %v", err)
	}
	return dir
}

func TestLoadMaterialOK(t *testing.T) {
	dir := generateFixture(t)
	mat, err := LoadMaterial(filepath.Join(dir, "mat_main.json"))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if mat.SampleRate != 16000 || mat.Channels != 1 {
		t.Fatalf("素材头字段异常: %+v", mat)
	}
	if len(mat.Events) == 0 {
		t.Fatal("期望至少一个声明事件")
	}
}

func TestSchemaAndRangeErrors(t *testing.T) {
	dir := generateFixture(t)
	valid, err := LoadMaterial(filepath.Join(dir, "mat_main.json"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("类别非法返回E_SCHEMA", func(t *testing.T) {
		bad := *valid
		bad.Events = append([]model.EventDecl(nil), valid.Events...)
		bad.Events[0].Label = "not_a_category"
		path := filepath.Join(dir, "bad_schema.json")
		writeDesc(t, path, &bad)
		_, err := LoadMaterial(path)
		assertCode(t, err, model.ESchema)
	})

	t.Run("区间越界返回E_RANGE并带边界", func(t *testing.T) {
		bad := *valid
		bad.Events = append([]model.EventDecl(nil), valid.Events...)
		bad.Events[0].EndSample = valid.TotalSamples + 100
		path := filepath.Join(dir, "bad_range.json")
		writeDesc(t, path, &bad)
		_, err := LoadMaterial(path)
		appErr := assertCode(t, err, model.ERange)
		if appErr.Details["material_bound"] != valid.TotalSamples {
			t.Fatalf("缺少实测边界: %v", appErr.Details)
		}
	})

	t.Run("事件重叠返回E_RANGE", func(t *testing.T) {
		bad := *valid
		bad.Events = append([]model.EventDecl(nil), valid.Events...)
		if len(bad.Events) < 2 {
			t.Skip("事件不足")
		}
		bad.Events[1].StartSample = bad.Events[0].StartSample + 1
		path := filepath.Join(dir, "bad_overlap.json")
		writeDesc(t, path, &bad)
		_, err := LoadMaterial(path)
		assertCode(t, err, model.ERange)
	})

	t.Run("采样率非法返回E_SCHEMA", func(t *testing.T) {
		bad := *valid
		bad.SampleRate = 12345
		path := filepath.Join(dir, "bad_rate.json")
		writeDesc(t, path, &bad)
		_, err := LoadMaterial(path)
		assertCode(t, err, model.ESchema)
	})
}

func writeDesc(t *testing.T, path string, mat *model.Material) {
	t.Helper()
	mat.PCMFile = validPCMPath(mat)
	data, err := json.Marshal(mat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func validPCMPath(mat *model.Material) string {
	if filepath.IsAbs(mat.PCMFile) {
		return filepath.Base(mat.PCMFile)
	}
	return mat.PCMFile
}

func assertCode(t *testing.T, err error, code string) *model.AppError {
	t.Helper()
	appErr, ok := err.(*model.AppError)
	if !ok {
		t.Fatalf("期望 AppError，实际 %T: %v", err, err)
	}
	if appErr.Code != code {
		t.Fatalf("期望 %s，实际 %s (%v)", code, appErr.Code, appErr)
	}
	return appErr
}
