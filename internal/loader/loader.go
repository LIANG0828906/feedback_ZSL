package loader

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"battlekernel/internal/canonical"
	"battlekernel/internal/convert"
	"battlekernel/internal/errs"
	"battlekernel/internal/model"
)

const MaxCooldownRemaining = 99

type DocumentSet struct {
	V1    *model.Project
	V2    *model.Project
	V3    *model.Project
	Truth *model.Truth
}

func LoadDocument(data []byte) (*model.Project, error) {
	var envelope Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		if typeErr, ok := err.(*json.UnmarshalTypeError); ok {
			field := typeErr.Field
			if field == "" {
				field = "format"
			}
			return nil, errs.Wrap(errs.ESchema, field, err, "format field has invalid type")
		}
		return nil, errs.Wrap(errs.EDecode, "$", err, "document is not valid JSON")
	}
	if envelope.Format == 0 {
		var probe map[string]any
		_ = json.Unmarshal(data, &probe)
		if _, ok := probe["fmt"]; ok {
			var w3 Wire3
			return loadW3(data, &w3)
		}
		if _, ok := probe["formatNo"]; ok {
			var w2 Wire2
			return loadW2(data, &w2)
		}
		if _, ok := probe["format"]; ok {
			return nil, errs.New(errs.ESchema, "format", "format number is zero")
		}
		return nil, errs.New(errs.ESchema, "format", "unknown or missing format number")
	}
	switch envelope.Format {
	case 1:
		var w Wire1
		return loadW1(data, &w)
	case 2:
		var w Wire2
		return loadW2(data, &w)
	case 3:
		var w Wire3
		return loadW3(data, &w)
	default:
		return nil, errs.New(errs.ESchema, "format", "unknown format number: %d", envelope.Format)
	}
}

func LoadTruth(data []byte) (*model.Truth, error) {
	var truth model.Truth
	if err := json.Unmarshal(data, &truth); err != nil {
		if _, ok := err.(*json.UnmarshalTypeError); ok {
			return nil, errs.Wrap(errs.ESchema, "truth", err, "truth field has invalid type")
		}
		return nil, errs.Wrap(errs.EDecode, "truth", err, "truth document is not valid JSON")
	}
	if truth.ProjectID == "" {
		return nil, errs.New(errs.ESchema, "truth.project_id", "required truth field is missing")
	}
	if len(truth.Rounds) == 0 {
		return nil, errs.New(errs.ESchema, "truth.rounds", "truth rounds must not be empty")
	}
	return &truth, nil
}

func LoadSet(v1Data, v2Data, v3Data, truthData []byte) (*DocumentSet, error) {
	v1, err := LoadDocument(v1Data)
	if err != nil {
		return nil, err
	}
	v2, err := LoadDocument(v2Data)
	if err != nil {
		return nil, err
	}
	v3, err := LoadDocument(v3Data)
	if err != nil {
		return nil, err
	}
	truth, err := LoadTruth(truthData)
	if err != nil {
		return nil, err
	}
	v1.Truth = truth
	v2.Truth = truth
	v3.Truth = truth
	if err := EnsureSameBattle(v1, v2, v3); err != nil {
		return nil, err
	}
	return &DocumentSet{V1: v1, V2: v2, V3: v3, Truth: truth}, nil
}

