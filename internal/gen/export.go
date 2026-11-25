package gen

import (
	"sort"

	"replayaudit/internal/formats"
	"replayaudit/internal/model"
	"replayaudit/internal/timeconv"
)

func i64ptr(v int64) *int64 { return &v }
func intptr(v int) *int     { return &v }
func boolptr(v bool) *bool  { return &v }

// ToFormats projects a normalized replay into the three wire formats.
func ToFormats(r *model.Replay, p model.Params) (formats.Format1, formats.Format2, formats.Format3) {
	f1 := formats.Format1{
		Format: 1, ReplayID: r.ReplayID, MsPerTurn: p.MsPerTurn,
		Entities: append([]model.EntitySpec(nil), r.Entities...),
	}
	f2 := formats.Format2{Format: 2, ReplayID: r.ReplayID, Entities: append([]model.EntitySpec(nil), r.Entities...)}
	f3 := formats.Format3{Format: 3, ReplayID: r.ReplayID, Entities: append([]model.EntitySpec(nil), r.Entities...)}

	for _, f := range r.Flags {
		dto := formats.FlagDefDTO{ID: f.ID, Name: f.Name, Value: f.Value, Priority: f.Priority}
		f1.FlagDefs = append(f1.FlagDefs, dto)
		f3.FlagDefs = append(f3.FlagDefs, dto)
	}
	for _, m := range r.Mutex {
		f1.MutexGroups = append(f1.MutexGroups, formats.MutexDTO{Group: m.ID, EventIDs: append([]string(nil), m.EventIDs...)})
	}
	syncTimes := model.SortedSyncTimes(r.Sync)
	f2.SyncPoints = append([]int64(nil), syncTimes...)
	f3.SyncPoints = append([]int64(nil), syncTimes...)

	events := append([]*model.Event(nil), r.Events...)
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })

	for _, e := range events {
		turn, _ := timeconv.MSToTurn(e.At, p.MsPerTurn, p.Round)
		one := formats.F1Event{
			ID: e.ID, At: i64ptr(turn), Kind: e.Kind,
			Chain:   append([]string(nil), e.Casters...), // casters semantic
			Targets: append([]string(nil), e.Targets...),
			Amount:  i64ptr(e.Amount), Skill: e.Skill,
			Priority: intptr(e.Priority), Mandatory: e.Mandatory, Fault: e.Fault,
		}
		if e.CooldownUnits > 0 {
			one.CooldownMS = i64ptr(e.CooldownUnits)
		}
		if e.Cond != nil {
			one.CondFlag = e.Cond.Flag
			one.CondWant = boolptr(e.Cond.Want)
		}
		f1.Events = append(f1.Events, one)

		two := formats.F2Event{
			ID: e.ID, At: i64ptr(e.At), Kind: e.Kind,
			Chain:   append([]string(nil), e.Targets...), // TARGETS semantic swap
			Casters: append([]string(nil), e.Casters...),
			Amount:  i64ptr(e.Amount), Skill: e.Skill,
			Priority: intptr(e.Priority), Mandatory: e.Mandatory, Fault: e.Fault,
		}
		if e.CooldownUnits > 0 {
			two.CooldownMS = i64ptr(e.CooldownUnits)
		}
		f2.Events = append(f2.Events, two)

		three := formats.F3Event{
			ID: e.ID, AtMS: i64ptr(e.At), Kind: e.Kind,
			CasterSeq: append([]string(nil), e.Casters...),
			TargetSeq: append([]string(nil), e.Targets...),
			Amount:    i64ptr(e.Amount), Skill: e.Skill,
			Priority: intptr(e.Priority), Mandatory: e.Mandatory, Fault: e.Fault,
		}
		if e.CooldownUnits > 0 {
			three.CooldownMS = i64ptr(e.CooldownUnits)
		}
		if e.Cond != nil {
			three.CondFlag = e.Cond.Flag
			three.CondWant = boolptr(e.Cond.Want)
		}
		f3.Events = append(f3.Events, three)
	}
	return f1, f2, f3
}
