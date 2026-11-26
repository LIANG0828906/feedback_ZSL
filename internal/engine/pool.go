package engine

import (
	"context"
	"sort"
	"sync"

	"screenrelay/internal/dispatch"
	"screenrelay/internal/mapping"
	"screenrelay/internal/model"
)

type ScreenTask struct {
	WindowSeq        int
	Tick             int64
	Region           model.Rect
	Screen           model.Screen
	RequestedScale   model.ScaleTier
	TimeQuantum      int64
	MaxPacketBytes   int64
	ContentSeed      int64
	JoinedThisWindow bool
}

type Outcome struct {
	ScreenID string
	Mapping  *mapping.Result
	Dispatch *dispatch.ScreenDispatch
	Budget   BudgetRecord
	Err      error
}

func RunScreenPool(ctx context.Context, cfg BudgetConfig, tasks []ScreenTask) []Outcome {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 1
	}
	if cfg.BufferPixelsPerThread <= 0 {
		cfg.BufferPixelsPerThread = 4096
	}
	ordered := append([]ScreenTask(nil), tasks...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Screen.ID < ordered[j].Screen.ID })
	outcomes := make([]Outcome, len(ordered))
	buffers := make([][]byte, cfg.MaxParallel)
	for i := range buffers {
		buffers[i] = make([]byte, cfg.BufferPixelsPerThread*model.PixelBytes)
	}

	for start := 0; start < len(ordered); start += cfg.MaxParallel {
		end := start + cfg.MaxParallel
		if end > len(ordered) {
			end = len(ordered)
		}
		var wg sync.WaitGroup
		for offset := 0; offset < end-start; offset++ {
			idx := start + offset
			slot := idx % cfg.MaxParallel
			task := ordered[idx]
			wave := idx / cfg.MaxParallel
			budget := BudgetRecord{
				WindowSeq: task.WindowSeq, ScreenID: task.Screen.ID, Wave: wave, Slot: slot,
				Queued: wave > 0, BufferPixels: cfg.BufferPixelsPerThread,
				MaxParallel: cfg.MaxParallel,
			}
			if budget.Queued {
				budget.Reason = "queued: all thread pixel buffers are busy; acquired in next bounded wave"
			} else {
				budget.Reason = "acquired worker slot and per-thread pixel buffer"
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if recovered := recover(); recovered != nil {
						outcomes[idx] = Outcome{
							ScreenID: task.Screen.ID, Budget: budget,
							Err: model.NewError(model.EInternal, "isolated worker panic for screen %s: %v", task.Screen.ID, recovered),
						}
					}
				}()
				if err := ctx.Err(); err != nil {
					outcomes[idx] = Outcome{ScreenID: task.Screen.ID, Budget: budget, Err: err}
					return
				}
				mapped, err := mapping.MapScreen(mapping.Request{
					Region: task.Region, Screen: task.Screen, Requested: task.RequestedScale,
					TimeQuantum: task.TimeQuantum, PacketBytes: task.MaxPacketBytes,
				}, task.Tick)
				if err != nil {
					outcomes[idx] = Outcome{ScreenID: task.Screen.ID, Budget: budget, Err: err}
					return
				}
				packets, contentHash, err := dispatch.SplitPacketsBudgeted(
					task.WindowSeq, task.Screen.ID, mapped.PixelRegion,
					task.MaxPacketBytes, task.ContentSeed,
					cfg.BufferPixelsPerThread, buffers[slot],
				)
				if err != nil {
					outcomes[idx] = Outcome{ScreenID: task.Screen.ID, Budget: budget, Err: err}
					return
				}
				outcomes[idx] = Outcome{
					ScreenID: task.Screen.ID, Mapping: mapped, Budget: budget,
					Dispatch: &dispatch.ScreenDispatch{ScreenID: task.Screen.ID, ContentHash: contentHash, Packets: packets},
				}
			}()
		}
		wg.Wait()
	}
	return outcomes
}
