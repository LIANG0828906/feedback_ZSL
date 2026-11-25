package loader

import (
	"encoding/json"
	"fmt"
	"os"

	"replaybench/internal/eerr"
	"replaybench/internal/fixed"
	"replaybench/internal/model"
	"replaybench/internal/timeconv"
)

type WireReplay struct {
	Format            *int             `json:"format"`
	FormatVersion     string           `json:"format_version"`
	ReplayID          string           `json:"replay_id"`
	Seed              int64            `json:"seed"`
	SchemaVersion     string           `json:"schema_version"`
	Rounds            *int             `json:"rounds"`
	MsPerRound        *int64           `json:"ms_per_round"`
	EpochMs           int64            `json:"epoch_ms"`
	Rounding          string           `json:"rounding"`
	ResourceRegen     *int64           `json:"resource_regen"`
	CooldownReduction *int64           `json:"cooldown_reduction"`
	Flags             map[string]any   `json:"flags"`
	Units             []WireUnit       `json:"units"`
	Skills            []WireSkill      `json:"skills"`
	Frames            []WireFrame      `json:"frames"`
	Edits             []model.Edit     `json:"edits"`
	Truth             *json.RawMessage `json:"truth"`
}

type WireUnit struct {
	ID              *string `json:"id"`
	Team            *string `json:"team"`
	MaxHP           *int64  `json:"max_hp"`
	HP              *int64  `json:"hp"`
	MaxResource     *int64  `json:"max_resource"`
	Resource        *int64  `json:"resource"`
	PrimaryInt      *int64  `json:"primary_int"`
	SecondaryInt    *int64  `json:"secondary_int"`
	CooldownSkillID string  `json:"cooldown_skill_id"`
	QueuePosition   *int    `json:"queue_position"`
}

type WireSkill struct {
	ID            *string `json:"id"`
	OwnerID       *string `json:"owner_id"`
	TargetID      *string `json:"target_id"`
	Priority      *int    `json:"priority"`
	ResourceCost  *int64  `json:"resource_cost"`
	Cooldown      *int64  `json:"cooldown"`
	CooldownGroup string  `json:"cooldown_group"`
	CooldownScope string  `json:"cooldown_scope"`
	Damage        *int64  `json:"damage"`
	Heal          int64   `json:"heal"`
	GrantResource int64   `json:"grant_resource"`
	ConditionFlag string  `json:"condition_flag"`
}

type WireFrame struct {
	Offset        *int      `json:"offset"`
	Round         *int64    `json:"round"`
	TimestampMs   *int64    `json:"timestamp_ms"`
	PrimaryList   *[]string `json:"primary_list"`
	SecondaryList *[]string `json:"secondary_list"`
}

type Loaded struct {
	Replay *model.Replay
	Truth  *model.Truth
	Raw    []byte
}

func LoadFile(path string) (*Loaded, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, eerr.Schema("$.file", "read sample %s: %v", path, err)
	}
	return LoadJSON(b)
}

