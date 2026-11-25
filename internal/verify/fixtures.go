package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"terrainpipeline/internal/budget"
	"terrainpipeline/internal/checkpoint"
	"terrainpipeline/internal/compare"
	"terrainpipeline/internal/model"
	"terrainpipeline/internal/pipeline"
	"terrainpipeline/internal/projectio"
	"terrainpipeline/internal/seams"
)

func runExceptionMatrix(rep *Report, work, sampleDir string, fullOut *pipeline.Outcome) {
	root := filepath.Join(work, "exceptions")

	schemaDir := filepath.Join(root, "schema")
	mustCopyDir(sampleDir, schemaDir)
	mutateProjectJSON(schemaDir, func(m map[string]any) { delete(m, "schema_version") })
	_, err := projectio.LoadProject(schemaDir)
	expectModelError(rep, "E_SCHEMA-missing-field", err, model.CodeSchema, "", "schema_version")

	sizeDir := filepath.Join(root, "size")
	mustCopyDir(sampleDir, sizeDir)
	removeElevationSample(filepath.Join(sizeDir, "elevations", "B07.json"))
	_, err = projectio.LoadProject(sizeDir)
	expectModelError(rep, "E_SIZE-elevation-mismatch", err, model.CodeSize, "B07", "elevation")

	refDir := filepath.Join(root, "ref")
	mustCopyDir(sampleDir, refDir)
	mutateProjectJSON(refDir, func(m map[string]any) {
		seamsList := m["seams"].([]any)
		for _, item := range seamsList {
			sm := item.(map[string]any)
			if sm["id"] == "S01" {
				sm["block_b"] = "B999"
			}
		}
	})
	_, err = projectio.LoadProject(refDir)
	expectModelError(rep, "E_REF-missing-block", err, model.CodeRef, "B999", "block_b")

	weightDir := filepath.Join(root, "weight")
	mustCopyDir(sampleDir, weightDir)
	setIllegalWeight(filepath.Join(weightDir, "weights", "B07.json"))
	_, err = projectio.LoadProject(weightDir)
	expectModelError(rep, "E_SCHEMA-weight-range", err, model.CodeSchema, "B07", "weights")

	_, seamErr := seams.Classify(seams.Signals{
		ThresholdDeg: 6, RawAngleDeg: 9.2, GeometryAngleDeg: 6.1,
		CorrectedAngleDeg: 5.7, AlternativeAngleDeg: 7.4,
	})
	expectModelError(rep, "E_SEAM-ambiguous-attribution", seamErr, model.CodeSeam, "", "seam.attribution")

	scheduler := budget.NewScheduler(budget.Options{MaxWorkers: 1, PixelBufferPixels: 4, NormalMapMemoryBytes: 16})
	budgetErrs := scheduler.Run(context.Background(), []budget.Task{{
		ID: "BIG", PixelCount: 1024, NormalBytes: 4096,
		Run: func(scratch []model.PixelBake) error { return nil },
	}})
	expectModelError(rep, "E_BUDGET-oversized-block", budgetErrs["BIG"], model.CodeBudget, "BIG", "normal_map_memory_bytes")

	if fullOut != nil {
		wrong := *fullOut.Results["B01"]
		wrong.Pixels = append([]model.PixelBake(nil), fullOut.Results["B01"].Pixels...)
		wrong.Pixels[0].Normal[0] += 0.03125
		wrong.NormalDigest = "mutated-digest"
		wrongResults := map[string]*model.BlockResult{}
		for id, result := range fullOut.Results {
			wrongResults[id] = result
		}
		wrongResults["B01"] = &wrong
		diffErr := compare.BakeOutputs(wrongResults, fullOut.Results, fullOut.Seams, fullOut.Seams)
		expectModelError(rep, "E_DIFF-first-mismatch", diffErr, model.CodeDiff, "B01", "normal_map")
	}

	stalePath := filepath.Join(root, "stale.db")
	store, err := checkpoint.Open(stalePath)
	if err != nil {
		rep.ExceptionMatrix = append(rep.ExceptionMatrix, Check{Name: "checkpoint-version-invalidation", Passed: false, Detail: err.Error()})
	} else {
		_, err = store.CheckCompatibility("hash-a", "pv-a")
		if err != nil {
			rep.ExceptionMatrix = append(rep.ExceptionMatrix, Check{Name: "checkpoint-version-invalidation", Passed: false, Detail: err.Error()})
		}
		stale, err := store.CheckCompatibility("hash-b", "pv-a")
		_ = store.Close()
		passed := err == nil && !stale.Compatible && stale.Reason == "content-hash-mismatch"
		rep.ExceptionMatrix = append(rep.ExceptionMatrix, Check{
			Name: "checkpoint-content-hash-invalidates", Passed: passed,
			Expected: "content-hash-mismatch", Actual: stale.Reason,
			Detail: "重启时仅依据内容哈希与参数版本判定旧快照作废",
		})
		paramStore, perr := checkpoint.Open(filepath.Join(root, "stale-param.db"))
		if perr == nil {
			_, _ = paramStore.CheckCompatibility("hash-a", "pv-a")
			paramStale, _ := paramStore.CheckCompatibility("hash-a", "pv-b")
			_ = paramStore.Close()
			rep.ExceptionMatrix = append(rep.ExceptionMatrix, Check{
				Name: "checkpoint-param-version-invalidates", Passed: paramStale.Reason == "param-version-mismatch",
				Expected: "param-version-mismatch", Actual: paramStale.Reason,
			})
		}
	}
}

func expectModelError(rep *Report, name string, err error, code model.ErrorCode, blockID, fieldContains string) {
	if err == nil {
		rep.ExceptionMatrix = append(rep.ExceptionMatrix, Check{Name: name, Passed: false, Code: string(code), Detail: "期望错误但得到 nil"})
		return
	}
	var te *model.Error
	if !errors.As(err, &te) {
		rep.ExceptionMatrix = append(rep.ExceptionMatrix, Check{Name: name, Passed: false, Code: string(code), Detail: fmt.Sprintf("错误不是 *model.Error: %T %v", err, err)})
		return
	}
	passed := te.Code == code
	if blockID != "" {
		passed = passed && te.BlockID == blockID
	}
	if fieldContains != "" {
		passed = passed && contains(te.Field, fieldContains)
	}
	rep.ExceptionMatrix = append(rep.ExceptionMatrix, Check{
		Name: name, Passed: passed, Code: string(te.Code),
		Expected: map[string]string{"code": string(code), "block_id": blockID, "field_contains": fieldContains},
		Actual:   map[string]string{"code": string(te.Code), "block_id": te.BlockID, "field": te.Field, "message": te.Message},
		Detail:   te.Error(),
	})
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func mustCopyDir(src, dst string) {
	if err := copyDir(src, dst); err != nil {
		panic(err)
	}
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func mutateProjectJSON(dir string, mutate func(map[string]any)) {
	path := filepath.Join(dir, "project.json")
	m := readJSONMap(path)
	mutate(m)
	writeJSONMap(path, m)
}

func readJSONMap(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		panic(err)
	}
	return m
}

func writeJSONMap(path string, m map[string]any) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		panic(err)
	}
}

func removeElevationSample(path string) {
	m := readJSONMap(path)
	values := m["values"].([]any)
	m["values"] = values[:len(values)-1]
	writeJSONMap(path, m)
}

func setIllegalWeight(path string) {
	m := readJSONMap(path)
	values := m["values"].([]any)
	first := values[0].([]any)
	first[1] = float64(256)
	values[0] = first
	m["values"] = values
	writeJSONMap(path, m)
}
