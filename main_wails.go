//go:build wails_desktop

package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"storyboardstudio/internal/api"
	"storyboardstudio/internal/app"
	"storyboardstudio/internal/workbench"
	"storyboardstudio/web"
)

type Desktop struct {
	workbench *workbench.Workbench
}

func (d *Desktop) ListProjects() []map[string]any {
	projects := d.workbench.ListProjects()
	result := make([]map[string]any, 0, len(projects))
	for _, project := range projects {
		result = append(result, map[string]any{"id": project.ID, "seed": project.Seed})
	}
	return result
}

func (d *Desktop) VerifyProject(projectID string) (*workbench.Verification, error) {
	project, err := d.workbench.Get(projectID)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "wails-storyboard-verify-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	return workbench.VerifyProject(project, dir)
}

func main() {
	dbPath := filepath.Join("data", "wails-storyboard.db")
	wb, _, err := app.OpenWorkbench("samples", dbPath)
	if err != nil {
		panic(err)
	}
	defer wb.Close()

	desktop := &Desktop{workbench: wb}
	handler := api.New(wb).Handler()
	err = wails.Run(&options.App{
		Title:     "交互式叙事与分镜工作台",
		Width:     1480,
		Height:    940,
		MinWidth:  1120,
		MinHeight: 720,
		AssetServer: &assetserver.Options{
			Assets:  web.Files,
			Handler: handler,
		},
		Bind: []any{desktop},
		OnShutdown: func(ctx context.Context) {
			_ = wb.Close()
		},
	})
	if err != nil {
		panic(err)
	}
}
