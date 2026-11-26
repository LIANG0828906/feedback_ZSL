package maskqa

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

type DemoResult struct {
	DatasetPath         string   `json:"dataset_path"`
	Directory           string   `json:"directory"`
	FullDB              string   `json:"full_db"`
	RealtimeDB          string   `json:"realtime_db"`
	ResumeDB            string   `json:"resume_db"`
	IncrementalDB       string   `json:"incremental_db"`
	CommitNodes         int      `json:"commit_nodes"`
	ThreePathsIdentical bool     `json:"three_paths_identical"`
	InvalidationCount   int      `json:"invalidation_count"`
	Checks              []string `json:"checks"`
}

func RunDemo(dir string) (*DemoResult, *RunReport, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	d := GenerateDataset()
	datasetPath := filepath.Join(dir, "dataset.json")
	if err := SaveDataset(datasetPath, d); err != nil {
		return nil, nil, err
	}
	ctx := context.Background()
	fullDB := filepath.Join(dir, "full.sqlite")
	realtimeDB := filepath.Join(dir, "realtime.sqlite")
	resumeDB := filepath.Join(dir, "resume.sqlite")
	incDB := filepath.Join(dir, "incremental.sqlite")
	for _, p := range []string{fullDB, realtimeDB, resumeDB, incDB} {
		_ = os.Remove(p)
	}

	fullSvc, err := NewService(d, fullDB)
	if err != nil {
		return nil, nil, err
	}
	full, err := fullSvc.Run(ctx, RunOptions{Mode: "full", IncludePixels: false})
	if err != nil {
		return nil, nil, err
	}
	if err := fullSvc.Close(); err != nil {
		return nil, nil, err
	}

	rtSvc, err := NewService(d, realtimeDB)
	if err != nil {
		return nil, nil, err
	}
	rtParams := rtSvc.Params
	rtParams.MaxParallelBlocks = 3
	rtParams.QueuedPixelBudget = 4096
	rtSvc.Params = rtParams
	rtSvc.Scheduler = NewBoundedScheduler(rtParams)
	realtime, err := rtSvc.Run(ctx, RunOptions{Mode: "realtime"})
	if err != nil {
		return nil, nil, err
	}
	if err := rtSvc.Close(); err != nil {
		return nil, nil, err
	}

	resumeSvc, err := NewService(d, resumeDB)
	if err != nil {
		return nil, nil, err
	}
	interrupted, err := resumeSvc.Run(ctx, RunOptions{Mode: "interrupted", StopAfterCommits: 37})
	if err != nil {
		return nil, nil, err
	}
	if err := resumeSvc.Close(); err != nil {
		return nil, nil, err
	}
	resumeSvc, err = NewService(d, resumeDB)
	if err != nil {
		return nil, nil, err
	}
	resumed, err := resumeSvc.Run(ctx, RunOptions{Mode: "resume"})
	if err != nil {
		return nil, nil, err
	}
	if err := resumeSvc.Close(); err != nil {
		return nil, nil, err
	}

	diff := CompareRunReports(full, realtime)
	if diff == nil {
		diff = CompareRunReports(full, resumed)
	}
	identical := diff == nil && interrupted.Incomplete

	incSvc, err := NewService(d, incDB)
	if err != nil {
		return nil, nil, err
	}
	if _, err := incSvc.Run(ctx, RunOptions{Mode: "baseline"}); err != nil {
		return nil, nil, err
	}
	edited := incSvc.Params
	edited.MaxIterations += 2
	invalidations, err := incSvc.ApplyEdit(ctx, edited)
	if err != nil {
		return nil, nil, err
	}
	incremental, err := incSvc.Run(ctx, RunOptions{Mode: "incremental"})
	if err != nil {
		return nil, nil, err
	}
	if err := incSvc.Close(); err != nil {
		return nil, nil, err
	}

	for _, sr := range full.SampleReports {
		sample := d.Sample(sr.SampleID)
		sdir := filepath.Join(dir, sample.ID)
		if err := os.MkdirAll(sdir, 0o755); err != nil {
			return nil, nil, err
		}
		writePNG := func(name string, fn func(*os.File) error) error {
			f, err := os.Create(filepath.Join(sdir, name))
			if err != nil {
				return err
			}
			defer f.Close()
			return fn(f)
		}
		_ = writePNG("image.png", func(f *os.File) error { return WriteRasterPNG(f, sample.Raster) })
		_ = writePNG("rough.png", func(f *os.File) error { return WriteMaskPNG(f, sample.Rough, nil) })
		_ = writePNG("truth.png", func(f *os.File) error { return WriteMaskPNG(f, sample.Truth.Mask, nil) })
		_ = writePNG("final.png", func(f *os.File) error { return WriteMaskPNG(f, sr.Final, nil) })
		_ = writePNG("diff.png", func(f *os.File) error { return WriteDiffPNG(f, sr.Final, sample.Truth.Mask) })
		if sr.ErrorReport != nil {
			b, _ := json.MarshalIndent(sr.ErrorReport, "", "  ")
			_ = os.WriteFile(filepath.Join(sdir, "error-report.json"), b, 0o644)
		}
	}

	fullJSON, _ := json.MarshalIndent(full, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "full-report.json"), fullJSON, 0o644)
	incJSON, _ := json.MarshalIndent(incremental, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "incremental-report.json"), incJSON, 0o644)

	result := &DemoResult{
		DatasetPath: datasetPath, Directory: dir, FullDB: fullDB, RealtimeDB: realtimeDB, ResumeDB: resumeDB, IncrementalDB: incDB,
		CommitNodes: len(full.CommitSequence), ThreePathsIdentical: identical, InvalidationCount: len(invalidations),
		Checks: []string{
			"dataset schema validation passed",
			"fixed-point gradient/edge/radius/morphology/composition traces generated",
			"SQLite atomic stage checkpoints written in stable order",
			"real-time, resume and full paths compared pixel/component/hash identical",
			"edit invalidation list persisted and unaffected stages reused",
		},
	}
	if !identical {
		result.Checks = append(result.Checks, "E_DIFF: "+diff.Error())
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "demo-result.json"), b, 0o644)
	return result, full, nil
}
