package qos

import (
	"context"
	"fmt"
	"sort"
)

type Trace struct {
	Project   *Project
	Steps     []StepResult
	Snapshots []*EngineSnapshot
	Result    *RunResult
}

func ExecuteTrace(ctx context.Context, p *Project) (*Trace, error) {
	eng, err := NewEngine(p)
	if err != nil {
		return nil, err
	}
	trace := &Trace{Project: CloneProject(p)}
	for !eng.Done() {
		select {
		case <-ctx.Done():
			return nil, NewError(EContextCanceled, "trace", "execution canceled", nil)
		default:
		}
		st, err := eng.Step()
		if err != nil {
			return nil, err
		}
		st.SourceID = "initial"
		st.CacheToken = fmt.Sprintf("cache:initial:%d", st.Index)
		trace.Steps = append(trace.Steps, st)
		trace.Snapshots = append(trace.Snapshots, eng.Snapshot())
	}
	trace.Result = engineResult(eng, trace.Steps)
	return trace, nil
}

func engineResult(eng *Engine, steps []StepResult) *RunResult {
	final := make(map[string]int64, len(eng.channels))
	for _, cr := range eng.channels {
		final[cr.ch.ID] = cr.tokens
	}
	return &RunResult{
		Steps: steps, Emissions: cloneEmissions(eng.emitted),
		Delays: append([]DelayEvent(nil), eng.delayedEvents...),
		Drops:  cloneDrops(eng.dropped), Expired: append([]ExpiredEvent(nil), eng.expired...),
		TotalSteps: eng.nextStep, FinalTokens: final,
	}
}

type Edit struct {
	SourceID        string
	AtMS            int64
	ChannelID       string
	RatePermyriad   *int64
	CapacityBytes   *int64
	WeightPermyriad *int64
	AddPackets      []Packet
	RemovePacketIDs []string
}

type InvalidationReport struct {
	EditID              string   `json:"edit_id"`
	Cause               string   `json:"cause"`
	StartStep           int      `json:"start_step"`
	StartMS             int64    `json:"start_ms"`
	EndStep             int      `json:"end_step"`
	EndMS               int64    `json:"end_ms"`
	ReusedSteps         int      `json:"reused_steps"`
	RecomputedSteps     int      `json:"recomputed_steps"`
	CachedUntilStep     int      `json:"cached_until_step"`
	RootChannels        []string `json:"root_channels"`
	ClosureChannels     []string `json:"closure_channels"`
	ClosureEdges        []string `json:"closure_edges"`
	ChangedSteps        []int    `json:"changed_steps"`
	RecomputedUnchanged []int    `json:"recomputed_unchanged_steps"`
}

type Incremental struct {
	Trace *Trace
	edits int
}

func NewIncremental(ctx context.Context, p *Project) (*Incremental, error) {
	t, err := ExecuteTrace(ctx, p)
	if err != nil {
		return nil, err
	}
	return &Incremental{Trace: t}, nil
}

