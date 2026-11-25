package scenario

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"lipsync/domain"
	"lipsync/formats"
)

type Artifacts struct {
	Seed        int64                          `json:"seed"`
	Canonical   domain.Project                 `json:"canonical"`
	Documents   map[string][]byte              `json:"-"`
	LossReports map[string]*formats.LossReport `json:"loss_reports"`
	Truth       domain.ArbitrationResult       `json:"truth"`
}

// Canonical constructs a designed multi-voice contention scenario entirely in
// integer frame/basis-point units. The seed only perturbs deterministic symbols
// and packet costs in a bounded, replayable way.
func Canonical(seed int64) domain.Project {
	offset := int64(seed%5) * 10
	frameSamples := int64(960)
	dialogue := func(id, channelID string, priority int, start, duration int64, symbol string, cost int64) domain.Dialogue {
		d := domain.Dialogue{
			ID: id, ChannelID: channelID, VoiceID: "voice-" + id, Priority: priority,
			StartFrame: start,
			PCM: domain.PCMDescription{
				SampleRate: 48000, Samples: duration * frameSamples,
				DurationMs: duration * 20, Channels: 1,
			},
			Phonemes: []domain.Phoneme{{
				ID: "pkt-" + id, Symbol: symbol, StartFrame: 0, DurationFrames: duration,
				StartSample: 0, DurationSamples: duration * frameSamples,
				StartMs: 0, DurationMs: duration * 20, CostBasisPoints: cost,
			}},
		}
		d.ContentHash = domain.ComputeDialogueHash(d)
		return d
	}
	project := domain.Project{
		SchemaVersion:     "canonical",
		Seed:              seed,
		Name:              fmt.Sprintf("multi-voice-arbitration-%d", seed),
		SampleRate:        48000,
		FrameRate:         50,
		SourceTimeBase:    domain.TimeBaseSamples,
		SourceBudgetScale: domain.BudgetScaleBasisPoint,
		Rounding:          "floor",
		Channels: []domain.Channel{
			{ID: "spk_main", Scene: "booth", SlotLimit: 2, FrameBudgetBasisPoints: 6000},
			{ID: "spk_alt", Scene: "booth", SlotLimit: 1, FrameBudgetBasisPoints: 4000},
			{ID: "spk_wide", Scene: "stage", SlotLimit: 2, FrameBudgetBasisPoints: 9000},
		},
		ArbitrationOrder: []string{"spk_wide", "spk_main", "spk_alt"},
		Recovery: domain.RecoveryPolicy{
			Strategy:            domain.StrategyNextFrame,
			RecoveryOffsetFrame: 1,
			MinContinuousFrames: 2,
			AllowAbandon:        true,
			WindowFrames:        4,
		},
		Dialogues: []domain.Dialogue{
			dialogue("d1-main-low", "spk_main", 10, 0, 5, "a", 3200+offset),
			dialogue("d2-main-mid", "spk_main", 30, 2, 3, "i", 3200+offset),
			dialogue("d3-main-high", "spk_main", 40, 3, 2, "u", 3300+offset),
			dialogue("d4-main-mid", "spk_main", 20, 4, 3, "e", 3200+offset),
			dialogue("d5-main-top", "spk_main", 30, 7, 2, "o", 3300+offset),
			dialogue("d6-alt", "spk_alt", 15, 1, 3, "s", 3000),
			dialogue("d7-wide-a", "spk_wide", 12, 2, 2, "m", 4000),
			dialogue("d8-wide-b", "spk_wide", 18, 3, 3, "n", 4100),
		},
	}
	project.ContentHash = domain.ComputeContentHash(project)
	return project
}

// Generate emits all three source documents plus the truth computed by the
// generator-local reference construction. It never calls arbitration.Run.
func Generate(seed int64) (*Artifacts, error) {
	project := Canonical(seed)
	artifacts := &Artifacts{
		Seed: seed, Canonical: project,
		Documents: map[string][]byte{}, LossReports: map[string]*formats.LossReport{},
	}
	for _, version := range domain.SupportedVersions {
		data, report, err := formats.Export(project, version)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", version, err)
		}
		artifacts.Documents[version] = data
		artifacts.LossReports[version] = report
	}
	truth, err := referenceArbitration(project)
	if err != nil {
		return nil, fmt.Errorf("generator truth: %w", err)
	}
	artifacts.Truth = truth
	return artifacts, nil
}

