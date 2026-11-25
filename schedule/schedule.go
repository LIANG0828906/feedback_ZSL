package schedule

import (
	"fmt"
	"sort"

	"lipsync/domain"
	"lipsync/merge"
)

// ValidateProject checks render and audio constraints that must agree before
// quantization. A declared samples-per-frame value is authoritative when
// present, making frame-rate/sample-rate contradictions explicit.
func ValidateProject(p *domain.Project) error {
	if p == nil {
		return &domain.Error{Code: domain.CodeSchema, Message: "nil project"}
	}
	r := p.Render
	if r.TargetFPS <= 0 {
		return &domain.Error{
			Code: domain.CodeRate, Field: "render.target_fps",
			Message: "target frame rate must be positive",
		}
	}
	if !r.Rounding.Valid() {
		return &domain.Error{
			Code: domain.CodeSchema, Field: "render.rounding",
			Value: string(r.Rounding), Message: "invalid rounding mode",
		}
	}
	if r.FrameBudget <= 0 || r.ChannelSlots <= 0 {
		return &domain.Error{
			Code: domain.CodeParam, Field: "render.frame_budget",
			Message: "frame budget and channel slots must be positive",
		}
	}
	if r.MaxPhonemesPerPackage <= 0 {
		return &domain.Error{
			Code: domain.CodeParam, Field: "render.max_phonemes_per_package",
			Message: "max phonemes per package must be positive",
		}
	}
	if r.MinVisibleFrames < 0 || r.MaxDelayFrames < 0 {
		return &domain.Error{
			Code: domain.CodeParam, Field: "render.visibility",
			Message: "minimum visible frames and max delay must not be negative",
		}
	}
	seenShard := make(map[int]struct{})
	seenID := make(map[string]struct{})
	for _, d := range p.Dialogues {
		if d.ID == "" {
			return &domain.Error{Code: domain.CodeSchema, Field: "dialogues.id", Message: "empty dialogue id"}
		}
		if _, ok := seenID[d.ID]; ok {
			return &domain.Error{
				Code: domain.CodeSchema, Field: "dialogues.id",
				DialogueID: d.ID, ShardIndex: d.ShardIndex, Message: "duplicate dialogue id",
			}
		}
		seenID[d.ID] = struct{}{}
		if _, ok := seenShard[d.ShardIndex]; ok {
			return &domain.Error{
				Code: domain.CodeSchema, Field: "dialogues.shard_index",
				DialogueID: d.ID, ShardIndex: d.ShardIndex, Message: "duplicate shard index",
			}
		}
		seenShard[d.ShardIndex] = struct{}{}
		if d.Channel < 0 {
			return &domain.Error{
				Code: domain.CodeParam, Field: "dialogues.channel",
				DialogueID: d.ID, ShardIndex: d.ShardIndex, Message: "negative channel",
			}
		}
		if d.PCM.SampleRate <= 0 || d.PCM.Channels != 1 {
			return &domain.Error{
				Code: domain.CodeRate, Field: "pcm.sample_rate",
				DialogueID: d.ID, ShardIndex: d.ShardIndex,
				Message: "dialogue must be mono PCM with a positive sample rate",
			}
		}
		if r.SamplesPerFrame > 0 {
			if r.SamplesPerFrame > (1<<62)/r.TargetFPS ||
				r.SamplesPerFrame*r.TargetFPS != d.PCM.SampleRate {
				first := ""
				if len(d.Phonemes) > 0 {
					first = d.Phonemes[0].ID
				}
				return &domain.Error{
					Code: domain.CodeRate, Field: "render.samples_per_frame",
					DialogueID: d.ID, ShardIndex: d.ShardIndex, PhonemeID: first,
					OperandA: r.SamplesPerFrame, OperandB: d.PCM.SampleRate,
					Message: "samples_per_frame * fps does not equal PCM sample_rate",
				}
			}
		}
	}
	return nil
}

// Build validates the project and deterministically eliminates input
// enumeration order by sorting dialogues, phonemes, and packages.
func Build(p *domain.Project) ([]domain.Package, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	dialogues := append([]domain.Dialogue(nil), p.Dialogues...)
	sort.Slice(dialogues, func(i, j int) bool {
		if dialogues[i].ShardIndex != dialogues[j].ShardIndex {
			return dialogues[i].ShardIndex < dialogues[j].ShardIndex
		}
		return dialogues[i].ID < dialogues[j].ID
	})
	var packages []domain.Package
	for _, d := range dialogues {
		pkgs, err := merge.Partition(d, p.Render)
		if err != nil {
			return nil, err
		}
		packages = append(packages, pkgs...)
	}
	sort.Slice(packages, func(i, j int) bool {
		if packages[i].StartFrame != packages[j].StartFrame {
			return packages[i].StartFrame < packages[j].StartFrame
		}
		if packages[i].Channel != packages[j].Channel {
			return packages[i].Channel < packages[j].Channel
		}
		if packages[i].DialogueID != packages[j].DialogueID {
			return packages[i].DialogueID < packages[j].DialogueID
		}
		return packages[i].ID < packages[j].ID
	})
	return packages, nil
}