func loadW1(data []byte, w *Wire1) (*model.Project, error) {
	raw, err := decodeDocument(data, w)
	if err != nil {
		return nil, err
	}
	if err := requireMany(raw, []string{"format", "battle_id", "seed", "tick_ms", "epoch_ms", "start_round", "rounds", "units", "skills", "timeline", "lists"}); err != nil {
		return nil, err
	}
	if len(w.Units) == 0 || len(w.Skills) == 0 || len(w.Timeline) == 0 {
		return nil, errs.New(errs.ESchema, "units", "units, skills and timeline must not be empty")
	}
	for i := range w.Units {
		path := fmt.Sprintf("units[%d]", i)
		if err := requireMany(raw, []string{path + ".id", path + ".priority", path + ".recast", path + ".slot", path + ".energy", path + ".energy_max", path + ".regen"}); err != nil {
			return nil, err
		}
	}
	for i := range w.Skills {
		path := fmt.Sprintf("skills[%d]", i)
		if err := requireMany(raw, []string{path + ".id", path + ".actor", path + ".cost", path + ".cast_cd", path + ".power"}); err != nil {
			return nil, err
		}
	}

	p := baseProject(w.BattleID, w.Seed, 1, FormatTickV1, w.TickMS, w.EpochMS, w.StartRound, w.Rounds, w.EnemyHP)
	p.Reorders = mapWReorders(w.Reorders)
	for _, u := range w.Units {
		if err := validateCooldown(u.ID, fmt.Sprintf("units[id=%s].recast", u.ID), u.Recast); err != nil {
			return nil, err
		}
		p.Units = append(p.Units, model.Unit{ID: u.ID, Name: u.Name, BaseWeight: u.Priority, TieOrdinal: u.Slot, InitialCooldown: u.Recast, InitialResource: u.Energy, MaxResource: u.EnergyMax, Regen: u.Regen})
		p.InitialStates = append(p.InitialStates, model.UnitState{UnitID: u.ID, Resource: u.Energy, CooldownRemaining: u.Recast, Alive: true})
	}
	for _, s := range w.Skills {
		p.Skills = append(p.Skills, model.Skill{ID: s.ID, UnitID: s.Actor, Cost: s.Cost, Cooldown: s.CastCD, Power: s.Power, Conditional: s.Conditional})
	}
	for i, frame := range w.Timeline {
		roundNo := frame.Round
		if roundNo == 0 {
			roundNo = w.StartRound + int64(i)
		}
		ts, err := convert.RoundIndexToMillis(roundNo, w.StartRound, w.EpochMS, w.TickMS, fmt.Sprintf("timeline[%d].round", i))
		if err != nil {
			return nil, err
		}
		spec := model.RoundSpec{Round: roundNo, TimestampMs: ts}
		actionsPath := fmt.Sprintf("timeline[%d].actions", i)
		if err := requirePath(raw, actionsPath); err != nil {
			return nil, err
		}
		for j, action := range frame.Actions {
			if err := requirePath(raw, fmt.Sprintf("%s[%d].actor", actionsPath, j)); err != nil {
				return nil, err
			}
			if err := requirePath(raw, fmt.Sprintf("%s[%d].technique", actionsPath, j)); err != nil {
				return nil, err
			}
			skill, ok := p.Skill(action.Technique)
			if !ok {
				return nil, errs.New(errs.ESchema, fmt.Sprintf("timeline[%d].actions[%d].technique", i, j), "unknown skill %q", action.Technique)
			}
			flag, err := normalizeIntFlag(action.Flag, action.Assert, skill.Conditional, fmt.Sprintf("timeline[%d].actions[%d].flag", i, j), roundNo, action.Actor)
			if err != nil {
				return nil, err
			}
			spec.Actions = append(spec.Actions, model.PlannedAction{UnitID: action.Actor, SkillID: action.Technique, Flag: flag})
		}
		p.Rounds = append(p.Rounds, spec)
	}
	p.SkillSequence, err = parseSequence(w.Lists.Alpha, true)
	if err != nil {
		return nil, errs.Wrap(errs.ESchema, "lists.alpha", err, "tick-v1 alpha list must be skill release sequence")
	}
	p.ActorSequence, err = parseSequence(w.Lists.Beta, false)
	if err != nil {
		return nil, errs.Wrap(errs.ESchema, "lists.beta", err, "tick-v1 beta list must be actor queue sequence")
	}
	if err := validateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadW2(data []byte, w *Wire2) (*model.Project, error) {
	raw, err := decodeDocument(data, w)
	if err != nil {
		return nil, err
	}
	if err := requireMany(raw, []string{"formatNo", "battleId", "seed", "tickMilliseconds", "epochMilliseconds", "startRound", "totalRounds", "unitRecords", "skillRecords", "timePoints", "sequenceBox"}); err != nil {
		return nil, err
	}
	if len(w.UnitRecords) == 0 || len(w.SkillRecords) == 0 || len(w.TimePoints) == 0 {
		return nil, errs.New(errs.ESchema, "unitRecords", "unitRecords, skillRecords and timePoints must not be empty")
	}
	for i := range w.UnitRecords {
		path := fmt.Sprintf("unitRecords[%d]", i)
		if err := requireMany(raw, []string{path + ".unitId", path + ".priority", path + ".recast", path + ".slotIndex", path + ".energy", path + ".energyCap", path + ".regen"}); err != nil {
			return nil, err
		}
	}

	p := baseProject(w.BattleID, w.Seed, 2, FormatChronoV2, w.TickMilliseconds, w.EpochMilliseconds, w.StartRound, w.TotalRounds, w.EnemyHP)
	p.Reorders = mapWReorders(w.ReorderCommands)
	for _, u := range w.UnitRecords {
		if err := validateCooldown(u.UnitID, fmt.Sprintf("unitRecords[id=%s].priority", u.UnitID), u.Priority); err != nil {
			return nil, err
		}
		// Chrono/swapped format: priority is cooldown remaining and recast is action weight.
		p.Units = append(p.Units, model.Unit{ID: u.UnitID, Name: u.DisplayName, BaseWeight: u.Recast, TieOrdinal: u.SlotIndex, InitialCooldown: u.Priority, InitialResource: u.Energy, MaxResource: u.EnergyCap, Regen: u.Regen})
		p.InitialStates = append(p.InitialStates, model.UnitState{UnitID: u.UnitID, Resource: u.Energy, CooldownRemaining: u.Priority, Alive: true})
	}
	for _, s := range w.SkillRecords {
		p.Skills = append(p.Skills, model.Skill{ID: s.SkillID, UnitID: s.OwnerID, Cost: s.EnergyCost, Cooldown: s.Cooldown, Power: s.Potency, Conditional: s.Conditional})
	}
	for i, point := range w.TimePoints {
		roundNo, canonicalMS, err := convert.CanonicalRoundTrip(point.AtMilliseconds, w.StartRound, w.EpochMilliseconds, w.TickMilliseconds, convert.RoundFloor, fmt.Sprintf("timePoints[%d].atMilliseconds", i))
		if err != nil {
			return nil, err
		}
		spec := model.RoundSpec{Round: roundNo, TimestampMs: canonicalMS}
		commandsPath := fmt.Sprintf("timePoints[%d].commands", i)
		if err := requirePath(raw, commandsPath); err != nil {
			return nil, err
		}
		for j, action := range point.Commands {
			if err := requirePath(raw, fmt.Sprintf("%s[%d].actorId", commandsPath, j)); err != nil {
				return nil, err
			}
			if err := requirePath(raw, fmt.Sprintf("%s[%d].techniqueId", commandsPath, j)); err != nil {
				return nil, err
			}
			skill, ok := p.Skill(action.TechniqueID)
			if !ok {
				return nil, errs.New(errs.ESchema, fmt.Sprintf("timePoints[%d].commands[%d].techniqueId", i, j), "unknown skill %q", action.TechniqueID)
			}
			flag, err := normalizeBoolFlag(action.ConditionFlag, action.ConditionBit, skill.Conditional, fmt.Sprintf("timePoints[%d].commands[%d].conditionFlag", i, j), roundNo, action.ActorID)
			if err != nil {
				return nil, err
			}
			spec.Actions = append(spec.Actions, model.PlannedAction{UnitID: action.ActorID, SkillID: action.TechniqueID, Flag: flag})
		}
		p.Rounds = append(p.Rounds, spec)
	}
	// Swapped list semantics: alpha is actor queue, beta is skill release sequence.
	p.ActorSequence, err = parseSequence(w.SequenceBox.Alpha, false)
	if err != nil {
		return nil, errs.Wrap(errs.ESchema, "sequenceBox.alpha", err, "chrono-v2 alpha list must be actor queue sequence")
	}
	p.SkillSequence, err = parseSequence(w.SequenceBox.Beta, true)
	if err != nil {
		return nil, errs.Wrap(errs.ESchema, "sequenceBox.beta", err, "chrono-v2 beta list must be skill release sequence")
	}
	if err := validateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadW3(data []byte, w *Wire3) (*model.Project, error) {
	raw, err := decodeDocument(data, w)
	if err != nil {
		return nil, err
	}
	if envelopeFormatMissingAs3(raw, w) {
		// Wire3 uses fmt; decodeDocument still succeeds and raw is available.
	}
	if err := requireMany(raw, []string{"fmt", "id", "seed", "tick", "epoch", "base_round", "rounds", "actors", "techs", "frames", "lists"}); err != nil {
		return nil, err
	}
	if w.Fmt != 3 {
		return nil, errs.New(errs.ESchema, "fmt", "unknown compact format number: %d", w.Fmt)
	}
	if len(w.Actors) == 0 || len(w.Techs) == 0 || len(w.Frames) == 0 {
		return nil, errs.New(errs.ESchema, "actors", "actors, techs and frames must not be empty")
	}
	for i := range w.Actors {
		path := fmt.Sprintf("actors[%d]", i)
		if err := requireMany(raw, []string{path + ".id", path + ".w", path + ".cd", path + ".ord", path + ".res", path + ".max", path + ".regen"}); err != nil {
			return nil, err
		}
	}

	p := baseProject(w.ID, w.Seed, 3, FormatCompactV3, w.Tick, w.Epoch, w.BaseRound, w.Rounds, w.Enemy)
	p.Reorders = mapWReorders(w.Orders)
	for _, u := range w.Actors {
		if err := validateCooldown(u.ID, fmt.Sprintf("actors[id=%s].cd", u.ID), u.CD); err != nil {
			return nil, err
		}
		p.Units = append(p.Units, model.Unit{ID: u.ID, Name: u.Name, BaseWeight: u.W, TieOrdinal: u.Ord, InitialCooldown: u.CD, InitialResource: u.Res, MaxResource: u.Max, Regen: u.Regen})
		p.InitialStates = append(p.InitialStates, model.UnitState{UnitID: u.ID, Resource: u.Res, CooldownRemaining: u.CD, Alive: true})
	}
	for _, s := range w.Techs {
		p.Skills = append(p.Skills, model.Skill{ID: s.ID, UnitID: s.Owner, Cost: s.Cost, Cooldown: s.CD, Power: s.Pow, Conditional: s.Cond})
	}
	for i, frame := range w.Frames {
		roundNo := frame.Round
		if roundNo == 0 {
			roundNo = w.BaseRound + int64(i)
		}
		ts, err := convert.RoundIndexToMillis(roundNo, w.BaseRound, w.Epoch, w.Tick, fmt.Sprintf("frames[%d].r", i))
		if err != nil {
			return nil, err
		}
		spec := model.RoundSpec{Round: roundNo, TimestampMs: ts}
		movesPath := fmt.Sprintf("frames[%d].moves", i)
		if err := requirePath(raw, movesPath); err != nil {
			return nil, err
		}
		for j, move := range frame.Moves {
			if err := requirePath(raw, fmt.Sprintf("%s[%d].a", movesPath, j)); err != nil {
				return nil, err
			}
			if err := requirePath(raw, fmt.Sprintf("%s[%d].t", movesPath, j)); err != nil {
				return nil, err
			}
			skill, ok := p.Skill(move.Tech)
			if !ok {
				return nil, errs.New(errs.ESchema, fmt.Sprintf("frames[%d].moves[%d].t", i, j), "unknown technique %q", move.Tech)
			}
			flag, err := normalizeStringFlag(move.Flag, move.Bit, skill.Conditional, fmt.Sprintf("frames[%d].moves[%d].f", i, j), roundNo, move.Actor)
			if err != nil {
				return nil, err
			}
			spec.Actions = append(spec.Actions, model.PlannedAction{UnitID: move.Actor, SkillID: move.Tech, Flag: flag})
		}
		p.Rounds = append(p.Rounds, spec)
	}
	p.SkillSequence, err = parseSequence(w.Lists.Skill, true)
	if err != nil {
		return nil, errs.Wrap(errs.ESchema, "lists.skill_seq", err, "compact skill sequence is invalid")
	}
	p.ActorSequence, err = parseSequence(w.Lists.Actor, false)
	if err != nil {
		return nil, errs.Wrap(errs.ESchema, "lists.actor_seq", err, "compact actor queue sequence is invalid")
	}
	if err := validateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func envelopeFormatMissingAs3(raw map[string]any, w *Wire3) bool { return raw != nil }

func baseProject(id string, seed int64, formatNo int, source string, tickMs, epochMs, startRound int64, rounds, enemyHP int) *model.Project {
	return &model.Project{
		ID:            id,
		Seed:          seed,
		FormatNo:      formatNo,
		SourceFormat:  source,
		SchemaVersion: model.CanonicalFormatVersion,
		TickMs:        tickMs,
		EpochMs:       epochMs,
		StartRound:    startRound,
		TotalRounds:   rounds,
		EnemyHP:       enemyHP,
	}
}

func mapWReorders(in []WReorder) []model.ReorderInstruction {
	out := make([]model.ReorderInstruction, 0, len(in))
	for _, r := range in {
		out = append(out, model.ReorderInstruction{Offset: r.Offset, ID: r.InstructionID, UnitID: r.Actor, AfterRound: r.AfterRound, NewTieOrdinal: r.NewSlot, Reason: r.Reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	return out
}

func validateCooldown(unitID, field string, cooldown int) error {
	if cooldown < 0 {
		return &errs.Error{Code: errs.ECool, Field: field, UnitID: unitID, Values: []string{fmt.Sprint(cooldown), "0"}, Message: "cooldown remaining is negative"}
	}
	if cooldown > MaxCooldownRemaining {
		return &errs.Error{Code: errs.ECool, Field: field, UnitID: unitID, Values: []string{fmt.Sprint(cooldown), fmt.Sprint(MaxCooldownRemaining)}, Message: "cooldown remaining exceeds protocol bound"}
	}
	return nil
}

func normalizeIntFlag(flag *int, assert *bool, conditional bool, field string, round int64, unit string) (*bool, error) {
	if !conditional {
		if flag == nil {
			return nil, nil
		}
		value := *flag != 0
		if assert != nil && *assert != value {
			return nil, flagContradiction(field, round, unit)
		}
		return &value, nil
	}
	if flag == nil {
		return nil, errs.New(errs.ESchema, field, "conditional action requires a flag").WithRound(round).WithUnit(unit)
	}
	if *flag != 0 && *flag != 1 {
		return nil, flagContradiction(field, round, unit)
	}
	value := *flag == 1
	if assert != nil && *assert != value {
		return nil, flagContradiction(field, round, unit)
	}
	return &value, nil
}

func normalizeBoolFlag(flag *bool, bit *int, conditional bool, field string, round int64, unit string) (*bool, error) {
	if bit != nil && (*bit != 0 && *bit != 1) {
		return nil, flagContradiction(field, round, unit)
	}
	if flag == nil {
		if bit == nil {
			if conditional {
				return nil, errs.New(errs.ESchema, field, "conditional action requires conditionFlag").WithRound(round).WithUnit(unit)
			}
			return nil, nil
		}
		value := *bit == 1
		return &value, nil
	}
	value := *flag
	if bit != nil && (*bit == 1) != value {
		return nil, flagContradiction(field, round, unit)
	}
	return &value, nil
}

func normalizeStringFlag(flag string, bit *int, conditional bool, field string, round int64, unit string) (*bool, error) {
	upper := strings.ToUpper(strings.TrimSpace(flag))
	var value bool
	switch upper {
	case "Y", "YES", "TRUE", "1":
		value = true
	case "N", "NO", "FALSE", "0":
		value = false
	case "":
		if conditional {
			return nil, errs.New(errs.ESchema, field, "conditional action requires flag").WithRound(round).WithUnit(unit)
		}
		return nil, nil
	default:
		return nil, flagContradiction(field, round, unit)
	}
	if bit != nil {
		if *bit != 0 && *bit != 1 {
			return nil, flagContradiction(field, round, unit)
		}
		if (*bit == 1) != value {
			return nil, flagContradiction(field, round, unit)
		}
	}
	return &value, nil
}

func flagContradiction(field string, round int64, unit string) error {
	return &errs.Error{Code: errs.EFlag, Field: field, Round: round, UnitID: unit, Message: "conditional flag representations contradict each other"}
}

func parseSequence(items []string, skill bool) ([]model.SequenceItem, error) {
	out := make([]model.SequenceItem, 0, len(items))
	for _, item := range items {
		parts := strings.Split(item, "|")
		if skill {
			if len(parts) != 4 || parts[0] != "S" {
				return nil, fmt.Errorf("skill ref %q must be S|round|unit|skill", item)
			}
			var round int64
			if _, err := fmt.Sscanf(parts[1], "%d", &round); err != nil {
				return nil, err
			}
			out = append(out, model.SequenceItem{Round: round, UnitID: parts[2], SkillID: parts[3], Ref: item})
		} else {
			if len(parts) != 3 || parts[0] != "A" {
				return nil, fmt.Errorf("actor ref %q must be A|round|unit", item)
			}
			var round int64
			if _, err := fmt.Sscanf(parts[1], "%d", &round); err != nil {
				return nil, err
			}
			out = append(out, model.SequenceItem{Round: round, UnitID: parts[2], Ref: item})
		}
	}
	return out, nil
}

func validateProject(p *model.Project) error {
	if p.ID == "" {
		return errs.New(errs.ESchema, "id", "project id is missing")
	}
	unitIDs := map[string]bool{}
	for _, u := range p.Units {
		if unitIDs[u.ID] {
			return errs.New(errs.ESchema, "units[].id", "duplicate unit id %q", u.ID)
		}
		unitIDs[u.ID] = true
		if u.MaxResource < 0 || u.InitialResource < 0 || u.InitialResource > u.MaxResource {
			return errs.New(errs.ESchema, fmt.Sprintf("units[id=%s].energy", u.ID), "resource is outside [0,max]")
		}
	}
	skillIDs := map[string]bool{}
	for _, s := range p.Skills {
		if skillIDs[s.ID] {
			return errs.New(errs.ESchema, "skills[].id", "duplicate skill id %q", s.ID)
		}
		skillIDs[s.ID] = true
		if !unitIDs[s.UnitID] {
			return errs.New(errs.ESchema, fmt.Sprintf("skills[id=%s].actor", s.ID), "owner unit %q does not exist", s.UnitID)
		}
		if s.Cost < 0 || s.Cooldown < 0 || s.Power < 0 {
			return errs.New(errs.ESchema, fmt.Sprintf("skills[id=%s]", s.ID), "cost, cooldown and power must be non-negative")
		}
	}
	if len(p.Rounds) != p.TotalRounds {
		return errs.New(errs.ESchema, "rounds", "round count %d does not match timeline length %d", p.TotalRounds, len(p.Rounds))
	}
	for i, round := range p.Rounds {
		expected := p.StartRound + int64(i)
		if round.Round != expected {
			return errs.New(errs.ESchema, fmt.Sprintf("timeline[%d].round", i), "round index %d does not match expected %d", round.Round, expected)
		}
		seen := map[string]bool{}
		for _, action := range round.Actions {
			if !unitIDs[action.UnitID] {
				return errs.New(errs.ESchema, fmt.Sprintf("rounds[%d].actions", i), "unknown unit %q", action.UnitID)
			}
			key := action.UnitID + "/" + action.SkillID
			if seen[key] {
				return errs.New(errs.ESchema, fmt.Sprintf("rounds[%d].actions", i), "duplicate action %s", key)
			}
			seen[key] = true
			skill, ok := p.Skill(action.SkillID)
			if !ok || skill.UnitID != action.UnitID {
				return errs.New(errs.ESchema, fmt.Sprintf("rounds[%d].actions.skill", i), "skill %q does not belong to unit %q", action.SkillID, action.UnitID)
			}
			if skill.Conditional && action.Flag == nil {
				return errs.New(errs.ESchema, fmt.Sprintf("rounds[%d].actions.flag", i), "conditional skill flag was not normalized")
			}
		}
	}
	return nil
}

func EnsureSameBattle(projects ...*model.Project) error {
	if len(projects) < 2 {
		return nil
	}
	base := projects[0]
	baseHash, err := SemanticHash(base)
	if err != nil {
		return err
	}
	for _, p := range projects[1:] {
		hash, err := SemanticHash(p)
		if err != nil {
			return err
		}
		if hash == baseHash {
			continue
		}
		if loc, flagErr := firstFlagDifference(base, p); flagErr != nil {
			return flagErr
		} else if loc != nil {
			return flagContradiction("conditionFlag", loc.Round, loc.UnitID)
		}
		return errs.New(errs.ESchema, "formats", "format %d semantic hash %s differs from format %d hash %s", p.FormatNo, hash[:12], base.FormatNo, baseHash[:12])
	}
	return nil
}

type flagLocation struct {
	Round  int64
	UnitID string
}

func firstFlagDifference(a, b *model.Project) (*flagLocation, error) {
	rounds := len(a.Rounds)
	if len(b.Rounds) < rounds {
		rounds = len(b.Rounds)
	}
	for i := 0; i < rounds; i++ {
		am := map[string]*bool{}
		bm := map[string]*bool{}
		for _, action := range a.Rounds[i].Actions {
			am[action.UnitID+"/"+action.SkillID] = action.Flag
		}
		for _, action := range b.Rounds[i].Actions {
			bm[action.UnitID+"/"+action.SkillID] = action.Flag
		}
		for key, af := range am {
			bf, ok := bm[key]
			if !ok {
				continue
			}
			if (af == nil) != (bf == nil) || (af != nil && *af != *bf) {
				parts := strings.SplitN(key, "/", 2)
				unit := key
				if len(parts) == 2 {
					unit = parts[0]
				}
				return &flagLocation{Round: a.Rounds[i].Round, UnitID: unit}, nil
			}
		}
	}
	return nil, nil
}

type semanticProject struct {
	ID            string                     `json:"id"`
	Seed          int64                      `json:"seed"`
	TickMs        int64                      `json:"tick_ms"`
	EpochMs       int64                      `json:"epoch_ms"`
	StartRound    int64                      `json:"start_round"`
	TotalRounds   int                        `json:"total_rounds"`
	EnemyHP       int                        `json:"enemy_hp"`
	Units         []model.Unit               `json:"units"`
	Skills        []model.Skill              `json:"skills"`
	Rounds        []model.RoundSpec          `json:"rounds"`
	SkillSequence []model.SequenceItem       `json:"skill_sequence"`
	ActorSequence []model.SequenceItem       `json:"actor_sequence"`
	Reorders      []model.ReorderInstruction `json:"reorders"`
	InitialStates []model.UnitState          `json:"initial_states"`
}

func SemanticHash(p *model.Project) (string, error) {
	return canonical.JSONHash(semanticProject{
		ID: p.ID, Seed: p.Seed, TickMs: p.TickMs, EpochMs: p.EpochMs, StartRound: p.StartRound,
		TotalRounds: p.TotalRounds, EnemyHP: p.EnemyHP, Units: p.Units, Skills: p.Skills,
		Rounds: p.Rounds, SkillSequence: p.SkillSequence, ActorSequence: p.ActorSequence,
		Reorders: p.Reorders, InitialStates: model.SortedStates(p.InitialStates),
	})
}

func MustSemanticHash(p *model.Project) string {
	h, err := SemanticHash(p)
	if err != nil {
		panic(err)
	}
	return h
}

func TruthFingerprint(truth *model.Truth) ([]string, string, error) {
	roundHashes := make([]string, 0, len(truth.Rounds))
	for _, round := range truth.Rounds {
		h, err := canonical.JSONHash(round)
		if err != nil {
			return nil, "", err
		}
		roundHashes = append(roundHashes, h)
	}
	all, err := canonical.JSONHash(struct {
		Version string   `json:"version"`
		ID      string   `json:"id"`
		Rounds  []string `json:"rounds"`
		Winner  string   `json:"winner"`
	}{model.TruthFingerprintVer, truth.ProjectID, roundHashes, truth.FinalWinner})
	if err != nil {
		return nil, "", err
	}
	return roundHashes, all, nil
}