func LoadJSON(raw []byte) (*Loaded, error) {
	var wire WireReplay
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, eerr.Schema("$", "invalid JSON: %v", err)
	}
	if wire.Format == nil {
		return nil, eerr.Schema("$.format", "format is required")
	}
	format := model.FormatID(*wire.Format)
	switch format {
	case model.FormatWeightCast, model.FormatCooldownQueue, model.FormatTimestamp:
	default:
		return nil, eerr.Schema("$.format", "unknown format number %d", *wire.Format)
	}
	if wire.FormatVersion == "" {
		return nil, eerr.Schema("$.format_version", "format_version is required")
	}
	if wire.ReplayID == "" {
		return nil, eerr.Schema("$.replay_id", "replay_id is required")
	}
	if wire.SchemaVersion == "" {
		wire.SchemaVersion = model.SchemaVersion
	}
	if wire.Rounds == nil {
		return nil, eerr.Schema("$.rounds", "rounds is required")
	}
	rounds := *wire.Rounds
	if rounds <= 0 {
		return nil, eerr.Schema("$.rounds", "rounds must be positive")
	}
	if wire.MsPerRound == nil || *wire.MsPerRound <= 0 {
		return nil, eerr.Schema("$.ms_per_round", "positive ms_per_round is required")
	}
	rounding, err := timeconv.NormalizeRounding(wire.Rounding)
	if err != nil {
		return nil, err
	}
	if len(wire.Units) == 0 {
		return nil, eerr.Schema("$.units", "at least one unit is required")
	}
	if len(wire.Skills) == 0 {
		return nil, eerr.Schema("$.skills", "at least one skill is required")
	}
	if len(wire.Frames) != rounds {
		return nil, eerr.Schema("$.frames", "frames length %d must equal rounds %d", len(wire.Frames), rounds)
	}

	replay := &model.Replay{
		ID:                  wire.ReplayID,
		Seed:                wire.Seed,
		Format:              format,
		SourceFormatVersion: fmt.Sprintf("rf%d/%s", format, wire.FormatVersion),
		SchemaVersion:       wire.SchemaVersion,
		Rounds:              rounds,
		Time: model.TimeBasis{
			Mode:       map[model.FormatID]string{model.FormatTimestamp: "millis"}[format],
			EpochMs:    wire.EpochMs,
			MsPerRound: *wire.MsPerRound,
			Rounding:   rounding,
		},
		Rules: model.RuleSet{
			Weights:           map[string]int64{},
			CooldownReduction: 0,
			Flags:             map[string]bool{},
			ParamsVersion:     model.ParamsVersion,
		},
		Edits: wire.Edits,
	}
	if replay.Time.Mode == "" {
		replay.Time.Mode = "round"
	}
	if wire.CooldownReduction != nil {
		if *wire.CooldownReduction < 0 {
			return nil, eerr.Schema("$.cooldown_reduction", "must be non-negative")
		}
		replay.Rules.CooldownReduction = *wire.CooldownReduction
	}
	if wire.ResourceRegen != nil {
		regen, err := fixed.FromWhole(*wire.ResourceRegen, "$.resource_regen")
		if err != nil {
			return nil, err
		}
		replay.Rules.ResourceRegen = regen
	}
	for k, raw := range wire.Flags {
		v, err := normalizeFlag(raw, "$.flags."+k)
		if err != nil {
			return nil, err
		}
		replay.Rules.Flags[k] = v
	}

	unitIDs := map[string]struct{}{}
	for i, wu := range wire.Units {
		path := fmt.Sprintf("$.units[%d]", i)
		u, err := mapUnit(wu, path, format, replay)
		if err != nil {
			return nil, err
		}
		if _, exists := unitIDs[u.ID]; exists {
			return nil, eerr.Schema(path+".id", "duplicate unit id %q", u.ID)
		}
		unitIDs[u.ID] = struct{}{}
		replay.Units = append(replay.Units, u)
		replay.Rules.Weights[u.ID] = u.Weight
	}

	skillIDs := map[string]struct{}{}
	for i, ws := range wire.Skills {
		path := fmt.Sprintf("$.skills[%d]", i)
		s, err := mapSkill(ws, path)
		if err != nil {
			return nil, err
		}
		if _, exists := skillIDs[s.ID]; exists {
			return nil, eerr.Schema(path+".id", "duplicate skill id %q", s.ID)
		}
		if _, ok := unitIDs[s.OwnerID]; !ok {
			return nil, eerr.Schema(path+".owner_id", "owner unit %q does not exist", s.OwnerID)
		}
		if s.TargetID != "" {
			if _, ok := unitIDs[s.TargetID]; !ok {
				return nil, eerr.Schema(path+".target_id", "target unit %q does not exist", s.TargetID)
			}
		}
		skillIDs[s.ID] = struct{}{}
		replay.Skills = append(replay.Skills, s)
	}
	for _, u := range replay.Units {
		if u.CooldownSkillID != "" {
			s, ok := replay.SkillByID(u.CooldownSkillID)
			if !ok {
				return nil, eerr.Schema("$.units", "cooldown_skill_id %q does not exist", u.CooldownSkillID)
			}
			if s.OwnerID != u.ID {
				return nil, eerr.Schema("$.units", "cooldown skill %q must belong to unit %q", s.ID, u.ID)
			}
		}
	}

	queuePositions := map[string]int{}
	for i, wf := range wire.Frames {
		path := fmt.Sprintf("$.frames[%d]", i)
		d, cast, queue, err := mapFrame(wf, path, i, format, replay.Time)
		if err != nil {
			return nil, err
		}
		for _, sid := range cast {
			if _, ok := replay.SkillByID(sid); !ok {
				return nil, eerr.Schema(path+".primary_list", "skill id %q does not exist", sid)
			}
		}
		seen := map[string]struct{}{}
		queueListPath := path + ".secondary_list"
		if format == model.FormatCooldownQueue {
			queueListPath = path + ".primary_list"
		}
		for pos, uid := range queue {
			if _, ok := unitIDs[uid]; !ok {
				return nil, eerr.Schema(queueListPath, "unit id %q does not exist", uid)
			}
			if _, dup := seen[uid]; dup {
				return nil, eerr.Schema(queueListPath, "duplicate queued unit %q", uid)
			}
			seen[uid] = struct{}{}
			queuePositions[uid] = pos
		}
		if len(seen) != len(unitIDs) {
			return nil, eerr.Schema(path, "actor_queue must contain every unit exactly once")
		}
		d.CastSequence = append([]string(nil), cast...)
		d.ActorQueue = append([]string(nil), queue...)
		replay.Directives = append(replay.Directives, d)
		if i == 0 {
			replay.Rules.Formation = append([]string(nil), queue...)
		}
	}
	for idx := range replay.Units {
		if pos, ok := queuePositions[replay.Units[idx].ID]; ok {
			replay.Units[idx].QueuePosition = pos
		}
	}

	if wire.Truth == nil || len(*wire.Truth) == 0 || string(*wire.Truth) == "null" {
		return nil, eerr.Schema("$.truth", "truth manifest is required")
	}
	var truth model.Truth
	if err := json.Unmarshal(*wire.Truth, &truth); err != nil {
		return nil, eerr.Schema("$.truth", "invalid truth manifest: %v", err)
	}
	if len(truth.Rounds) != rounds {
		return nil, eerr.Schema("$.truth.rounds", "truth rounds length %d must equal %d", len(truth.Rounds), rounds)
	}

	replay.ContentHash = model.ContentHash(replay)
	return &Loaded{Replay: replay, Truth: &truth, Raw: append([]byte(nil), raw...)}, nil
}