type candidate struct {
	pkg    domain.Package
	delays int
}

type reservation struct {
	pkg   domain.Package
	start int64
	end   int64
}

// Schedule applies a deterministic non-preemptive interval scheduler. A
// package is retried at the next frame when any frame in its duration lacks
// either the global per-frame budget or a per-channel slot. It is rejected
// only after max_delay_frames have elapsed.
func Schedule(p *domain.Project, packages []domain.Package) (*domain.ScheduleResult, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	pkgs := append([]domain.Package(nil), packages...)
	sort.Slice(pkgs, func(i, j int) bool {
		if pkgs[i].StartFrame != pkgs[j].StartFrame {
			return pkgs[i].StartFrame < pkgs[j].StartFrame
		}
		if pkgs[i].Channel != pkgs[j].Channel {
			return pkgs[i].Channel < pkgs[j].Channel
		}
		if pkgs[i].DialogueID != pkgs[j].DialogueID {
			return pkgs[i].DialogueID < pkgs[j].DialogueID
		}
		return pkgs[i].ID < pkgs[j].ID
	})

	result := &domain.ScheduleResult{}
	if len(pkgs) == 0 {
		return result, nil
	}

	budgetByFrame := make(map[int64]int)
	channelByFrame := make(map[int64]map[int]int)
	reservations := make([]reservation, 0, len(pkgs))
	var pending []*candidate
	next := 0
	var frame int64

	for {
		if len(pending) == 0 && next >= len(pkgs) {
			break
		}
		if len(pending) == 0 && next < len(pkgs) && frame < pkgs[next].StartFrame {
			frame = pkgs[next].StartFrame
		}
		for next < len(pkgs) && pkgs[next].StartFrame <= frame {
			pending = append(pending, &candidate{pkg: pkgs[next]})
			next++
		}
		sort.SliceStable(pending, func(i, j int) bool {
			if pending[i].pkg.StartFrame != pending[j].pkg.StartFrame {
				return pending[i].pkg.StartFrame < pending[j].pkg.StartFrame
			}
			if pending[i].pkg.Channel != pending[j].pkg.Channel {
				return pending[i].pkg.Channel < pending[j].pkg.Channel
			}
			if pending[i].pkg.DialogueID != pending[j].pkg.DialogueID {
				return pending[i].pkg.DialogueID < pending[j].pkg.DialogueID
			}
			return pending[i].pkg.ID < pending[j].pkg.ID
		})

		remain := pending[:0]
		for _, c := range pending {
			duration := c.pkg.EndFrame - c.pkg.StartFrame
			if duration <= 0 {
				return nil, &domain.Error{
					Code: domain.CodeOrder, Field: "package.interval",
					DialogueID: c.pkg.DialogueID, ShardIndex: c.pkg.ShardIndex,
					Message: "package has non-positive duration",
				}
			}
			if p.Render.MinVisibleFrames > duration {
				duration = p.Render.MinVisibleFrames
			}
			if ok, blockedFrame, reason := canPlace(frame, duration, c.pkg, p.Render, budgetByFrame, channelByFrame); ok {
				for t := frame; t < frame+duration; t++ {
					budgetByFrame[t]++
					if channelByFrame[t] == nil {
						channelByFrame[t] = make(map[int]int)
					}
					channelByFrame[t][c.pkg.Channel]++
				}
				reservations = append(reservations, reservation{pkg: c.pkg, start: frame, end: frame + duration})
				result.Events = append(result.Events, domain.DispatchEvent{
					Frame: frame, PackageID: c.pkg.ID, DialogueID: c.pkg.DialogueID,
					ShardIndex: c.pkg.ShardIndex, Channel: c.pkg.Channel,
					Status: "accepted", Reason: "capacity available",
				})
				if c.delays > 0 {
					result.Events = append(result.Events, domain.DispatchEvent{
						Frame: frame, PackageID: c.pkg.ID, DialogueID: c.pkg.DialogueID,
						ShardIndex: c.pkg.ShardIndex, Channel: c.pkg.Channel,
						Status: "retry", Reason: fmt.Sprintf("accepted after %d delay(s)", c.delays),
					})
				}
			} else {
				deadline := c.pkg.StartFrame + p.Render.MaxDelayFrames
				if frame >= deadline {
					result.Events = append(result.Events, domain.DispatchEvent{
						Frame: frame, PackageID: c.pkg.ID, DialogueID: c.pkg.DialogueID,
						ShardIndex: c.pkg.ShardIndex, Channel: c.pkg.Channel,
						Status: "rejected", Reason: reason,
					})
					continue
				}
				nextFrame := frame + 1
				result.Events = append(result.Events, domain.DispatchEvent{
					Frame: frame, PackageID: c.pkg.ID, DialogueID: c.pkg.DialogueID,
					ShardIndex: c.pkg.ShardIndex, Channel: c.pkg.Channel,
					Status:     "delayed",
					Reason:     fmt.Sprintf("frame %d blocked: %s", blockedFrame, reason),
					RetryFrame: nextFrame,
				})
				c.delays++
				remain = append(remain, c)
			}
		}
		pending = remain
		frame++
	}

	sort.Slice(reservations, func(i, j int) bool {
		if reservations[i].start != reservations[j].start {
			return reservations[i].start < reservations[j].start
		}
		return reservations[i].pkg.ID < reservations[j].pkg.ID
	})
	byFrame := make(map[int64][]string)
	for _, r := range reservations {
		for f := r.start; f < r.end; f++ {
			byFrame[f] = append(byFrame[f], r.pkg.ID)
		}
	}
	frames := make([]int64, 0, len(byFrame))
	for f := range byFrame {
		frames = append(frames, f)
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i] < frames[j] })
	for _, f := range frames {
		ids := byFrame[f]
		sort.Strings(ids)
		result.Frames = append(result.Frames, domain.FrameRecord{Frame: f, PackageIDs: ids})
	}

	scheduled := make([]domain.ScheduledPackage, 0, len(pkgs))
	statusByID := make(map[string]struct {
		status string
		start  int64
		end    int64
	})
	for _, r := range reservations {
		statusByID[r.pkg.ID] = struct {
			status string
			start  int64
			end    int64
		}{status: "accepted", start: r.start, end: r.end}
	}
	for _, pkg := range pkgs {
		if s, ok := statusByID[pkg.ID]; ok {
			scheduled = append(scheduled, domain.ScheduledPackage{
				Package: pkg, Status: s.status,
				DispatchStartFrame: s.start, DispatchEndFrame: s.end,
			})
		} else {
			scheduled = append(scheduled, domain.ScheduledPackage{
				Package: pkg, Status: "rejected",
			})
		}
	}
	result.Packages = scheduled
	if err := ValidateSchedule(p.Render, result); err != nil {
		return nil, err
	}
	return result, nil
}

