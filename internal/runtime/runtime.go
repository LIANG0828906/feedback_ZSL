package runtime

import (
	"context"

	"replaybench/internal/checkpoint"
	"replaybench/internal/eerr"
	"replaybench/internal/engine"
	"replaybench/internal/model"
	"replaybench/internal/scheduler"
)

type Manager struct {
	Store       *checkpoint.Store
	Pool        *scheduler.Pool
	WindowSize  int
	AfterCommit func(windowIndex int)
}

type Report struct {
	Result          *engine.Result
	Windows         []model.Window
	RecoveredWindow *model.Window
	StartRound      int
	CommitSequence  []int
}

func (m *Manager) Run(ctx context.Context, replay *model.Replay) (*Report, error) {
	windowSize := m.WindowSize
	if windowSize <= 0 {
		windowSize = 2
	}
	identity := checkpoint.IdentityFromReplay(replay)
	last, err := m.Store.Recover(identity)
	if err != nil {
		return nil, err
	}
	report := &Report{RecoveredWindow: last, StartRound: 1}
	state := engine.NewState(replay)
	nextIndex := 1
	if last != nil {
		report.StartRound = last.LastRound + 1
		if len(last.Snapshots) > 0 {
			state = engine.StateFromSnapshot(last.Snapshots[len(last.Snapshots)-1])
		}
		nextIndex = last.Index + 1
	}

	for from := report.StartRound; from <= replay.Rounds; from += windowSize {
		end := from + windowSize - 1
		if end > replay.Rounds {
			end = replay.Rounds
		}
		windowIndex := nextIndex
		task := func(taskCtx context.Context) error {
			snaps := []model.RoundSnapshot{}
			for round := from; round <= end; round++ {
				snap, stepErr := engine.Step(taskCtx, replay, state, round)
				if stepErr != nil {
					return stepErr
				}
				snaps = append(snaps, snap)
			}
			lastSnap := snaps[len(snaps)-1]
			window := &model.Window{
				Index: windowIndex, FirstRound: from, LastRound: end,
				NextOffset: lastSnap.ConsumedOffset, ConsumedOffset: lastSnap.ConsumedOffset,
				StateDigest: lastSnap.StateDigest, ContentHash: replay.ContentHash,
				SourceFormatVer: replay.SourceFormatVersion, ParamsVersion: model.ParamsVersion,
				AlgorithmVersion: model.AlgorithmVersion, Snapshots: snaps,
			}
			return m.Store.CommitWindow(taskCtx, window)
		}
		if m.Pool != nil {
			if err := m.Pool.Submit(ctx, "window-"+itoa(windowIndex), task); err != nil {
				return report, err
			}
		} else {
			if err := task(ctx); err != nil {
				return report, err
			}
		}
		if m.AfterCommit != nil {
			m.AfterCommit(windowIndex)
		}
		nextIndex++
	}

	windows, err := m.Store.LoadWindows()
	if err != nil {
		return report, err
	}
	report.Windows = windows
	seq, err := m.Store.ListWindowIndexes()
	if err != nil {
		return report, err
	}
	report.CommitSequence = seq
	snaps := []model.RoundSnapshot{}
	for _, w := range windows {
		snaps = append(snaps, w.Snapshots...)
	}
	if len(snaps) != replay.Rounds {
		return report, eerr.New(eerr.ECheckpoint, "$.windows", "loaded %d snapshots want %d", len(snaps), replay.Rounds)
	}
	verdict, winner := engine.Verdict(snaps[len(snaps)-1])
	report.Result = &engine.Result{Snapshots: snaps, Verdict: verdict, WinnerTeam: winner}
	return report, nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