func requireInt64(p *int64, path string) (int64, error) {
	if p == nil {
		return 0, eerr.Schema(path, "field is required")
	}
	return *p, nil
}

func requireString(p *string, path string) (string, error) {
	if p == nil || *p == "" {
		return "", eerr.Schema(path, "non-empty string is required")
	}
	return *p, nil
}

func normalizeFlag(raw any, path string) (bool, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case float64:
		if v == 0 || v == 1 {
			return v == 1, nil
		}
	case int:
		if v == 0 || v == 1 {
			return v == 1, nil
		}
	case string:
		switch v {
		case "1", "true", "TRUE", "True":
			return true, nil
		case "0", "false", "FALSE", "False", "":
			return false, nil
		}
	case nil:
		return false, nil
	}
	return false, eerr.Schema(path, "flag must be boolean or 0/1 bit, got %T(%v)", raw, raw)
}

func mapUnit(wu WireUnit, path string, format model.FormatID, wire *model.Replay) (model.Unit, error) {
	id, err := requireString(wu.ID, path+".id")
	if err != nil {
		return model.Unit{}, err
	}
	team, err := requireString(wu.Team, path+".team")
	if err != nil {
		return model.Unit{}, err
	}
	maxHPWhole, err := requireInt64(wu.MaxHP, path+".max_hp")
	if err != nil {
		return model.Unit{}, err
	}
	hpWhole, err := requireInt64(wu.HP, path+".hp")
	if err != nil {
		return model.Unit{}, err
	}
	maxResWhole, err := requireInt64(wu.MaxResource, path+".max_resource")
	if err != nil {
		return model.Unit{}, err
	}
	resWhole, err := requireInt64(wu.Resource, path+".resource")
	if err != nil {
		return model.Unit{}, err
	}
	primary, err := requireInt64(wu.PrimaryInt, path+".primary_int")
	if err != nil {
		return model.Unit{}, err
	}
	secondary, err := requireInt64(wu.SecondaryInt, path+".secondary_int")
	if err != nil {
		return model.Unit{}, err
	}
	maxHP, err := fixed.FromWhole(maxHPWhole, path+".max_hp")
	if err != nil {
		return model.Unit{}, err
	}
	hp, err := fixed.FromWhole(hpWhole, path+".hp")
	if err != nil {
		return model.Unit{}, err
	}
	maxRes, err := fixed.FromWhole(maxResWhole, path+".max_resource")
	if err != nil {
		return model.Unit{}, err
	}
	res, err := fixed.FromWhole(resWhole, path+".resource")
	if err != nil {
		return model.Unit{}, err
	}
	if hp > maxHP {
		return model.Unit{}, eerr.Schema(path+".hp", "hp must not exceed max_hp")
	}
	if res > maxRes {
		return model.Unit{}, eerr.Schema(path+".resource", "resource must not exceed max_resource")
	}

	var weight, cooldown int64
	if format == model.FormatCooldownQueue {
		cooldown, weight = primary, secondary
	} else {
		weight, cooldown = primary, secondary
	}
	if weight < 0 {
		return model.Unit{}, eerr.Schema(path+".primary_int", "mapped action weight must be non-negative")
	}
	if cooldown < 0 {
		return model.Unit{}, eerr.Schema(path+".secondary_int", "mapped cooldown remaining must be non-negative")
	}
	queuePos := 0
	if wu.QueuePosition != nil {
		queuePos = *wu.QueuePosition
	}
	return model.Unit{
		ID: id, Team: team, MaxHP: maxHP, HP: hp, MaxResource: maxRes, Resource: res,
		Weight: weight, QueuePosition: queuePos,
		CooldownSkillID: wu.CooldownSkillID, CooldownRemain: cooldown,
	}, nil
}