func (inc *Incremental) Apply(ctx context.Context, edit Edit) (*Trace, *InvalidationReport, error) {
	if inc == nil || inc.Trace == nil {
		return nil, nil, NewError(EInternal, "incremental", "incremental engine is not initialized", nil)
	}
	inc.edits++
	if edit.SourceID == "" {
		edit.SourceID = fmt.Sprintf("edit-%d", inc.edits)
	}
	oldProject := inc.Trace.Project
	newProject := CloneProject(oldProject)
	startMS, roots, cause, err := applyEdit(newProject, oldProject, edit)
	if err != nil {
		return nil, nil, err
	}
	if startMS%newProject.TickMS != 0 {
		return nil, nil, NewError(ETimeAlignment, "edit.at_ms", "edit time must align to a logical tick",
			map[string]any{"at_ms": startMS, "tick_ms": newProject.TickMS})
	}
	startStep := int(startMS / newProject.TickMS)
	if startStep > len(inc.Trace.Steps) {
		return nil, nil, NewError(EEditTarget, "edit.at_ms", "edit starts after the cached trace", map[string]any{"start_step": startStep, "cached_steps": len(inc.Trace.Steps)})
	}
	if err := ValidateProject(newProject); err != nil {
		return nil, nil, err
	}
	var eng *Engine
	if startStep == 0 {
		eng, err = NewEngine(newProject)
	} else {
		eng, err = RestoreSnapshot(newProject, inc.Trace.Snapshots[startStep-1])
	}
	if err != nil {
		return nil, nil, err
	}
	newSteps := make([]StepResult, 0)
	newSnapshots := make([]*EngineSnapshot, 0)
	for !eng.Done() {
		select {
		case <-ctx.Done():
			return nil, nil, NewError(EContextCanceled, "incremental", "execution canceled", nil)
		default:
		}
		st, err := eng.Step()
		if err != nil {
			return nil, nil, err
		}
		st.SourceID = edit.SourceID
		st.CacheToken = fmt.Sprintf("cache:%s:%d", edit.SourceID, st.Index)
		newSteps = append(newSteps, st)
		newSnapshots = append(newSnapshots, eng.Snapshot())
	}
	steps := append([]StepResult(nil), inc.Trace.Steps[:startStep]...)
	for i := range steps {
		steps[i].Reused = true
	}
	steps = append(steps, newSteps...)
	snaps := append([]*EngineSnapshot(nil), inc.Trace.Snapshots[:startStep]...)
	snaps = append(snaps, newSnapshots...)
	trace := &Trace{Project: CloneProject(newProject), Steps: steps, Snapshots: snaps}
	trace.Result = engineResult(eng, steps)

	endStep := len(steps) - 1
	var changed, unchanged []int
	for i := startStep; i < len(steps); i++ {
		if i < len(inc.Trace.Steps) && TruthStepDigest(cleanCacheFields(steps[i])) == TruthStepDigest(cleanCacheFields(inc.Trace.Steps[i])) {
			unchanged = append(unchanged, i)
		} else {
			changed = append(changed, i)
		}
	}
	report := &InvalidationReport{
		EditID: edit.SourceID, Cause: cause, StartStep: startStep, StartMS: startMS,
		EndStep: endStep, EndMS: int64(endStep) * newProject.TickMS,
		ReusedSteps: startStep, RecomputedSteps: len(newSteps),
		CachedUntilStep: startStep - 1, RootChannels: roots,
		ClosureChannels: newProject.ChannelOrder(), ChangedSteps: changed,
		RecomputedUnchanged: unchanged,
	}
	if startStep == 0 {
		report.CachedUntilStep = -1
	}
	for _, root := range roots {
		report.ClosureEdges = append(report.ClosureEdges,
			fmt.Sprintf("%s -> tokens:%s@%dms -> delay_queue:%s@%dms..%dms -> scheduler:egress@%dms..%dms -> channels:*",
				cause, root, startMS, root, startMS, report.EndMS, startMS, report.EndMS))
	}
	if len(roots) == 0 {
		report.ClosureEdges = append(report.ClosureEdges,
			fmt.Sprintf("%s -> scheduler:egress@%dms..%dms -> channels:*", cause, startMS, report.EndMS))
	}
	inc.Trace = trace
	return trace, report, nil
}

func cleanCacheFields(st StepResult) StepResult {
	st.SourceID = ""
	st.Reused = false
	st.CacheToken = ""
	return st
}

