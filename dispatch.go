package lipsync

import (
	"fmt"
	"sort"
)

type scheduledPack struct {
	pack Pack
}

func DispatchPacks(packs []Pack, recipe Recipe) *DispatchResult {
	channelCount := recipe.SpeakerChannels
	if channelCount <= 0 {
		channelCount = 1
	}
	budget := recipe.FramePackBudget
	maxDefer := recipe.MaxDeferFrames

	orderedInput := append([]Pack(nil), packs...)
	sort.SliceStable(orderedInput, func(i, j int) bool {
		if orderedInput[i].StartFrame != orderedInput[j].StartFrame {
			return orderedInput[i].StartFrame < orderedInput[j].StartFrame
		}
		if orderedInput[i].ShardIndex != orderedInput[j].ShardIndex {
			return orderedInput[i].ShardIndex < orderedInput[j].ShardIndex
		}
		return orderedInput[i].Index < orderedInput[j].Index
	})

	byStart := map[int][]Pack{}
	maxStart := 0
	for _, pack := range orderedInput {
		byStart[pack.StartFrame] = append(byStart[pack.StartFrame], pack)
		if pack.StartFrame > maxStart {
			maxStart = pack.StartFrame
		}
	}

	busyUntil := make([]int, channelCount)
	activePackID := make([]string, channelCount)
	deliveredPackIDs := make([][]string, channelCount)
	queue := make([]Pack, 0)
	result := &DispatchResult{ChannelCount: channelCount}

	for frame := 0; ; frame++ {
		if started, ok := byStart[frame]; ok {
			queue = append(queue, started...)
		}
		sort.SliceStable(queue, func(i, j int) bool {
			if queue[i].StartFrame != queue[j].StartFrame {
				return queue[i].StartFrame < queue[j].StartFrame
			}
			if queue[i].ShardIndex != queue[j].ShardIndex {
				return queue[i].ShardIndex < queue[j].ShardIndex
			}
			return queue[i].Index < queue[j].Index
		})

		report := FrameDispatch{
			Frame:       frame,
			BudgetLimit: budget,
			Channels:    make([]ChannelState, channelCount),
		}
		for ch := 0; ch < channelCount; ch++ {
			if busyUntil[ch] <= frame {
				busyUntil[ch] = 0
				activePackID[ch] = ""
			}
			report.Channels[ch] = ChannelState{
				Channel:          ch,
				PackIDs:          []string{},
				DeliveredPackIDs: append([]string{}, deliveredPackIDs[ch]...),
			}
		}

		remaining := make([]Pack, 0, len(queue))
		for _, pack := range queue {
			channel := firstFreeChannel(busyUntil, frame)
			reason := ""
			if channel < 0 {
				reason = "channel_busy"
			} else if report.BudgetUsed >= budget {
				reason = "budget_exhausted"
			}

			if reason != "" {
				delay := frame - pack.StartFrame
				if maxDefer >= 0 && delay >= maxDefer {
					rejection := RejectionRecord{
						DialogueID: pack.DialogueID,
						ShardIndex: pack.ShardIndex,
						PackIndex:  pack.Index,
						PackID:     pack.PackID,
						LastFrame:  frame,
						Reason:     reason,
						Detail: fmt.Sprintf(
							"pack original frame [%d,%d) could not be accepted within %d defer frame(s)",
							pack.StartFrame, pack.EndFrame, maxDefer,
						),
					}
					result.Rejections = append(result.Rejections, rejection)
					report.RejectedThisFrame = append(report.RejectedThisFrame, rejection)
					continue
				}
				deferral := DeferralRecord{
					DialogueID:        pack.DialogueID,
					ShardIndex:        pack.ShardIndex,
					PackIndex:         pack.Index,
					PackID:            pack.PackID,
					Frame:             frame,
					NextEligibleFrame: frame + 1,
					Reason:            reason,
				}
				result.Deferrals = append(result.Deferrals, deferral)
				report.DeferredThisFrame = append(report.DeferredThisFrame, deferral)
				remaining = append(remaining, pack)
				continue
			}

			duration := pack.EndFrame - pack.StartFrame
			if duration <= 0 {
				duration = 1
			}
			assignment := Assignment{
				Frame:         frame,
				Channel:       channel,
				DialogueID:    pack.DialogueID,
				ShardIndex:    pack.ShardIndex,
				PackIndex:     pack.Index,
				PackID:        pack.PackID,
				OriginalStart: pack.StartFrame,
				OriginalEnd:   pack.EndFrame,
				ActiveFrom:    frame,
				ActiveTo:      frame + duration,
				DelayFrames:   frame - pack.StartFrame,
			}
			busyUntil[channel] = assignment.ActiveTo
			activePackID[channel] = pack.PackID
			deliveredPackIDs[channel] = append(deliveredPackIDs[channel], pack.PackID)
			report.BudgetUsed++
			report.Dispatched = append(report.Dispatched, assignment)
			result.Assignments = append(result.Assignments, assignment)
		}
		queue = remaining

		for ch := 0; ch < channelCount; ch++ {
			report.Channels[ch].DeliveredPackIDs = append([]string{}, deliveredPackIDs[ch]...)
			if busyUntil[ch] > frame && activePackID[ch] != "" {
				report.ActivePackCount++
				report.Channels[ch].PackIDs = []string{activePackID[ch]}
			}
		}
		report.BudgetOccupied = report.BudgetUsed
		result.Frames = append(result.Frames, report)

		if len(queue) == 0 {
			anyActive := false
			for ch := 0; ch < channelCount; ch++ {
				if busyUntil[ch] > frame+1 {
					anyActive = true
					break
				}
			}
			if !anyActive && frame >= maxStart {
				result.FrameCount = len(result.Frames)
				return result
			}
		}
		if frame > maxStart+maxDefer+10000 {
			panic("lipsync: dispatch scheduler exceeded deterministic safety horizon")
		}
	}
}

func DispatchPacksStream(packs []Pack, recipe Recipe, onFrame func(FrameDispatch)) *DispatchResult {
	result := DispatchPacks(packs, recipe)
	if onFrame != nil {
		for _, frame := range result.Frames {
			onFrame(frame)
		}
	}
	return result
}

func firstFreeChannel(busyUntil []int, frame int) int {
	for ch, until := range busyUntil {
		if until <= frame {
			return ch
		}
	}
	return -1
}
