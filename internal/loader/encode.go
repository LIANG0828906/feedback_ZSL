package loader

import (
	"encoding/json"

	"battlekernel/internal/errs"
	"battlekernel/internal/model"
)

func Encode(p *model.Project, formatNo int) ([]byte, error) {
	switch formatNo {
	case 1:
		return json.MarshalIndent(toW1(p), "", "  ")
	case 2:
		return json.MarshalIndent(toW2(p), "", "  ")
	case 3:
		return json.MarshalIndent(toW3(p), "", "  ")
	default:
		return nil, schemaFormatError(formatNo)
	}
}

func schemaFormatError(formatNo int) error {
	return errs.New(errs.ESchema, "format", "unknown format number: %d", formatNo)
}

func toW1(p *model.Project) Wire1 {
	w := Wire1{
		Format: 1, FormatName: FormatTickV1, Schema: "battle.tick.v1",
		BattleID: p.ID, Seed: p.Seed, TickMS: p.TickMs, EpochMS: p.EpochMs,
		StartRound: p.StartRound, Rounds: p.TotalRounds, EnemyHP: p.EnemyHP,
		Lists:    W1Lists{Alpha: refs(p.SkillSequence), Beta: refs(p.ActorSequence)},
		Reorders: toWReorders(p.Reorders),
	}
	for _, u := range p.Units {
		w.Units = append(w.Units, W1Unit{ID: u.ID, Name: u.Name, Priority: u.BaseWeight, Recast: u.InitialCooldown, Slot: u.TieOrdinal, Energy: u.InitialResource, EnergyMax: u.MaxResource, Regen: u.Regen})
	}
	for _, s := range p.Skills {
		w.Skills = append(w.Skills, W1Skill{ID: s.ID, Actor: s.UnitID, Cost: s.Cost, CastCD: s.Cooldown, Power: s.Power, Conditional: s.Conditional})
	}
	for _, r := range p.Rounds {
		wr := W1Round{Round: r.Round}
		for _, a := range r.Actions {
			wa := W1Action{Actor: a.UnitID, Technique: a.SkillID}
			if a.Flag != nil {
				v := 0
				if *a.Flag {
					v = 1
				}
				wa.Flag = &v
			}
			wr.Actions = append(wr.Actions, wa)
		}
		w.Timeline = append(w.Timeline, wr)
	}
	return w
}

func toW2(p *model.Project) Wire2 {
	w := Wire2{
		FormatNo: 2, FormatName: FormatChronoV2, Schema: "battle.chrono.v2",
		BattleID: p.ID, Seed: p.Seed, TickMilliseconds: p.TickMs, EpochMilliseconds: p.EpochMs,
		StartRound: p.StartRound, TotalRounds: p.TotalRounds, EnemyHP: p.EnemyHP,
		SequenceBox:     W2Sequences{Alpha: refs(p.ActorSequence), Beta: refs(p.SkillSequence)},
		ReorderCommands: toWReorders(p.Reorders),
	}
	for _, u := range p.Units {
		w.UnitRecords = append(w.UnitRecords, W2Unit{UnitID: u.ID, DisplayName: u.Name, Priority: u.InitialCooldown, Recast: u.BaseWeight, SlotIndex: u.TieOrdinal, Energy: u.InitialResource, EnergyCap: u.MaxResource, Regen: u.Regen})
	}
	for _, s := range p.Skills {
		w.SkillRecords = append(w.SkillRecords, W2Skill{SkillID: s.ID, OwnerID: s.UnitID, EnergyCost: s.Cost, Cooldown: s.Cooldown, Potency: s.Power, Conditional: s.Conditional})
	}
	for _, r := range p.Rounds {
		wp := W2TimePoint{AtMilliseconds: r.TimestampMs}
		for _, a := range r.Actions {
			wp.Commands = append(wp.Commands, W2Action{ActorID: a.UnitID, TechniqueID: a.SkillID, ConditionFlag: a.Flag})
		}
		w.TimePoints = append(w.TimePoints, wp)
	}
	return w
}

func toW3(p *model.Project) Wire3 {
	w := Wire3{
		Fmt: 3, Kind: FormatCompactV3, Doc: "battle.compact.v3",
		ID: p.ID, Seed: p.Seed, Tick: p.TickMs, Epoch: p.EpochMs,
		BaseRound: p.StartRound, Rounds: p.TotalRounds, Enemy: p.EnemyHP,
		Lists:  W3Lists{Skill: refs(p.SkillSequence), Actor: refs(p.ActorSequence)},
		Orders: toWReorders(p.Reorders),
	}
	for _, u := range p.Units {
		w.Actors = append(w.Actors, W3Actor{ID: u.ID, Name: u.Name, W: u.BaseWeight, CD: u.InitialCooldown, Ord: u.TieOrdinal, Res: u.InitialResource, Max: u.MaxResource, Regen: u.Regen})
	}
	for _, s := range p.Skills {
		w.Techs = append(w.Techs, W3Tech{ID: s.ID, Owner: s.UnitID, Cost: s.Cost, CD: s.Cooldown, Pow: s.Power, Cond: s.Conditional})
	}
	for _, r := range p.Rounds {
		wf := W3Frame{Round: r.Round}
		for _, a := range r.Actions {
			flag := ""
			if a.Flag != nil {
				if *a.Flag {
					flag = "Y"
				} else {
					flag = "N"
				}
			}
			wf.Moves = append(wf.Moves, W3Move{Actor: a.UnitID, Tech: a.SkillID, Flag: flag})
		}
		w.Frames = append(w.Frames, wf)
	}
	return w
}

func refs(items []model.SequenceItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Ref)
	}
	return out
}

func toWReorders(in []model.ReorderInstruction) []WReorder {
	out := make([]WReorder, 0, len(in))
	for _, r := range in {
		out = append(out, WReorder{Offset: r.Offset, InstructionID: r.ID, Actor: r.UnitID, AfterRound: r.AfterRound, NewSlot: r.NewTieOrdinal, Reason: r.Reason})
	}
	return out
}
