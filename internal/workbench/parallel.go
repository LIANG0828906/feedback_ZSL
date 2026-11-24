package workbench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"storyboardstudio/internal/model"
	"storyboardstudio/internal/pool"
)

type ParallelVerification struct {
	Reports      []*Verification `json:"reports"`
	PeakParallel int32           `json:"peak_parallel"`
	MaxParallel  int             `json:"max_parallel"`
}

func VerifyProjectsParallel(ctx context.Context, projects []*model.Project, rootDir string, maxParallel, bufferCapacity int) (*ParallelVerification, error) {
	if maxParallel <= 0 {
		maxParallel = 2
	}
	if bufferCapacity <= 0 {
		bufferCapacity = 4
	}
	executor := pool.New(maxParallel, bufferCapacity, func(record pool.StateRecord) error {
		return nil
	})
	var mu sync.Mutex
	reports := []*Verification{}
	tasks := map[string]pool.Task{}
	for _, project := range projects {
		project := project
		dir := filepath.Join(rootDir, project.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		tasks[project.ID] = func(ctx context.Context, buffer *pool.Buffer) error {
			if err := buffer.Push(pool.StateRecord{SessionID: project.ID, Payload: "started"}); err != nil {
				return err
			}
			report, err := VerifyProject(project, dir)
			if err != nil {
				return err
			}
			if err := buffer.Push(pool.StateRecord{SessionID: project.ID, ShotIndex: report.ShotCount, Payload: "verified"}); err != nil {
				return err
			}
			mu.Lock()
			reports = append(reports, report)
			mu.Unlock()
			return nil
		}
	}
	errs := executor.RunAll(ctx, tasks)
	combined := []error{}
	for _, err := range errs {
		if err != nil {
			combined = append(combined, err)
		}
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].ProjectID < reports[j].ProjectID })
	return &ParallelVerification{Reports: reports, PeakParallel: executor.Peak(), MaxParallel: maxParallel}, errors.Join(combined...)
}
