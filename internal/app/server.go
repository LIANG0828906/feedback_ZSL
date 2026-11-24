package app

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"storyboardstudio/internal/api"
	"storyboardstudio/internal/workbench"
)

func OpenWorkbench(samplesDir, dbPath string) (*workbench.Workbench, int, error) {
	if dir := filepath.Dir(dbPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, 0, err
		}
	}
	wb, err := workbench.New(dbPath)
	if err != nil {
		return nil, 0, err
	}
	loaded, err := LoadSamples(wb, samplesDir)
	if err != nil {
		_ = wb.Close()
		return nil, 0, err
	}
	return wb, loaded, nil
}

func LoadSamples(wb *workbench.Workbench, samplesDir string) (int, error) {
	entries, err := os.ReadDir(samplesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if _, err := wb.LoadFile(filepath.Join(samplesDir, entry.Name())); err != nil {
			return loaded, fmt.Errorf("load %s: %w", entry.Name(), err)
		}
		loaded++
	}
	return loaded, nil
}

func Serve(addr, samplesDir, dbPath string) error {
	wb, loaded, err := OpenWorkbench(samplesDir, dbPath)
	if err != nil {
		return err
	}
	defer wb.Close()
	server := &http.Server{
		Addr:              addr,
		Handler:           api.New(wb).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("storyboard workbench: http://%s (loaded %d projects, db=%s)\n", addr, loaded, dbPath)
	return server.ListenAndServe()
}
