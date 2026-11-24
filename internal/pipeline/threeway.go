package pipeline

import (
	"context"
	"os"
	"path/filepath"

	"maskpipe/internal/model"
	"maskpipe/internal/sample"
	"maskpipe/internal/store"
)

type ThreeWayResult struct {
	Report  *ThreeWayReport    `json:"report"`
	Results map[string]*Result `json:"results"`
	DBPaths map[string]string  `json:"db_paths"`
	Partial *Result            `json:"partial_checkpoint,omitempty"`
}

func RunThreeWay(ctx context.Context, loaded *sample.LoadedScene, params model.ParameterSet, dir string) (*ThreeWayResult, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	paths := map[string]string{
		"live":   filepath.Join(dir, "live.db"),
		"resume": filepath.Join(dir, "resume.db"),
		"full":   filepath.Join(dir, "full.db"),
	}
	for _, path := range paths {
		_ = os.Remove(path)
	}

	out := &ThreeWayResult{DBPaths: paths, Results: map[string]*Result{}}

	resumeStore, err := store.Open(paths["resume"])
	if err != nil {
		return nil, err
	}
	partialService, err := NewService(loaded, resumeStore, &params, NewControl())
	if err != nil {
		resumeStore.Close()
		return nil, err
	}
	partialService.ChunkFilter = map[int]bool{0: true}
	partialResult, partialErr := partialService.Run(ctx, ModeIncremental, nil)
	resumeStore.Close()
	if partialResult == nil {
		return nil, partialErr
	}
	out.Partial = partialResult

	resumeStore2, err := store.Open(paths["resume"])
	if err != nil {
		return nil, err
	}
	resumeService, err := NewService(loaded, resumeStore2, &params, NewControl())
	if err != nil {
		resumeStore2.Close()
		return nil, err
	}
	resumeResult, resumeErr := resumeService.Run(ctx, ModeResume, nil)
	resumeStore2.Close()
	if resumeErr != nil && !isQualityOnly(resumeErr) {
		return nil, resumeErr
	}
	out.Results["resume"] = resumeResult

	liveStore, err := store.Open(paths["live"])
	if err != nil {
		return nil, err
	}
	liveService, err := NewService(loaded, liveStore, &params, NewControl())
	if err != nil {
		liveStore.Close()
		return nil, err
	}
	liveResult, liveErr := liveService.Run(ctx, ModeIncremental, nil)
	liveStore.Close()
	if liveErr != nil && !isQualityOnly(liveErr) {
		return nil, liveErr
	}
	out.Results["live"] = liveResult

	fullStore, err := store.Open(paths["full"])
	if err != nil {
		return nil, err
	}
	fullService, err := NewService(loaded, fullStore, &params, NewControl())
	if err != nil {
		fullStore.Close()
		return nil, err
	}
	fullResult, fullErr := fullService.Run(ctx, ModeFull, nil)
	fullStore.Close()
	if fullErr != nil && !isQualityOnly(fullErr) {
		return nil, fullErr
	}
	out.Results["full"] = fullResult

	out.Report = CompareThree("full", fullResult, map[string]*Result{
		"live":   liveResult,
		"resume": resumeResult,
	})
	return out, nil
}

func isQualityOnly(err error) bool {
	pe, ok := model.AsError(err)
	if !ok {
		return false
	}
	return pe.Code == model.CodeRegion || pe.Code == model.CodeQual
}
