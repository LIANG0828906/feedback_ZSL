package generator

import (
	"encoding/json"
	"math/rand"

	"replaybench/internal/loader"
	"replaybench/internal/model"
)

const DefaultSeed int64 = 20261126

type DocumentSet struct {
	Seed  int64
	Truth *model.Truth
	Wire  *loader.WireReplay
	JSON  map[model.FormatID][]byte
}

func DeterministicSet(seed int64) (*DocumentSet, error) {
	if seed == 0 {
		seed = DefaultSeed
	}
	truth := buildTruth(seed)
	wire := buildWire(seed, truth)
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(wire.Units), func(i, j int) { wire.Units[i], wire.Units[j] = wire.Units[j], wire.Units[i] })
	rng.Shuffle(len(wire.Skills), func(i, j int) { wire.Skills[i], wire.Skills[j] = wire.Skills[j], wire.Skills[i] })

	docs := map[model.FormatID][]byte{}
	for _, format := range []model.FormatID{model.FormatWeightCast, model.FormatCooldownQueue, model.FormatTimestamp} {
		w, err := variant(wire, format)
		if err != nil {
			return nil, err
		}
		b, err := json.MarshalIndent(w, "", "  ")
		if err != nil {
			return nil, err
		}
		docs[format] = b
	}
	return &DocumentSet{Seed: seed, Truth: truth, Wire: wire, JSON: docs}, nil
}

func buildWire(seed int64, truth *model.Truth) *loader.WireReplay {
	format := 1
	rounds := 3
	msPerRound := int64(1000)
	regen := int64(10)
	rawTruth := json.RawMessage(model.CanonicalJSON(truth))
	return &loader.WireReplay{
		Format: &format, FormatVersion: "1.0.0", ReplayID: "deterministic-contention",
		Seed: seed, SchemaVersion: model.SchemaVersion, Rounds: &rounds,
		MsPerRound: &msPerRound, EpochMs: 1_700_000_000_000, Rounding: "floor",
		ResourceRegen: &regen, Flags: map[string]any{"execute_flag": 1},
		Units: []loader.WireUnit{
			unit("red", "red", 260, 260, 100, 100, 9, 0, "red_bolt", 0),
			unit("alpha", "blue", 180, 180, 120, 70, 6, 1, "a_execute", 1),
			unit("beta", "blue", 140, 140, 100, 60, 3, 2, "b_hex", 0),
		},
		Skills: []loader.WireSkill{
			skill("red_bolt", "red", "alpha", 10, 0, 0, "", "unit", 35, 0, 0, ""),
			skill("a_nova", "alpha", "red", 10, 50, 1, "a_charge", "unit", 60, 0, 0, ""),
			skill("a_strike", "alpha", "red", 5, 30, 0, "", "unit", 30, 0, 0, ""),
			skill("a_execute", "alpha", "red", 20, 30, 2, "", "unit", 70, 0, 0, "execute_flag"),
			skill("a_team_ultra", "alpha", "red", 10, 0, 1, "global_fire", "global", 40, 0, 0, ""),
			skill("b_arrow", "beta", "red", 10, 10, 1, "b_ammo", "unit", 20, 0, 0, ""),
			skill("b_volley", "beta", "red", 5, 20, 1, "b_ammo", "unit", 30, 0, 0, ""),
			skill("b_team_ultra", "beta", "red", 20, 0, 1, "global_fire", "global", 35, 0, 0, ""),
			skill("b_hex", "beta", "red", 5, 10, 0, "", "unit", 25, 0, 0, ""),
		},
		Frames: []loader.WireFrame{
			frame(0, 1, []string{"red_bolt", "a_nova", "a_strike", "b_arrow", "b_volley"}, []string{"red", "alpha", "beta"}),
			frame(1, 2, []string{"red_bolt", "a_execute", "a_team_ultra", "b_volley", "b_team_ultra"}, []string{"red", "alpha", "beta"}),
			frame(2, 3, []string{"red_bolt", "a_nova", "a_strike", "b_arrow", "b_hex"}, []string{"red", "alpha", "beta"}),
		},
		Truth: &rawTruth,
	}
}

func unit(id, team string, maxHP, hp, maxRes, res, weight int64, queue int, cdSkill string, cooldown int64) loader.WireUnit {
	q := queue
	return loader.WireUnit{
		ID: &id, Team: &team, MaxHP: &maxHP, HP: &hp, MaxResource: &maxRes, Resource: &res,
		PrimaryInt: &weight, SecondaryInt: &cooldown, CooldownSkillID: cdSkill, QueuePosition: &q,
	}
}

func skill(id, owner, target string, priority int, cost, cooldown int64, group, scope string, damage, heal, grant int64, flag string) loader.WireSkill {
	p := priority
	return loader.WireSkill{
		ID: &id, OwnerID: &owner, TargetID: &target, Priority: &p,
		ResourceCost: &cost, Cooldown: &cooldown, CooldownGroup: group, CooldownScope: scope,
		Damage: &damage, Heal: heal, GrantResource: grant, ConditionFlag: flag,
	}
}

