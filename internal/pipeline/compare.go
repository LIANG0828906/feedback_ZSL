package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/store"
	"ocrworkbench/internal/synth"
)

type pathRun struct {
	name   string
	result *Result
	err    *domain.AppError
	took   time.Duration
}

func Compare(ctx context.Context, sampleDir string, seed int64, sampleID string, params domain.Params) (domain.ComparisonResult, *domain.AppError) {
	if appErr := params.Validate(); appErr != nil {
		return domain.ComparisonResult{}, appErr
	}
	if _, appErr := synth.Ensure(sampleDir, seed); appErr != nil {
		return domain.ComparisonResult{}, appErr
	}
	root, err := tempDir()
	if err != nil {
		return domain.ComparisonResult{}, domain.Errorf(domain.CodeStore, "cannot create comparison directory: %v", err)
	}
	defer os.RemoveAll(root)
	runs := make([]pathRun, 0, 3)

	live, appErr := runSingle(ctx, filepath.Join(root, "live.db"), sampleDir, seed, sampleID, params, "live", 0)
	if appErr != nil {
		return domain.ComparisonResult{}, appErr
	}
	runs = append(runs, live)

	resume, appErr := runResume(ctx, filepath.Join(root, "resume.db"), sampleDir, seed, sampleID, params)
	if appErr != nil {
		return domain.ComparisonResult{}, appErr
	}
	runs = append(runs, resume)

	reset, appErr := runSingle(ctx, filepath.Join(root, "reset.db"), sampleDir, seed, sampleID, params, "reset", 0)
	if appErr != nil {
		return domain.ComparisonResult{}, appErr
	}
	runs = append(runs, reset)
	return compareRuns(runs)
}

func runSingle(ctx context.Context, dbPath, sampleDir string, seed int64, sampleID string, params domain.Params, mode string, autoPauseAt int) (pathRun, *domain.AppError) {
	start := time.Now()
	db, err := store.Open(dbPath)
	if err != nil {
		return pathRun{}, domain.Errorf(domain.CodeStore, "cannot open comparison database: %v", err)
	}
	defer db.Close()
	engine := &Engine{Store: db, SampleDir: sampleDir, Seed: seed}
	control := NewControl(params, 0, "")
	if autoPauseAt > 0 {
		control.SetAutoPauseAfter(autoPauseAt)
	}
	result, appErr := engine.Run(ctx, sampleID, params, mode, control, nil)
	return pathRun{name: mode, result: result, err: appErr, took: time.Since(start)}, nil
}

func runResume(ctx context.Context, dbPath, sampleDir string, seed int64, sampleID string, params domain.Params) (pathRun, *domain.AppError) {
	start := time.Now()
	first, appErr := runSingle(ctx, dbPath, sampleDir, seed, sampleID, params, "resume", 3)
	if appErr != nil {
		return pathRun{}, appErr
	}
	if first.err != nil {
		return first, nil
	}
	if first.result == nil {
		return first, nil
	}
	if first.result.Snapshot.Run.Status != "paused" && first.result.Snapshot.Run.Status != "completed" {
		return first, nil
	}
	// The first store has been closed by runSingle. Reopen the same file to
	// simulate a process restart and resume exclusively from committed state.
	second, appErr := runSingle(ctx, dbPath, sampleDir, seed, sampleID, params, "resume", 0)
	if appErr != nil {
		return pathRun{}, appErr
	}
	second.name = "resume"
	second.took = time.Since(start)
	return second, nil
}

