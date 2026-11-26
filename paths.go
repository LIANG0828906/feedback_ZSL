package quantforensics

import (
	"os"
	"path/filepath"
)

type ThreePathResult struct {
	Realtime        *RunReport        `json:"realtime"`
	RecoveryPartial *RunReport        `json:"recoveryPartial"`
	Recovery        *RunReport        `json:"recovery"`
	FullRecompute   *RunReport        `json:"fullRecompute"`
	Consistency     ConsistencyReport `json:"consistency"`
	CheckpointDir   string            `json:"checkpointDir"`
}

func removeSQLiteFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

// RunThreePathVerification executes realtime, interrupted/resumed and full
// recomputation against separate SQLite checkpoint files.
func RunThreePathVerification(project *Project, checkpointDir string) (*ThreePathResult, error) {
	if err := os.MkdirAll(checkpointDir, 0o755); err != nil {
		return nil, err
	}
	result := &ThreePathResult{CheckpointDir: checkpointDir}

	realtimePath := filepath.Join(checkpointDir, "realtime.sqlite")
	removeSQLiteFiles(realtimePath)
	rtStore, err := OpenSQLiteStore(realtimePath)
	if err != nil {
		return nil, err
	}
	rtEngine := NewEngine(project, rtStore, EngineOptions{Mode: "realtime", Workers: 4, PixelBufferPerWorker: 256, MaxPendingBlocks: 2, PanelID: "panel-realtime"})
	rt, err := rtEngine.Run()
	if err != nil {
		_ = rtStore.Close()
		return nil, err
	}
	if err := rtStore.Close(); err != nil {
		return nil, err
	}
	result.Realtime = rt

	recoveryPath := filepath.Join(checkpointDir, "recovery.sqlite")
	removeSQLiteFiles(recoveryPath)
	partialStore, err := OpenSQLiteStore(recoveryPath)
	if err != nil {
		return nil, err
	}
	partialEngine := NewEngine(project, partialStore, EngineOptions{Mode: "recovery-partial", Workers: 2, PixelBufferPerWorker: 256, MaxPendingBlocks: 1, StopAfterCommits: 6, PanelID: "panel-recovery"})
	partial, err := partialEngine.Run()
	if err != nil {
		_ = partialStore.Close()
		return nil, err
	}
	if err := partialStore.Close(); err != nil {
		return nil, err
	}
	result.RecoveryPartial = partial

	resumeStore, err := OpenSQLiteStore(recoveryPath)
	if err != nil {
		return nil, err
	}
	resumeEngine := NewEngine(project, resumeStore, EngineOptions{Mode: "breakpoint-recovery", Workers: 3, PixelBufferPerWorker: 256, MaxPendingBlocks: 2, ReuseCheckpoints: true, PanelID: "panel-recovery"})
	recovery, err := resumeEngine.Run()
	if err != nil {
		_ = resumeStore.Close()
		return nil, err
	}
	if err := resumeStore.Close(); err != nil {
		return nil, err
	}
	result.Recovery = recovery

	fullPath := filepath.Join(checkpointDir, "full-recompute.sqlite")
	removeSQLiteFiles(fullPath)
	fullStore, err := OpenSQLiteStore(fullPath)
	if err != nil {
		return nil, err
	}
	fullEngine := NewEngine(project, fullStore, EngineOptions{Mode: "full-recompute", Workers: 1, PixelBufferPerWorker: 256, MaxPendingBlocks: 2, PanelID: "panel-full"})
	full, err := fullEngine.Run()
	if err != nil {
		_ = fullStore.Close()
		return nil, err
	}
	if err := fullStore.Close(); err != nil {
		return nil, err
	}
	result.FullRecompute = full
	result.Consistency = CompareRunReports(rt, recovery, full)
	return result, nil
}
