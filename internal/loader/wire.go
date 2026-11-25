package loader

import "encoding/json"

const (
	FormatTickV1    = "tick-weight-v1"
	FormatChronoV2  = "chrono-swapped-v2"
	FormatCompactV3 = "compact-legacy-v3"
)

type Envelope struct {
	Format int `json:"format"`
}

type Wire1 struct {
	Format     int        `json:"format"`
	FormatName string     `json:"format_name"`
	Schema     string     `json:"schema"`
	BattleID   string     `json:"battle_id"`
	Seed       int64      `json:"seed"`
	TickMS     int64      `json:"tick_ms"`
	EpochMS    int64      `json:"epoch_ms"`
	StartRound int64      `json:"start_round"`
	Rounds     int        `json:"rounds"`
	EnemyHP    int        `json:"enemy_hp"`
	Units      []W1Unit   `json:"units"`
	Skills     []W1Skill  `json:"skills"`
	Timeline   []W1Round  `json:"timeline"`
	Lists      W1Lists    `json:"lists"`
	Reorders   []WReorder `json:"reorders"`
}

type W1Unit struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Priority  int    `json:"priority"`
	Recast    int    `json:"recast"`
	Slot      int    `json:"slot"`
	Energy    int    `json:"energy"`
	EnergyMax int    `json:"energy_max"`
	Regen     int    `json:"regen"`
}

type W1Skill struct {
	ID          string `json:"id"`
	Actor       string `json:"actor"`
	Cost        int    `json:"cost"`
	CastCD      int    `json:"cast_cd"`
	Power       int    `json:"power"`
	Conditional bool   `json:"conditional"`
}

type W1Round struct {
	Round   int64      `json:"round"`
	Actions []W1Action `json:"actions"`
}

type W1Action struct {
	Actor     string `json:"actor"`
	Technique string `json:"technique"`
	Flag      *int   `json:"flag"`
	Assert    *bool  `json:"flag_assert,omitempty"`
}

type W1Lists struct {
	Alpha []string `json:"alpha"`
	Beta  []string `json:"beta"`
}

type WReorder struct {
	Offset        int    `json:"offset"`
	InstructionID string `json:"instruction_id"`
	Actor         string `json:"actor"`
	AfterRound    int64  `json:"after_round"`
	NewSlot       int    `json:"new_slot"`
	Reason        string `json:"reason"`
}

type Wire2 struct {
	FormatNo          int           `json:"formatNo"`
	FormatName        string        `json:"formatName"`
	Schema            string        `json:"schema"`
	BattleID          string        `json:"battleId"`
	Seed              int64         `json:"seed"`
	TickMilliseconds  int64         `json:"tickMilliseconds"`
	EpochMilliseconds int64         `json:"epochMilliseconds"`
	StartRound        int64         `json:"startRound"`
	TotalRounds       int           `json:"totalRounds"`
	EnemyHP           int           `json:"enemyHp"`
	UnitRecords       []W2Unit      `json:"unitRecords"`
	SkillRecords      []W2Skill     `json:"skillRecords"`
	TimePoints        []W2TimePoint `json:"timePoints"`
	SequenceBox       W2Sequences   `json:"sequenceBox"`
	ReorderCommands   []WReorder    `json:"reorderCommands"`
}

type W2Unit struct {
	UnitID      string `json:"unitId"`
	DisplayName string `json:"displayName"`
	Priority    int    `json:"priority"`
	Recast      int    `json:"recast"`
	SlotIndex   int    `json:"slotIndex"`
	Energy      int    `json:"energy"`
	EnergyCap   int    `json:"energyCap"`
	Regen       int    `json:"regen"`
}

type W2Skill struct {
	SkillID     string `json:"skillId"`
	OwnerID     string `json:"ownerId"`
	EnergyCost  int    `json:"energyCost"`
	Cooldown    int    `json:"cooldown"`
	Potency     int    `json:"potency"`
	Conditional bool   `json:"conditional"`
}

type W2TimePoint struct {
	AtMilliseconds int64      `json:"atMilliseconds"`
	Commands       []W2Action `json:"commands"`
}

type W2Action struct {
	ActorID       string `json:"actorId"`
	TechniqueID   string `json:"techniqueId"`
	ConditionFlag *bool  `json:"conditionFlag"`
	ConditionBit  *int   `json:"conditionBit,omitempty"`
}

type W2Sequences struct {
	Alpha []string `json:"alpha"`
	Beta  []string `json:"beta"`
}

type Wire3 struct {
	Fmt       int        `json:"fmt"`
	Kind      string     `json:"kind"`
	Doc       string     `json:"doc"`
	ID        string     `json:"id"`
	Seed      int64      `json:"seed"`
	Tick      int64      `json:"tick"`
	Epoch     int64      `json:"epoch"`
	BaseRound int64      `json:"base_round"`
	Rounds    int        `json:"rounds"`
	Enemy     int        `json:"enemy"`
	Actors    []W3Actor  `json:"actors"`
	Techs     []W3Tech   `json:"techs"`
	Frames    []W3Frame  `json:"frames"`
	Lists     W3Lists    `json:"lists"`
	Orders    []WReorder `json:"orders"`
}

type W3Actor struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	W     int    `json:"w"`
	CD    int    `json:"cd"`
	Ord   int    `json:"ord"`
	Res   int    `json:"res"`
	Max   int    `json:"max"`
	Regen int    `json:"regen"`
}

type W3Tech struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Cost  int    `json:"cost"`
	CD    int    `json:"cd"`
	Pow   int    `json:"pow"`
	Cond  bool   `json:"cond"`
}

type W3Frame struct {
	Round int64    `json:"r"`
	Moves []W3Move `json:"moves"`
}

type W3Move struct {
	Actor string `json:"a"`
	Tech  string `json:"t"`
	Flag  string `json:"f"`
	Bit   *int   `json:"b,omitempty"`
}

type W3Lists struct {
	Skill []string `json:"skill_seq"`
	Actor []string `json:"actor_seq"`
}

func decodeRaw(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
