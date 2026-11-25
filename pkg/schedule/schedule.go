package schedule

import (
	"fmt"
	"sort"

	"lipsync.local/prototype/pkg/domain"
)

type Config struct {
	MaxPackagesPerFrame        int
	MaxPackagesPerChannelFrame int
	MaxDeferFrames             int
	Channels                   map[string]int
}

func ConfigFromRecipe(recipe domain.Recipe) Config {
	channels := make(map[string]int, len(recipe.Channels))
	for _, ch := range recipe.Channels {
		channels[ch.ID] = ch.Slots
	}
	return Config{
		MaxPackagesPerFrame:        recipe.MaxPackagesPerFrame,
		MaxPackagesPerChannelFrame: recipe.MaxPackagesPerChannelFrame,
		MaxDeferFrames:             recipe.MaxDeferFrames,
		Channels:                   channels,
	}
}

type request struct {
	pkg       domain.LipPackage
	start     int
	nextFrame int
	deadline  int
	done      bool
}

// Dispatch performs deterministic frame-budget scheduling. A package claims one
// global slot and one channel slot at its dispatch frame. It may be retried on
// subsequent frames until MaxDeferFrames is exceeded, after which it is rejected
// without charging any budget.
func Dispatch(packages []domain.LipPackage, cfg Config) (domain.DispatchResult, error) {
	result := domain.DispatchResult{
		DispatchFrame: make(map[string]int),
	}
	if cfg.MaxPackagesPerFrame < 1 {
		return result, &domain.Error{Code: domain.ErrConflict, Field: "max_packages_per_frame", Reason: "must be positive"}
	}
	if cfg.MaxPackagesPerChannelFrame < 1 {
		return result, &domain.Error{Code: domain.ErrConflict, Field: "max_packages_per_channel_frame", Reason: "must be positive"}
	}
	if cfg.MaxDeferFrames < 0 {
		return result, &domain.Error{Code: domain.ErrConflict, Field: "max_defer_frames", Reason: "must not be negative"}
	}
	if len(cfg.Channels) == 0 {
		return result, &domain.Error{Code: domain.ErrConflict, Field: "channels", Reason: "no channels configured"}
	}

	requests := make([]request, 0, len(packages))
	seen := make(map[string]struct{}, len(packages))
	minFrame := 0
	for i, pkg := range packages {
		if pkg.ID == "" || pkg.DialogueID == "" {
			return result, &domain.Error{Code: domain.ErrConflict, PackageID: pkg.ID, DialogueID: pkg.DialogueID, Reason: "package identity missing"}
		}
		if _, exists := seen[pkg.ID]; exists {
			return result, &domain.Error{Code: domain.ErrConflict, PackageID: pkg.ID, DialogueID: pkg.DialogueID, Reason: "duplicate package id"}
		}
		seen[pkg.ID] = struct{}{}
		if pkg.EndFrame <= pkg.StartFrame || pkg.StartFrame < 0 {
			return result, &domain.Error{Code: domain.ErrConflict, PackageID: pkg.ID, DialogueID: pkg.DialogueID, Field: "package_frame_range", Reason: "invalid package frame range"}
		}
		slots, ok := cfg.Channels[pkg.Channel]
		if !ok {
			return result, &domain.Error{Code: domain.ErrConflict, PackageID: pkg.ID, DialogueID: pkg.DialogueID, Field: "channel", Reason: "unknown channel"}
		}
		if slots < 1 {
			return result, &domain.Error{Code: domain.ErrConflict, PackageID: pkg.ID, DialogueID: pkg.DialogueID, Field: "channel.slots", Reason: "non-positive channel capacity"}
		}
		if i == 0 || pkg.StartFrame < minFrame {
			minFrame = pkg.StartFrame
		}
		deadline := pkg.StartFrame + cfg.MaxDeferFrames
		if deadline > pkg.EndFrame-1 {
			deadline = pkg.EndFrame - 1
		}
		requests = append(requests, request{pkg: pkg, start: pkg.StartFrame, nextFrame: pkg.StartFrame, deadline: deadline})
	}
	if len(requests) == 0 {
		return result, nil
	}
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].start != requests[j].start {
			return requests[i].start < requests[j].start
		}
		if requests[i].pkg.DialogueID != requests[j].pkg.DialogueID {
			return requests[i].pkg.DialogueID < requests[j].pkg.DialogueID
		}
		return requests[i].pkg.ID < requests[j].pkg.ID
	})
	maxEnd := 0
	for _, req := range requests {
		if req.pkg.EndFrame > maxEnd {
			maxEnd = req.pkg.EndFrame
		}
	}
	lastFrame := 0
	for _, req := range requests {
		if req.deadline > lastFrame {
			lastFrame = req.deadline
		}
	}
	if maxEnd-1 > lastFrame {
		lastFrame = maxEnd - 1
	}
	frames := make([]domain.FrameChannelState, 0, lastFrame-minFrame+1)
	accepted := make(map[string]struct{}, len(requests))
	deferred := make(map[string]struct{})
	rejected := make(map[string]struct{})
	for frame := minFrame; frame <= lastFrame; frame++ {
		state := domain.FrameChannelState{
			Frame: frame, Channels: make(map[string][]string, len(cfg.Channels)),
		}
		for channelID := range cfg.Channels {
			state.Channels[channelID] = []string{}
		}
		channelUsed := make(map[string]int, len(cfg.Channels))
		for i := range requests {
			req := &requests[i]
			if req.done || req.start > frame || req.nextFrame > frame {
				continue
			}
			if req.nextFrame < frame {
				// A request can only become ready once per frame; this branch is
				// defensive and keeps retries explicitly frame-based.
				req.nextFrame = frame
			}
			if frame > req.start {
				result.Events = append(result.Events, domain.DispatchEvent{
					DialogueID: req.pkg.DialogueID, PackageID: req.pkg.ID, Frame: frame,
					Action: "retry", Reason: "retry_after_deferral",
				})
			}
			channelLimit := cfg.Channels[req.pkg.Channel]
			if cfg.MaxPackagesPerChannelFrame < channelLimit {
				channelLimit = cfg.MaxPackagesPerChannelFrame
			}
			globalFull := state.Used >= cfg.MaxPackagesPerFrame
			channelFull := channelUsed[req.pkg.Channel] >= channelLimit
			if !globalFull && !channelFull {
				state.Dispatched = append(state.Dispatched, req.pkg.ID)
				state.Channels[req.pkg.Channel] = append(state.Channels[req.pkg.Channel], req.pkg.ID)
				state.Used++
				channelUsed[req.pkg.Channel]++
				req.done = true
				accepted[req.pkg.ID] = struct{}{}
				result.DispatchFrame[req.pkg.ID] = frame
				result.Events = append(result.Events, domain.DispatchEvent{
					DialogueID: req.pkg.DialogueID, PackageID: req.pkg.ID, Frame: frame,
					Action: "accept", Reason: "budget_and_channel_slot_available",
				})
				continue
			}
			reason := "global_budget_full"
			if globalFull && channelFull {
				reason = "global_budget_and_channel_slot_full"
			} else if channelFull {
				reason = "channel_slot_full"
			}
			if frame >= req.deadline {
				req.done = true
				rejected[req.pkg.ID] = struct{}{}
				result.Events = append(result.Events, domain.DispatchEvent{
					DialogueID: req.pkg.DialogueID, PackageID: req.pkg.ID, Frame: frame,
					Action: "reject", Reason: "dispatch_window_exhausted:" + reason,
				})
				continue
			}
			deferred[req.pkg.ID] = struct{}{}
			req.nextFrame = frame + 1
			result.Events = append(result.Events, domain.DispatchEvent{
				DialogueID: req.pkg.DialogueID, PackageID: req.pkg.ID, Frame: frame,
				Action: "defer", Reason: reason,
			})
		}
		sort.Strings(state.Dispatched)
		for channelID := range state.Channels {
			sort.Strings(state.Channels[channelID])
		}
		if state.Used > cfg.MaxPackagesPerFrame {
			return result, &domain.Error{
				Code: domain.ErrConflict, Frame: frame, HaveFrame: true,
				Field: "frame_budget", Reason: fmt.Sprintf("dispatched %d packages above limit %d", state.Used, cfg.MaxPackagesPerFrame),
			}
		}
		frames = append(frames, state)
	}
	for id := range accepted {
		result.Accepted = append(result.Accepted, id)
	}
	for id := range deferred {
		result.Deferred = append(result.Deferred, id)
	}
	for id := range rejected {
		result.Rejected = append(result.Rejected, id)
	}
	sort.Strings(result.Accepted)
	sort.Strings(result.Deferred)
	sort.Strings(result.Rejected)
	result.Frames = frames
	return result, nil
}

