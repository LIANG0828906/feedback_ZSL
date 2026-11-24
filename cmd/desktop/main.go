//go:build gui

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"fyne.io/fyne/v2/app"

	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/generator"
	"performanceexpressions/workspace/ui"
)

func main() {
	fs := flag.NewFlagSet("workspace-desktop", flag.ContinueOnError)
	projectPath := fs.String("project", "samples/aerial-race.json", "project JSON")
	dbPath := fs.String("db", "artifacts/ui-workspace.db", "SQLite checkpoint path")
	interp := fs.String("interpolation", domain.InterpolationLinear, "linear, hold or smoothstep")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	project, err := generator.LoadProject(*projectPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	params := domain.DefaultParameters()
	params.Interpolation = *interp
	a := app.NewWithID("performanceexpressions.workspace")
	workspace, err := ui.NewWorkspace(a, project, params, *dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	workspace.Window().ShowAndRun()
}