// LoadAll verifies that every generated document can be read back.
func (a *Artifacts) LoadAll() (map[string]*domain.Project, error) {
	projects := map[string]*domain.Project{}
	for _, version := range domain.SupportedVersions {
		project, err := formats.Load(a.Documents[version])
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", version, err)
		}
		projects[version] = project
	}
	return projects, nil
}

// WriteArtifacts materializes the deterministic source documents, truth and
// loss reports for inspection or external pipeline integration.
func (a *Artifacts) WriteArtifacts(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, version := range domain.SupportedVersions {
		if err := os.WriteFile(filepath.Join(dir, "project."+version+".json"), a.Documents[version], 0o644); err != nil {
			return err
		}
	}
	truth, err := json.MarshalIndent(a.Truth, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), append(truth, '\n'), 0o644); err != nil {
		return err
	}
	losses, err := json.MarshalIndent(a.LossReports, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "loss-reports.json"), append(losses, '\n'), 0o644)
}

const (
	refPending   = "pending"
	refActive    = "active"
	refPreempted = "preempted"
	refDone      = "done"
	refAbandoned = "abandoned"
)

type refPacket struct {
	pkt            domain.Packet
	status         string
	delivered      int64
	remaining      int64
	eligible       int64
	preemptedAt    int64
	recoveryFrame  int64
	protectedUntil int64
	lastDelivery   int64
	abandonedAt    int64
	abandonReason  string
}

