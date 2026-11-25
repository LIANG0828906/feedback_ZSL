package generate

import (
	"fmt"
	"sort"

	"lipsync/domain"
	"lipsync/truth"
)

// referencePartition is intentionally a separate implementation from
// internal/merge. It is the generator's analytic packaging oracle.
func referencePartition(d canonicalDialogue) []truth.Package {
	phones := append([]domain.Phoneme(nil), d.Phonemes...)
	sort.Slice(phones, func(i, j int) bool {
		if phones[i].StartFrame != phones[j].StartFrame {
			return phones[i].StartFrame < phones[j].StartFrame
		}
		return phones[i].ID < phones[j].ID
	})
	var out []truth.Package
	for start := 0; start < len(phones); {
		end := start + 1
		if phones[start].Category != domain.CategorySilence {
			for end < len(phones) {
				last, next := phones[end-1], phones[end]
				if next.Category == domain.CategorySilence ||
					last.Category == domain.CategorySilence ||
					last.Affinity < 0.5 ||
					end-start >= 3 ||
					next.StartFrame != last.EndFrame {
					break
				}
				end++
			}
		}
		ids := make([]string, 0, end-start)
		for _, ph := range phones[start:end] {
			ids = append(ids, ph.ID)
		}
		out = append(out, truth.Package{
			ID:         fmt.Sprintf("pkg:%s:%d:%s:%s", d.ID, d.Shard, ids[0], ids[len(ids)-1]),
			DialogueID: d.ID, ShardIndex: d.Shard, Channel: d.Channel,
			StartFrame: phones[start].StartFrame,
			EndFrame:   phones[end-1].EndFrame,
			PhonemeIDs: ids,
		})
		start = end
	}
	return out
}

type refCandidate struct {
	pkg    truth.Package
	delays int
}

type refReservation struct {
	pkg   truth.Package
	start int64
	end   int64
}

// referenceSchedule is a second, independent implementation of the documented
// deterministic interval scheduler.
func referenceSchedule(packages []truth.Package) ([]truth.Package, []truth.Frame, []truth.Event) {
	pkgs := append([]truth.Package(nil), packages...)
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
	const (
		budget       = 3
		channelSlots = 1
		maxDelay     = 8
	)
	budgetByFrame := make(map[int64]int)
	channelByFrame := make(map[int64]map[int]int)
	var pending []*refCandidate
	var reservations []refReservation
	var events []truth.Event
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
			pending = append(pending, &refCandidate{pkg: pkgs[next]})
			next++
		}
		sort.SliceStable(pending, func(i, j int) bool {
			a, b := pending[i].pkg, pending[j].pkg
			if a.StartFrame != b.StartFrame {
				return a.StartFrame < b.StartFrame
			}
			if a.Channel != b.Channel {
				return a.Channel < b.Channel
			}
			if a.DialogueID != b.DialogueID {
				return a.DialogueID < b.DialogueID
			}
			return a.ID < b.ID
		})
		remain := pending[:0]
		for _, c := range pending {
			duration := c.pkg.EndFrame - c.pkg.StartFrame
			if duration < 2 {
				duration = 2 // generated project's declared minimum visible frames
			}
			ok, blocked, reason := refCanPlace(frame, duration, c.pkg, budgetByFrame, channelByFrame, budget, channelSlots)
			if ok {
				for f := frame; f < frame+duration; f++ {
					budgetByFrame[f]++
					if channelByFrame[f] == nil {
						channelByFrame[f] = make(map[int]int)
					}
					channelByFrame[f][c.pkg.Channel]++
				}
				reservations = append(reservations, refReservation{pkg: c.pkg, start: frame, end: frame + duration})
				events = append(events, truth.Event{
					Frame: frame, PackageID: c.pkg.ID, DialogueID: c.pkg.DialogueID,
					ShardIndex: c.pkg.ShardIndex, Channel: c.pkg.Channel,
					Status: "accepted", Reason: "capacity available",
				})
				if c.delays > 0 {
					events = append(events, truth.Event{
						Frame: frame, PackageID: c.pkg.ID, DialogueID: c.pkg.DialogueID,
						ShardIndex: c.pkg.ShardIndex, Channel: c.pkg.Channel,
						Status: "retry", Reason: fmt.Sprintf("accepted after %d delay(s)", c.delays),
					})
				}
			} else {
				deadline := c.pkg.StartFrame + maxDelay
				if frame >= deadline {
					events = append(events, truth.Event{
						Frame: frame, PackageID: c.pkg.ID, DialogueID: c.pkg.DialogueID,
						ShardIndex: c.pkg.ShardIndex, Channel: c.pkg.Channel,
						Status: "rejected", Reason: reason,
					})
					continue
				}
				events = append(events, truth.Event{
					Frame: frame, PackageID: c.pkg.ID, DialogueID: c.pkg.DialogueID,
					ShardIndex: c.pkg.ShardIndex, Channel: c.pkg.Channel,
					Status: "delayed", Reason: fmt.Sprintf("frame %d blocked: %s", blocked, reason),
					RetryFrame: frame + 1,
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
	frameMap := make(map[int64][]string)
	for _, r := range reservations {
		for f := r.start; f < r.end; f++ {
			frameMap[f] = append(frameMap[f], r.pkg.ID)
		}
	}
	frames := make([]int64, 0, len(frameMap))
	for f := range frameMap {
		frames = append(frames, f)
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i] < frames[j] })
	var frameOut []truth.Frame
	for _, f := range frames {
		ids := frameMap[f]
		sort.Strings(ids)
		frameOut = append(frameOut, truth.Frame{Frame: f, PackageIDs: ids})
	}

	type disp struct {
		status string
		start  int64
		end    int64
	}
	byID := make(map[string]disp)
	for _, r := range reservations {
		byID[r.pkg.ID] = disp{status: "accepted", start: r.start, end: r.end}
	}
	for i := range pkgs {
		if d, ok := byID[pkgs[i].ID]; ok {
			pkgs[i].Status = d.status
			pkgs[i].DispatchStartFrame = d.start
			pkgs[i].DispatchEndFrame = d.end
		} else {
			pkgs[i].Status = "rejected"
		}
	}
	return pkgs, frameOut, events
}

func refCanPlace(frame, duration int64, pkg truth.Package,
	budgetByFrame map[int64]int, channelByFrame map[int64]map[int]int,
	budget, slots int) (bool, int64, string) {
	for f := frame; f < frame+duration; f++ {
		if budgetByFrame[f] >= budget {
			return false, f, fmt.Sprintf("global budget %d/%d", budgetByFrame[f], budget)
		}
		if channelByFrame[f] != nil && channelByFrame[f][pkg.Channel] >= slots {
			return false, f, fmt.Sprintf("channel %d slots %d/%d", pkg.Channel, channelByFrame[f][pkg.Channel], slots)
		}
	}
	return true, 0, ""
}