func frame(offset, round int, cast, queue []string) loader.WireFrame {
	off := offset
	r := int64(round)
	primary := append([]string(nil), cast...)
	secondary := append([]string(nil), queue...)
	return loader.WireFrame{Offset: &off, Round: &r, PrimaryList: &primary, SecondaryList: &secondary}
}

func variant(in *loader.WireReplay, format model.FormatID) (*loader.WireReplay, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var w loader.WireReplay
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	f := int(format)
	w.Format = &f
	w.FormatVersion = map[model.FormatID]string{
		model.FormatWeightCast: "1.0.0", model.FormatCooldownQueue: "2.0.0", model.FormatTimestamp: "3.0.0",
	}[format]
	if format == model.FormatCooldownQueue {
		for i := range w.Units {
			w.Units[i].PrimaryInt, w.Units[i].SecondaryInt = w.Units[i].SecondaryInt, w.Units[i].PrimaryInt
		}
		for i := range w.Frames {
			w.Frames[i].PrimaryList, w.Frames[i].SecondaryList = w.Frames[i].SecondaryList, w.Frames[i].PrimaryList
		}
	}
	if format == model.FormatTimestamp {
		for i := range w.Frames {
			round := int64(i + 1)
			ts := w.EpochMs + (round-1)*(*w.MsPerRound)
			w.Frames[i].Round = nil
			w.Frames[i].TimestampMs = &ts
		}
	}
	return &w, nil
}

func buildTruth(seed int64) *model.Truth {
	adj := []model.Adjudication{
		dec(1, "red", "red_bolt", "none", "cast", nil),
		dec(1, "alpha", "a_nova", "resource", "cast", []string{"a_nova", "a_strike"}),
		dec(1, "alpha", "a_strike", "resource", "rejected", []string{"a_nova", "a_strike"}),
		dec(1, "beta", "b_arrow", "cooldown_group", "cast", []string{"b_arrow", "b_volley"}),
		dec(1, "beta", "b_volley", "cooldown_group", "rejected", []string{"b_arrow", "b_volley"}),

		dec(2, "red", "red_bolt", "none", "cast", nil),
		dec(2, "alpha", "a_execute", "none", "cast", nil),
		dec(2, "alpha", "a_team_ultra", "cooldown_group", "cast", []string{"a_team_ultra", "b_team_ultra"}),
		dec(2, "beta", "b_team_ultra", "cooldown_group", "rejected", []string{"a_team_ultra", "b_team_ultra"}),
		dec(2, "beta", "b_volley", "none", "cast", nil),

		dec(3, "red", "red_bolt", "none", "cast", nil),
		dec(3, "alpha", "a_nova", "resource", "rejected", []string{"a_nova", "a_strike"}),
		dec(3, "alpha", "a_strike", "resource", "rejected", []string{"a_nova", "a_strike"}),
		dec(3, "beta", "b_arrow", "resource", "cast", []string{"b_arrow", "b_hex"}),
		dec(3, "beta", "b_hex", "resource", "cast", []string{"b_arrow", "b_hex"}),
	}
	return &model.Truth{
		GeneratorVersion: model.GeneratorVersion, Seed: seed,
		Rounds: []model.RoundTruth{
			{Round: 1, EffectiveSkills: []string{"red_bolt", "a_nova", "b_arrow"},
				Adjudications: adj[0:5],
				HP:            map[string]int64{"red": 180, "alpha": 145, "beta": 140},
				Resource:      map[string]int64{"red": 100, "alpha": 30, "beta": 60}},
			{Round: 2, EffectiveSkills: []string{"red_bolt", "a_execute", "a_team_ultra", "b_volley"},
				Adjudications: adj[5:10],
				HP:            map[string]int64{"red": 40, "alpha": 110, "beta": 140},
				Resource:      map[string]int64{"red": 100, "alpha": 10, "beta": 50}},
			{Round: 3, EffectiveSkills: []string{"red_bolt", "b_arrow", "b_hex"},
				Adjudications: adj[10:15],
				HP:            map[string]int64{"red": 0, "alpha": 75, "beta": 140},
				Resource:      map[string]int64{"red": 100, "alpha": 20, "beta": 40}},
		},
		FinalHP:       map[string]int64{"red": 0, "alpha": 75, "beta": 140},
		FinalResource: map[string]int64{"red": 100, "alpha": 20, "beta": 40},
		Verdict:       "win", WinnerTeam: "blue", Adjudications: adj,
	}
}

func dec(round int, unit, skill, cType, result string, set []string) model.Adjudication {
	return model.Adjudication{
		Round: round, UnitID: unit, SkillID: skill, ContentionType: cType,
		Result: result, ContendingSkills: append([]string(nil), set...),
	}
}