func canPlace(frame, duration int64, pkg domain.Package, cfg domain.RenderConfig,
	budget map[int64]int, channels map[int64]map[int]int) (bool, int64, string) {
	for t := frame; t < frame+duration; t++ {
		if budget[t] >= cfg.FrameBudget {
			return false, t, fmt.Sprintf("global budget %d/%d", budget[t], cfg.FrameBudget)
		}
		if channels[t] != nil && channels[t][pkg.Channel] >= cfg.ChannelSlots {
			return false, t, fmt.Sprintf("channel %d slots %d/%d", pkg.Channel, channels[t][pkg.Channel], cfg.ChannelSlots)
		}
	}
	return true, 0, ""
}

// ValidateSchedule is public so persisted artifacts from any path can be
// checked independently. It returns E_BUDGET at the first violating frame.
func ValidateSchedule(cfg domain.RenderConfig, result *domain.ScheduleResult) error {
	byID := make(map[string]domain.ScheduledPackage, len(result.Packages))
	for _, p := range result.Packages {
		byID[p.ID] = p
	}
	for _, fr := range result.Frames {
		if len(fr.PackageIDs) > cfg.FrameBudget {
			ch := 0
			if len(fr.PackageIDs) > 0 {
				ch = byID[fr.PackageIDs[0]].Channel
			}
			return &domain.Error{
				Code: domain.CodeBudget, Message: "per-frame package budget exceeded",
				Field: "frames.package_ids", Frame: fr.Frame, Channel: ch,
				Expected: fmt.Sprintf("<=%d", cfg.FrameBudget),
				Actual:   fmt.Sprintf("%d", len(fr.PackageIDs)),
			}
		}
		seen := make(map[string]struct{}, len(fr.PackageIDs))
		perChannel := make(map[int]int)
		for _, id := range fr.PackageIDs {
			if _, ok := seen[id]; ok {
				return &domain.Error{
					Code: domain.CodeBudget, Message: "duplicate package in a frame",
					Field: "frames.package_ids", Frame: fr.Frame,
				}
			}
			seen[id] = struct{}{}
			p, ok := byID[id]
			if !ok {
				return &domain.Error{
					Code: domain.CodeOrder, Message: "frame references unknown package",
					Field: "frames.package_ids", Frame: fr.Frame, Value: id,
				}
			}
			perChannel[p.Channel]++
			if perChannel[p.Channel] > cfg.ChannelSlots {
				return &domain.Error{
					Code: domain.CodeBudget, Message: "per-channel concurrent slot limit exceeded",
					Field: "frames.channel_slots", Frame: fr.Frame, Channel: p.Channel,
					Expected: fmt.Sprintf("<=%d", cfg.ChannelSlots),
					Actual:   fmt.Sprintf("%d", perChannel[p.Channel]),
				}
			}
		}
	}
	return nil
}