// referenceArbitration is the generator's independent analytic construction.
// It intentionally does not import the production arbitration package.
func referenceArbitration(project domain.Project) (domain.ArbitrationResult, error) {
	packets := project.Packets()
	states := make(map[string]*refPacket, len(packets))
	byChannel := map[string][]*refPacket{}
	for _, packet := range packets {
		state := &refPacket{
			pkt: packet, status: refPending,
			remaining: packet.DurationFrames, eligible: packet.StartFrame,
		}
		states[packet.ID] = state
		byChannel[packet.ChannelID] = append(byChannel[packet.ChannelID], state)
	}
	for _, list := range byChannel {
		sort.Slice(list, func(i, j int) bool {
			if list[i].pkt.StartFrame != list[j].pkt.StartFrame {
				return list[i].pkt.StartFrame < list[j].pkt.StartFrame
			}
			if list[i].pkt.Priority != list[j].pkt.Priority {
				return list[i].pkt.Priority > list[j].pkt.Priority
			}
			return list[i].pkt.ID < list[j].pkt.ID
		})
	}
	channels := map[string]domain.Channel{}
	for _, channel := range project.Channels {
		channels[channel.ID] = channel
	}
	result := domain.ArbitrationResult{
		Frames: []domain.FrameResult{}, Preemptions: []domain.PreemptEvent{},
		Resumes: []domain.ResumeEvent{}, Abandonments: []domain.AbandonEvent{},
	}
	resolved := func() bool {
		for _, state := range states {
			if state.status != refDone && state.status != refAbandoned {
				return false
			}
		}
		return true
	}
	maxFrame := int64(0)
	for _, state := range states {
		if state.pkt.DeadlineFrame > maxFrame {
			maxFrame = state.pkt.DeadlineFrame
		}
	}
	for frame := int64(0); !resolved() && frame <= maxFrame+1; frame++ {
		frameResult := domain.FrameResult{Frame: frame, Channels: []domain.FrameChannelResult{}}
		for _, channelID := range project.ArbitrationOrder {
			channel := channels[channelID]
			channelResult := domain.FrameChannelResult{
				ChannelID: channelID, Accepted: []string{}, Delayed: []string{},
				Preempted: []string{}, Resumed: []string{},
			}
			candidates := []*refPacket{}
			booked := []*refPacket{}
			bookedSet := map[string]bool{}
			used := int64(0)
			for _, state := range byChannel[channelID] {
				switch state.status {
				case refActive:
					candidates = append(candidates, state)
					booked = append(booked, state)
					bookedSet[state.pkt.ID] = true
					used += state.pkt.CostBasisPoints
				case refPending, refPreempted:
					if state.eligible > frame {
						continue
					}
					available := state.pkt.DeadlineFrame - frame + 1
					required := state.remaining
					if state.status == refPreempted && int64(project.Recovery.MinContinuousFrames) < required {
						required = int64(project.Recovery.MinContinuousFrames)
					}
					if available < state.remaining || available < required {
						if !project.Recovery.AllowAbandon {
							return result, &domain.Error{
								Code: domain.CodeResume, Path: "packet." + state.pkt.ID,
								ChannelID: channelID, PacketID: state.pkt.ID,
								Available: available, Required: required,
								Message: "reference construction cannot meet recovery window",
							}
						}
						state.status = refAbandoned
						state.abandonedAt = frame
						state.abandonReason = "window_exhausted"
						result.Abandonments = append(result.Abandonments, domain.AbandonEvent{
							Frame: frame, ChannelID: channelID, PacketID: state.pkt.ID,
							Reason: state.abandonReason, AvailableFrames: max64(0, available),
							RequiredFrames: state.remaining,
						})
						continue
					}
					candidates = append(candidates, state)
				}
			}
			sort.Slice(candidates, func(i, j int) bool {
				if candidates[i].pkt.Priority != candidates[j].pkt.Priority {
					return candidates[i].pkt.Priority > candidates[j].pkt.Priority
				}
				iActive, jActive := candidates[i].status == refActive, candidates[j].status == refActive
				if iActive != jActive {
					return iActive
				}
				if candidates[i].pkt.StartFrame != candidates[j].pkt.StartFrame {
					return candidates[i].pkt.StartFrame < candidates[j].pkt.StartFrame
				}
				return candidates[i].pkt.ID < candidates[j].pkt.ID
			})
			blocked := map[string]bool{}
			fits := func(active []*refPacket, candidate *refPacket, budget int64) bool {
				return len(active)+1 <= channel.SlotLimit &&
					budget+candidate.pkt.CostBasisPoints <= channel.FrameBudgetBasisPoints
			}
			for _, candidate := range candidates {
				if blocked[candidate.pkt.ID] || bookedSet[candidate.pkt.ID] {
					continue
				}
				if candidate.status != refPending && candidate.status != refPreempted {
					continue
				}
				if fits(booked, candidate, used) {
					acceptRef(candidate, frame, project.Recovery.MinContinuousFrames, &result)
					booked = append(booked, candidate)
					bookedSet[candidate.pkt.ID] = true
					used += candidate.pkt.CostBasisPoints
					continue
				}
				var victims []*refPacket
				for _, active := range booked {
					if active.pkt.Priority >= candidate.pkt.Priority {
						continue
					}
					if active.protectedUntil > 0 && active.protectedUntil >= frame {
						continue
					}
					victims = append(victims, active)
				}
				sort.Slice(victims, func(i, j int) bool {
					if victims[i].pkt.Priority != victims[j].pkt.Priority {
						return victims[i].pkt.Priority < victims[j].pkt.Priority
					}
					if victims[i].pkt.StartFrame != victims[j].pkt.StartFrame {
						return victims[i].pkt.StartFrame > victims[j].pkt.StartFrame
					}
					return victims[i].pkt.ID < victims[j].pkt.ID
				})
				removed := map[string]bool{}
				for _, victim := range victims {
					if fits(withoutRef(booked, removed), candidate, used-removedCostRef(booked, removed)) {
						break
					}
					victim.status = refPreempted
					victim.preemptedAt = frame
					victim.recoveryFrame = recoveryFrameRef(project.Recovery, frame)
					available := victim.pkt.DeadlineFrame - victim.recoveryFrame + 1
					required := victim.remaining
					if int64(project.Recovery.MinContinuousFrames) < required {
						required = int64(project.Recovery.MinContinuousFrames)
					}
					event := domain.PreemptEvent{
						Frame: frame, ChannelID: channelID, PacketID: victim.pkt.ID,
						ByPacketID: candidate.pkt.ID, DeliveredFrames: victim.delivered,
						RemainingFrames: victim.remaining, RecoveryFrame: victim.recoveryFrame,
					}
					if available < victim.remaining || available < required {
						if !project.Recovery.AllowAbandon {
							return result, &domain.Error{
								Code: domain.CodeResume, Path: "packet." + victim.pkt.ID,
								ChannelID: channelID, PacketID: victim.pkt.ID,
								Available: available, Required: required,
								Message: "reference construction recovery window too short",
							}
						}
						victim.status = refAbandoned
						victim.abandonedAt = frame
						victim.abandonReason = "insufficient_recovery_window"
						event.RecoveryFrame = 0
						result.Abandonments = append(result.Abandonments, domain.AbandonEvent{
							Frame: frame, ChannelID: channelID, PacketID: victim.pkt.ID,
							Reason: victim.abandonReason, AvailableFrames: max64(0, available),
							RequiredFrames: required,
						})
					} else {
						victim.eligible = victim.recoveryFrame
					}
					removed[victim.pkt.ID] = true
					blocked[victim.pkt.ID] = true
					used -= victim.pkt.CostBasisPoints
					result.Preemptions = append(result.Preemptions, event)
					channelResult.Preempted = append(channelResult.Preempted, victim.pkt.ID)
				}
				booked = withoutRef(booked, removed)
				bookedSet = mapRef(booked)
				if fits(booked, candidate, used) {
					acceptRef(candidate, frame, project.Recovery.MinContinuousFrames, &result)
					booked = append(booked, candidate)
					bookedSet[candidate.pkt.ID] = true
					used += candidate.pkt.CostBasisPoints
				} else {
					channelResult.Delayed = append(channelResult.Delayed, candidate.pkt.ID)
				}
			}
			sort.Strings(channelResult.Preempted)
			sort.Strings(channelResult.Delayed)
			for _, state := range booked {
				state.delivered++
				state.remaining--
				state.lastDelivery = frame
				if state.remaining == 0 {
					state.status = refDone
				}
				channelResult.Accepted = append(channelResult.Accepted, state.pkt.ID)
			}
			sort.Strings(channelResult.Accepted)
			for _, resume := range result.Resumes {
				if resume.Frame == frame && resume.ChannelID == channelID {
					channelResult.Resumed = append(channelResult.Resumed, resume.PacketID)
				}
			}
			sort.Strings(channelResult.Resumed)
			channelResult.OccupiedSlot = len(booked)
			channelResult.UsedBudget = used
			frameResult.Channels = append(frameResult.Channels, channelResult)
		}
		result.Frames = append(result.Frames, frameResult)
	}
	ids := make([]string, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		state := states[id]
		result.PacketStates = append(result.PacketStates, domain.PacketState{
			ID: state.pkt.ID, ChannelID: state.pkt.ChannelID, Status: state.status,
			Delivered: state.delivered, Remaining: state.remaining, EligibleFrame: state.eligible,
			PreemptedAt: state.preemptedAt, RecoveryFrame: state.recoveryFrame,
			ProtectedUntil: state.protectedUntil, LastDelivery: state.lastDelivery,
			AbandonedAt: state.abandonedAt, AbandonReason: state.abandonReason,
		})
	}
	return result, nil
}