func applyEdit(newProject, oldProject *Project, edit Edit) (int64, []string, string, error) {
	startMS := int64(-1)
	var roots []string
	var causes []string
	touch := func(ms int64, root, cause string) error {
		if ms < 0 {
			return NewError(ETimeNegative, "edit.at_ms", "edit time must not be negative", map[string]any{"at_ms": ms})
		}
		if startMS < 0 || ms < startMS {
			startMS = ms
		}
		if root != "" {
			roots = append(roots, root)
		}
		causes = append(causes, cause)
		return nil
	}
	if edit.ChannelID != "" {
		ch, idx := newProject.Channel(edit.ChannelID)
		if ch == nil {
			return 0, nil, "", NewError(EEditTarget, "edit.channel_id", "unknown channel", map[string]any{"channel": edit.ChannelID})
		}
		if edit.RatePermyriad != nil {
			if *edit.RatePermyriad < 0 {
				return 0, nil, "", NewError(ERateNegative, "edit.rate", "rate must not be negative", map[string]any{"channel": edit.ChannelID, "rate_permyriad": *edit.RatePermyriad})
			}
			newProject.Channels[idx].RatePermyriad = *edit.RatePermyriad
			if err := touch(edit.AtMS, edit.ChannelID, fmt.Sprintf("rate:%s@%dms", edit.ChannelID, edit.AtMS)); err != nil {
				return 0, nil, "", err
			}
		}
		if edit.CapacityBytes != nil {
			if *edit.CapacityBytes <= 0 {
				return 0, nil, "", NewError(ECapacityNegative, "edit.capacity", "capacity must be positive", map[string]any{"channel": edit.ChannelID, "capacity_bytes": *edit.CapacityBytes})
			}
			newProject.Channels[idx].CapacityBytes = *edit.CapacityBytes
			if err := touch(edit.AtMS, edit.ChannelID, fmt.Sprintf("capacity:%s@%dms", edit.ChannelID, edit.AtMS)); err != nil {
				return 0, nil, "", err
			}
		}
		if edit.WeightPermyriad != nil {
			if *edit.WeightPermyriad <= 0 {
				return 0, nil, "", NewError(EWeightRange, "edit.weight", "weight must be positive", map[string]any{"channel": edit.ChannelID, "weight_permyriad": *edit.WeightPermyriad})
			}
			newProject.Channels[idx].WeightPermyriad = *edit.WeightPermyriad
			if err := touch(edit.AtMS, edit.ChannelID, fmt.Sprintf("weight:%s@%dms", edit.ChannelID, edit.AtMS)); err != nil {
				return 0, nil, "", err
			}
		}
	}
	removed := make(map[string]bool, len(edit.RemovePacketIDs))
	for _, id := range edit.RemovePacketIDs {
		if removed[id] {
			return 0, nil, "", NewError(EEditTarget, "edit.remove", "duplicate removal id", map[string]any{"packet": id})
		}
		found := false
		for i := range newProject.Packets {
			if newProject.Packets[i].ID == id {
				found = true
				if err := touch(newProject.Packets[i].ArrivalMS, newProject.Packets[i].ChannelID, "remove:"+id); err != nil {
					return 0, nil, "", err
				}
				newProject.Packets = append(newProject.Packets[:i], newProject.Packets[i+1:]...)
				break
			}
		}
		if !found {
			return 0, nil, "", NewError(EEditTarget, "edit.remove", "packet not found", map[string]any{"packet": id})
		}
		removed[id] = true
	}
	nextSeq := 0
	for _, pkt := range newProject.Packets {
		if pkt.Seq >= nextSeq {
			nextSeq = pkt.Seq + 1
		}
	}
	for i := range edit.AddPackets {
		pkt := edit.AddPackets[i]
		if pkt.ID == "" {
			return 0, nil, "", NewError(EFieldMissing, "edit.add.id", "packet id is required", nil)
		}
		for j := range newProject.Packets {
			if newProject.Packets[j].ID == pkt.ID {
				return 0, nil, "", NewError(EDuplicatePacket, "edit.add.id", "packet already exists", map[string]any{"packet": pkt.ID})
			}
		}
		if pkt.Seq < 0 {
			pkt.Seq = nextSeq
			nextSeq++
		}
		if pkt.DSCP != nil {
			d := *pkt.DSCP
			pkt.DSCP = &d
		}
		newProject.Packets = append(newProject.Packets, pkt)
		if err := touch(pkt.ArrivalMS, pkt.ChannelID, "add:"+pkt.ID); err != nil {
			return 0, nil, "", err
		}
	}
	if startMS < 0 {
		return 0, nil, "", NewError(EEditTarget, "edit", "edit does not change any project field", nil)
	}
	if len(causes) == 0 {
		causes = []string{"edit"}
	}
	sort.Strings(roots)
	roots = uniqueStrings(roots)
	return startMS, roots, joinCauses(causes), nil
}

func uniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func joinCauses(causes []string) string {
	sort.Strings(causes)
	out := ""
	for i, c := range causes {
		if i > 0 {
			out += "+"
		}
		out += c
	}
	return out
}