func mapSkill(ws WireSkill, path string) (model.Skill, error) {
	id, err := requireString(ws.ID, path+".id")
	if err != nil {
		return model.Skill{}, err
	}
	owner, err := requireString(ws.OwnerID, path+".owner_id")
	if err != nil {
		return model.Skill{}, err
	}
	target, err := requireString(ws.TargetID, path+".target_id")
	if err != nil {
		return model.Skill{}, err
	}
	costWhole, err := requireInt64(ws.ResourceCost, path+".resource_cost")
	if err != nil {
		return model.Skill{}, err
	}
	cd, err := requireInt64(ws.Cooldown, path+".cooldown")
	if err != nil {
		return model.Skill{}, err
	}
	damageWhole, err := requireInt64(ws.Damage, path+".damage")
	if err != nil {
		return model.Skill{}, err
	}
	cost, err := fixed.FromWhole(costWhole, path+".resource_cost")
	if err != nil {
		return model.Skill{}, err
	}
	damage, err := fixed.FromWhole(damageWhole, path+".damage")
	if err != nil {
		return model.Skill{}, err
	}
	heal, err := fixed.FromWhole(ws.Heal, path+".heal")
	if err != nil {
		return model.Skill{}, err
	}
	grant, err := fixed.FromWhole(ws.GrantResource, path+".grant_resource")
	if err != nil {
		return model.Skill{}, err
	}
	priority := 0
	if ws.Priority != nil {
		priority = *ws.Priority
	}
	scope := ws.CooldownScope
	if scope == "" {
		scope = "unit"
	}
	if scope != "unit" && scope != "global" {
		return model.Skill{}, eerr.Schema(path+".cooldown_scope", "must be unit or global")
	}
	return model.Skill{
		ID: id, OwnerID: owner, TargetID: target, Priority: priority,
		ResourceCost: cost, Cooldown: cd, CooldownGroup: ws.CooldownGroup,
		CooldownScope: scope, Damage: damage, Heal: heal, GrantResource: grant,
		ConditionFlag: ws.ConditionFlag,
	}, nil
}

func mapFrame(wf WireFrame, path string, index int, format model.FormatID, basis model.TimeBasis) (model.Directive, []string, []string, error) {
	offset := index
	if wf.Offset != nil {
		offset = *wf.Offset
	}
	d := model.Directive{Offset: offset}
	timeField := path + ".round"
	if format == model.FormatTimestamp {
		if wf.TimestampMs == nil {
			return d, nil, nil, eerr.Schema(path+".timestamp_ms", "timestamp_ms is required for format 3")
		}
		timeField = path + ".timestamp_ms"
		round, err := timeconv.TimestampToRound(*wf.TimestampMs, basis, timeField)
		if err != nil {
			return d, nil, nil, err
		}
		d.Round = round
	} else {
		if wf.Round == nil {
			return d, nil, nil, eerr.Schema(path+".round", "round is required for format %d", format)
		}
		d.Round = int(*wf.Round)
		if int64(d.Round) != *wf.Round {
			return d, nil, nil, eerr.Schema(path+".round", "round %d is outside int range", *wf.Round)
		}
	}
	if d.Round != index+1 {
		return d, nil, nil, eerr.Schema(timeField, "mapped round %d must equal frame index+1 %d", d.Round, index+1)
	}
	if wf.PrimaryList == nil {
		return d, nil, nil, eerr.Schema(path+".primary_list", "primary_list is required")
	}
	if wf.SecondaryList == nil {
		return d, nil, nil, eerr.Schema(path+".secondary_list", "secondary_list is required")
	}
	primary := *wf.PrimaryList
	secondary := *wf.SecondaryList
	if format == model.FormatCooldownQueue {
		return d, append([]string(nil), secondary...), append([]string(nil), primary...), nil
	}
	return d, append([]string(nil), primary...), append([]string(nil), secondary...), nil
}
