package engine

import "path/filepath"

type Options struct {
	DBPath            string
	Width             int
	Height            int
	TileCols          int
	TileRows          int
	ExactDensity      int
	PreviewDensity    int
	CurvatureScale    float64
	NormalTolerance   float64
	MaxParallel       int
	PerTaskPixelBytes int64
	PerTaskMeshBytes  int64
	TotalPixelBytes   int64
	TotalMeshBytes    int64
	StageFailTile     int
}

func DefaultOptions(baseDir string) Options {
	return Options{
		DBPath:            filepath.Join(baseDir, "workbench.sqlite"),
		Width:             720,
		Height:            440,
		TileCols:          8,
		TileRows:          5,
		ExactDensity:      30,
		PreviewDensity:    6,
		CurvatureScale:    1.0,
		NormalTolerance:   0.75,
		MaxParallel:       2,
		PerTaskPixelBytes: 1 << 20,
		PerTaskMeshBytes:  4 << 20,
		TotalPixelBytes:   4 << 20,
		TotalMeshBytes:    9 << 20,
		StageFailTile:     -1,
	}
}