func compareRuns(runs []pathRun) (domain.ComparisonResult, *domain.AppError) {
	result := domain.ComparisonResult{
		Equal:        true,
		Message:      "all paths match",
		TilesEqual:   true,
		LinesEqual:   true,
		CommitsEqual: true,
		Paths:        make([]domain.ComparisonPath, 0, len(runs)),
	}
	for _, run := range runs {
		path := domain.ComparisonPath{
			Name:       run.name,
			DurationMS: run.took.Milliseconds(),
			Error:      run.err,
		}
		if run.result != nil {
			path.RunID = fmt.Sprint(run.result.RunID)
			path.Status = run.result.Snapshot.Run.Status
			path.Stats = BuildRunView(run.result.Sample, run.result.Snapshot, run.result.Invalidated).Stats
			path.CommitCount = len(run.result.Snapshot.Commits)
		} else if run.err != nil {
			path.Status = "failed"
		}
		result.Paths = append(result.Paths, path)
	}
	base := runs[0]
	if base.result == nil {
		e := domain.Errorf(domain.CodeDiff, "live comparison path did not produce a run")
		e.DifferenceKey = "live_result"
		result.Equal = false
		result.Code = domain.CodeDiff
		result.Message = e.Message
		result.FirstDiff = e
		return result, e
	}
	baseView := BuildRunView(base.result.Sample, base.result.Snapshot, nil)
	for i := 1; i < len(runs); i++ {
		other := runs[i]
		if other.result == nil {
			e := diffError(fmt.Sprintf("%s path did not produce a run", other.name), "result", nil, nil, baseView)
			result.Equal = false
			result.Code = domain.CodeDiff
			result.Message = e.Message
			result.FirstDiff = e
			return result, e
		}
		view := BuildRunView(other.result.Sample, other.result.Snapshot, nil)
		if ok, key := compareViews(baseView, view); !ok {
			result.Equal = false
			result.Code = domain.CodeDiff
			e := diffError(fmt.Sprintf("%s differs from live at %s", other.name, key), key, nil, nil, baseView)
			switch {
			case strings.HasPrefix(key, "tile"):
				if tileIndex, parseErr := parseIndex(key, "tile"); parseErr == nil && tileIndex < len(baseView.Tiles) {
					tile := baseView.Tiles[tileIndex]
					e.TileIndex = tile.Index
					e.TileX = tile.X
					e.TileY = tile.Y
				}
			case strings.HasPrefix(key, "line"):
				if lineIndex, parseErr := parseIndex(key, "line"); parseErr == nil && lineIndex < len(baseView.Lines) {
					line := baseView.Lines[lineIndex]
					e.LineIndex = line.ReadingIndex
					tileIndex, x, y := tileCoordinatesForLine(line, baseView.Tiles)
					e.TileIndex = tileIndex
					e.TileX = x
					e.TileY = y
				}
			case strings.HasPrefix(key, "commit"):
				if commitIndex, parseErr := parseIndex(key, "commit"); parseErr == nil && commitIndex < len(baseView.Commits) {
					commit := baseView.Commits[commitIndex]
					if strings.HasPrefix(commit.UnitKey, "line:") {
						if lineIndex, parseErr := strconv.Atoi(strings.TrimPrefix(commit.UnitKey, "line:")); parseErr == nil && lineIndex < len(baseView.Lines) {
							line := baseView.Lines[lineIndex]
							e.LineIndex = lineIndex
							tileIndex, x, y := tileCoordinatesForLine(line, baseView.Tiles)
							e.TileIndex = tileIndex
							e.TileX = x
							e.TileY = y
						}
					} else if strings.HasPrefix(commit.UnitKey, "tile:") {
						if tileIndex, parseErr := strconv.Atoi(strings.TrimPrefix(commit.UnitKey, "tile:")); parseErr == nil && tileIndex < len(baseView.Tiles) {
							tile := baseView.Tiles[tileIndex]
							e.TileIndex = tileIndex
							e.TileX = tile.X
							e.TileY = tile.Y
						}
					}
				}
			}
			result.Message = e.Message
			result.FirstDiff = e
			return result, e
		}
	}
	result.ComparedLines = len(baseView.Lines)
	result.ComparedCommits = len(baseView.Commits)
	return result, nil
}

func compareViews(a, b domain.RunView) (bool, string) {
	if a.Status != b.Status {
		return false, "status"
	}
	if len(a.Tiles) != len(b.Tiles) {
		return false, "tiles.length"
	}
	for i := range a.Tiles {
		if ok, key := tileMatches(a.Tiles[i], b.Tiles[i]); !ok {
			return false, fmt.Sprintf("tile[%d].%s", i, key)
		}
	}
	if len(a.Lines) != len(b.Lines) {
		return false, "lines.length"
	}
	for i := range a.Lines {
		if ok, key := lineMatches(a.Lines[i], b.Lines[i]); !ok {
			return false, fmt.Sprintf("line[%d].%s", i, key)
		}
	}
	if len(a.Commits) != len(b.Commits) {
		return false, "commits.length"
	}
	for i := range a.Commits {
		if ok, key := commitMatches(a.Commits[i], b.Commits[i]); !ok {
			return false, fmt.Sprintf("commit[%d].%s", i, key)
		}
	}
	if a.Stats.TileCount != b.Stats.TileCount ||
		a.Stats.CommittedTiles != b.Stats.CommittedTiles ||
		a.Stats.DetectedLines != b.Stats.DetectedLines ||
		a.Stats.CommittedLines != b.Stats.CommittedLines ||
		a.Stats.CharCount != b.Stats.CharCount ||
		a.Stats.AvgConfidence != b.Stats.AvgConfidence ||
		a.Stats.MinConfidence != b.Stats.MinConfidence ||
		a.Stats.TextAccuracy != b.Stats.TextAccuracy {
		return false, "stats"
	}
	return true, ""
}

func diffError(message, key string, tile *domain.TileView, line *domain.LineView, base domain.RunView) *domain.AppError {
	e := domain.Errorf(domain.CodeDiff, message)
	e.DifferenceKey = key
	if dot := strings.LastIndex(key, "."); dot >= 0 {
		e.DifferenceKey = key[dot+1:]
	}
	if tile != nil {
		e.TileIndex = tile.Index
		e.TileX = tile.X
		e.TileY = tile.Y
	}
	if line != nil {
		e.LineIndex = line.ReadingIndex
		tileIndex, x, y := tileCoordinatesForLine(*line, base.Tiles)
		e.TileIndex = tileIndex
		e.TileX = x
		e.TileY = y
	}
	return e
}

func parseIndex(key, prefix string) (int, error) {
	start := strings.Index(key, prefix+"[")
	if start < 0 {
		return 0, fmt.Errorf("index not found")
	}
	start += len(prefix) + 1
	end := strings.IndexByte(key[start:], ']')
	if end < 0 {
		return 0, fmt.Errorf("index not terminated")
	}
	return strconv.Atoi(key[start : start+end])
}

func tempDir() (string, error) {
	return os.MkdirTemp("", "ocr-workbench-compare-*")
}
