package maskqa

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
)

type PathSummary struct {
	Name            string          `json:"name"`
	MaskHash        string          `json:"mask_hash"`
	ReportHash      string          `json:"report_hash"`
	CommitCount     int             `json:"commit_count"`
	Sequence        []LogicalCommit `json:"sequence"`
	Recovery        *RecoveryReport `json:"recovery,omitempty"`
	RecomputedTiles []int           `json:"recomputed_tiles,omitempty"`
	ReusedTiles     []int           `json:"reused_tiles,omitempty"`
}

type ThreePathReport struct {
	OK            bool        `json:"ok"`
	ProjectID     string      `json:"project_id"`
	Realtime      PathSummary `json:"realtime"`
	Recovery      PathSummary `json:"recovery"`
	Full          PathSummary `json:"full"`
	FirstDiffTile int         `json:"first_diff_tile"`
	Differences   []string    `json:"differences,omitempty"`
}

// RunThreePaths executes realtime, crash-recovery and one-shot recomputation against separate SQLite files.
func RunThreePaths(ctx context.Context, p *Project, truth *TruthManifest, cfg SchedulerConfig, dir string) (*ThreePathReport, error) {
	if dir == "" {
		dir = "."
	}
	report := &ThreePathReport{ProjectID: p.ProjectID, OK: true, FirstDiffTile: -1}

	realtimeStore, err := OpenCheckpoint(filepath.Join(dir, "realtime.db"))
	if err != nil {
		return nil, err
	}
	realtimeEngine, err := NewEngine(p, truth, cfg, realtimeStore)
	if err != nil {
		_ = realtimeStore.Close()
		return nil, err
	}
	realtimeResult, err := realtimeEngine.Run(ctx, RunOptions{Label: "realtime"})
	_ = realtimeStore.Close()
	if err != nil {
		return nil, err
	}
	report.Realtime = summarizePath("realtime", realtimeResult)

	recoveryPath := filepath.Join(dir, "recovery.db")
	recoveryStore, err := OpenCheckpoint(recoveryPath)
	if err != nil {
		return nil, err
	}
	recoveryEngine, err := NewEngine(p, truth, cfg, recoveryStore)
	if err != nil {
		_ = recoveryStore.Close()
		return nil, err
	}
	prefix := len(recoveryEngine.Order.OrderedStrokes) / 2
	if prefix < 1 {
		prefix = 1
	}
	if _, err := recoveryEngine.RunPrefix(ctx, prefix); err != nil {
		_ = recoveryStore.Close()
		return nil, err
	}
	if err := recoveryStore.Close(); err != nil {
		return nil, err
	}

	recoveredStore, err := OpenCheckpoint(recoveryPath)
	if err != nil {
		return nil, err
	}
	recoveryEngine2, err := NewEngine(p, truth, cfg, recoveredStore)
	if err != nil {
		_ = recoveredStore.Close()
		return nil, err
	}
	recoveryResult, err := recoveryEngine2.Run(ctx, RunOptions{Recover: true, Label: "recovery"})
	_ = recoveredStore.Close()
	if err != nil {
		return nil, err
	}
	report.Recovery = summarizePath("recovery", recoveryResult)

	fullStore, err := OpenCheckpoint(filepath.Join(dir, "full.db"))
	if err != nil {
		return nil, err
	}
	fullEngine, err := NewEngine(p, truth, cfg, fullStore)
	if err != nil {
		_ = fullStore.Close()
		return nil, err
	}
	fullResult, err := fullEngine.Run(ctx, RunOptions{Label: "full"})
	_ = fullStore.Close()
	if err != nil {
		return nil, err
	}
	report.Full = summarizePath("full", fullResult)

	if !realtimeResult.Mask.Equal(recoveryResult.Mask) {
		tile, _, _ := FirstMaskDifference(realtimeResult.Mask, recoveryResult.Mask, realtimeEngine.Tiles)
		report.OK = false
		report.FirstDiffTile = tile
		report.Differences = append(report.Differences, "realtime mask != recovery mask")
	}
	if !realtimeResult.Mask.Equal(fullResult.Mask) {
		tile, _, _ := FirstMaskDifference(realtimeResult.Mask, fullResult.Mask, realtimeEngine.Tiles)
		report.OK = false
		if report.FirstDiffTile == -1 {
			report.FirstDiffTile = tile
		}
		report.Differences = append(report.Differences, "realtime mask != full mask")
	}
	if report.Realtime.ReportHash != report.Recovery.ReportHash || report.Realtime.ReportHash != report.Full.ReportHash {
		report.OK = false
		report.Differences = append(report.Differences, "verification report hash mismatch")
	}
	if !reflect.DeepEqual(report.Realtime.Sequence, report.Recovery.Sequence) {
		tile, idx, msg := FirstSequenceDiff(report.Realtime.Sequence, report.Recovery.Sequence)
		report.OK = false
		if report.FirstDiffTile == -1 {
			report.FirstDiffTile = tile
		}
		report.Differences = append(report.Differences, fmt.Sprintf("commit sequence recovery mismatch at index %d tile %d: %s", idx, tile, msg))
	}
	if !reflect.DeepEqual(report.Realtime.Sequence, report.Full.Sequence) {
		tile, idx, msg := FirstSequenceDiff(report.Realtime.Sequence, report.Full.Sequence)
		report.OK = false
		if report.FirstDiffTile == -1 {
			report.FirstDiffTile = tile
		}
		report.Differences = append(report.Differences, fmt.Sprintf("commit sequence full mismatch at index %d tile %d: %s", idx, tile, msg))
	}
	if !report.OK {
		return report, &Error{Code: CodeDIFF, FirstTile: maxInt(0, report.FirstDiffTile), Message: "three-path consistency check failed: " + joinStrings(report.Differences, "; ")}
	}
	return report, nil
}

func summarizePath(name string, r *RunResult) PathSummary {
	s := PathSummary{
		Name: name, CommitCount: len(r.CommitSequence),
		Sequence:        LogicalSequence(r.CommitSequence),
		RecomputedTiles: append([]int(nil), r.RecomputedTiles...),
		ReusedTiles:     append([]int(nil), r.ReusedTiles...),
	}
	if r.Mask != nil {
		s.MaskHash = HashBytes(r.Mask.Pix)
	}
	if r.Report != nil {
		s.ReportHash = HashJSON(r.Report)
	}
	if r.Recovery != nil {
		s.Recovery = r.Recovery
	}
	return s
}

// FirstMaskDifference locates the first changed pixel and maps it to a tile.
func FirstMaskDifference(a, b *Mask, tiles []Tile) (tileID int, p Point, message string) {
	if a == nil || b == nil || a.Width != b.Width || a.Height != b.Height {
		return -1, Point{}, "mask dimensions differ"
	}
	for y := 0; y < a.Height; y++ {
		for x := 0; x < a.Width; x++ {
			if a.Get(x, y) != b.Get(x, y) {
				tileID = -1
				if t, ok := TileAt(tiles, x, y); ok {
					tileID = t.ID
				}
				return tileID, Point{X: x, Y: y}, fmt.Sprintf("first pixel difference at (%d,%d): %d != %d", x, y, a.Get(x, y), b.Get(x, y))
			}
		}
	}
	return -1, Point{}, ""
}

func joinStrings(items []string, sep string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