func acceptRef(packet *refPacket, frame int64, minRun int, result *domain.ArbitrationResult) {
	wasPreempted := packet.status == refPreempted
	packet.status = refActive
	if wasPreempted {
		run := minRun
		if int64(run) > packet.remaining {
			run = int(packet.remaining)
		}
		packet.protectedUntil = frame + int64(run) - 1
		result.Resumes = append(result.Resumes, domain.ResumeEvent{
			Frame: frame, ChannelID: packet.pkt.ChannelID, PacketID: packet.pkt.ID,
			ProtectedUntil: packet.protectedUntil,
		})
	}
}

func recoveryFrameRef(policy domain.RecoveryPolicy, preemptFrame int64) int64 {
	base := preemptFrame + 1
	if policy.RecoveryOffsetFrame > 0 && preemptFrame+policy.RecoveryOffsetFrame > base {
		base = preemptFrame + policy.RecoveryOffsetFrame
	}
	if policy.Strategy == domain.StrategyNextWindow {
		remainder := base % policy.WindowFrames
		if remainder != 0 {
			base += policy.WindowFrames - remainder
		}
	}
	return base
}

func withoutRef(in []*refPacket, removed map[string]bool) []*refPacket {
	out := make([]*refPacket, 0, len(in))
	for _, packet := range in {
		if !removed[packet.pkt.ID] {
			out = append(out, packet)
		}
	}
	return out
}

func mapRef(in []*refPacket) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, packet := range in {
		out[packet.pkt.ID] = true
	}
	return out
}

func removedCostRef(in []*refPacket, removed map[string]bool) int64 {
	var total int64
	for _, packet := range in {
		if removed[packet.pkt.ID] {
			total += packet.pkt.CostBasisPoints
		}
	}
	return total
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