// ForDialogue slices a global dispatch result into the records owned by one
// dialogue while retaining event order.
func ForDialogue(global domain.DispatchResult, dialogueID string) domain.DispatchResult {
	local := domain.DispatchResult{DispatchFrame: make(map[string]int)}
	for _, id := range global.Accepted {
		if pkgDialogue(id) == dialogueID {
			local.Accepted = append(local.Accepted, id)
			local.DispatchFrame[id] = global.DispatchFrame[id]
		}
	}
	for _, id := range global.Deferred {
		if pkgDialogue(id) == dialogueID {
			local.Deferred = append(local.Deferred, id)
		}
	}
	for _, id := range global.Rejected {
		if pkgDialogue(id) == dialogueID {
			local.Rejected = append(local.Rejected, id)
		}
	}
	for _, event := range global.Events {
		if event.DialogueID == dialogueID {
			local.Events = append(local.Events, event)
		}
	}
	for _, frame := range global.Frames {
		state := domain.FrameChannelState{
			Frame: frame.Frame, Channels: map[string][]string{},
		}
		for channelID, ids := range frame.Channels {
			for _, id := range ids {
				if pkgDialogue(id) == dialogueID {
					state.Channels[channelID] = append(state.Channels[channelID], id)
				}
			}
		}
		for _, id := range frame.Dispatched {
			if pkgDialogue(id) == dialogueID {
				state.Dispatched = append(state.Dispatched, id)
			}
		}
		state.Used = len(state.Dispatched)
		local.Frames = append(local.Frames, state)
	}
	return local
}

func pkgDialogue(id string) string {
	const marker = "/pkg/"
	for i := 0; i+len(marker) <= len(id); i++ {
		if id[i:i+len(marker)] == marker {
			return id[:i]
		}
	}
	return ""
}
