package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"replaybench/internal/fixed"
)

const (
	SchemaVersion       = "replay-v1"
	AlgorithmVersion    = "alg-1.0.0"
	ParamsVersion       = "params-1.0.0"
	GeneratorVersion    = "gen-1.0.0"
	SQLiteSchemaVersion = "store-v1"
)

type FormatID int

const (
	FormatWeightCast    FormatID = 1
	FormatCooldownQueue FormatID = 2
	FormatTimestamp     FormatID = 3
)

func (f FormatID) Name() string {
	switch f {
	case FormatWeightCast:
		return "weight-cast-v1"
	case FormatCooldownQueue:
		return "cooldown-queue-v2"
	case FormatTimestamp:
		return "timestamp-v3"
	default:
		return "unknown"
	}
}

func (f FormatID) SourceVersion() string {
	return "rf" + hex.EncodeToString([]byte{byte(f)}) + "-" + SchemaVersion
}

type TimeBasis struct {
	Mode       string `json:"mode"`
	EpochMs    int64  `json:"epoch_ms"`
	MsPerRound int64  `json:"ms_per_round"`
	Rounding   string `json:"rounding"`
}

type Unit struct {
	ID              string  `json:"id"`
	Team            string  `json:"team"`
	MaxHP           fixed.P `json:"max_hp"`
	HP              fixed.P `json:"hp"`
	MaxResource     fixed.P `json:"max_resource"`
	Resource        fixed.P `json:"resource"`
	Weight          int64   `json:"weight"`
	QueuePosition   int     `json:"queue_position"`
	CooldownSkillID string  `json:"cooldown_skill_id,omitempty"`
	CooldownRemain  int64   `json:"cooldown_remaining,omitempty"`
}

type Skill struct {
	ID            string  `json:"id"`
	OwnerID       string  `json:"owner_id"`
	TargetID      string  `json:"target_id"`
	Priority      int     `json:"priority"`
	ResourceCost  fixed.P `json:"resource_cost"`
	Cooldown      int64   `json:"cooldown"`
	CooldownGroup string  `json:"cooldown_group,omitempty"`
	CooldownScope string  `json:"cooldown_scope,omitempty"`
	Damage        fixed.P `json:"damage"`
	Heal          fixed.P `json:"heal,omitempty"`
	GrantResource fixed.P `json:"grant_resource,omitempty"`
	ConditionFlag string  `json:"condition_flag,omitempty"`
}

type Directive struct {
	Offset       int      `json:"offset"`
	Round        int      `json:"round"`
	CastSequence []string `json:"cast_sequence"`
	ActorQueue   []string `json:"actor_queue"`
}

type Edit struct {
	Type      string   `json:"type"`
	Round     int      `json:"round"`
	UnitID    string   `json:"unit_id,omitempty"`
	Flag      string   `json:"flag,omitempty"`
	Value     int64    `json:"value,omitempty"`
	BoolValue bool     `json:"bool_value,omitempty"`
	Order     []string `json:"order,omitempty"`
}

type RuleSet struct {
	Weights           map[string]int64 `json:"weights"`
	CooldownReduction int64            `json:"cooldown_reduction"`
	ResourceRegen     fixed.P          `json:"resource_regen"`
	Flags             map[string]bool  `json:"flags"`
	Formation         []string         `json:"formation"`
	Precedence        [][2]string      `json:"precedence,omitempty"`
	ParamsVersion     string           `json:"params_version"`
}

type Replay struct {
	ID                  string      `json:"id"`
	Seed                int64       `json:"seed"`
	Format              FormatID    `json:"format"`
	SourceFormatVersion string      `json:"source_format_version"`
	SchemaVersion       string      `json:"schema_version"`
	Rounds              int         `json:"rounds"`
	Time                TimeBasis   `json:"time"`
	Units               []Unit      `json:"units"`
	Skills              []Skill     `json:"skills"`
	Directives          []Directive `json:"directives"`
	Rules               RuleSet     `json:"rules"`
	Edits               []Edit      `json:"edits,omitempty"`
	ContentHash         string      `json:"content_hash"`
}

type Adjudication struct {
	Round            int      `json:"round"`
	UnitID           string   `json:"unit_id"`
	SkillID          string   `json:"skill_id"`
	ContentionType   string   `json:"contention_type"`
	Result           string   `json:"result"`
	Reason           string   `json:"reason,omitempty"`
	ContendingSkills []string `json:"contending_skills,omitempty"`
	PriorityKey      string   `json:"priority_key,omitempty"`
}

type UnitSnapshot struct {
	UnitID         string           `json:"unit_id"`
	Team           string           `json:"team"`
	Alive          bool             `json:"alive"`
	HP             fixed.P          `json:"hp"`
	MaxHP          fixed.P          `json:"max_hp"`
	Resource       fixed.P          `json:"resource"`
	MaxResource    fixed.P          `json:"max_resource"`
	Cooldowns      map[string]int64 `json:"cooldowns"`
	GroupCooldowns map[string]int64 `json:"group_cooldowns"`
}

type RoundSnapshot struct {
	ReplayID         string           `json:"replay_id"`
	Round            int              `json:"round"`
	ConsumedOffset   int              `json:"consumed_offset"`
	States           []UnitSnapshot   `json:"states"`
	GlobalCooldowns  map[string]int64 `json:"global_cooldowns"`
	EffectiveSkills  []string         `json:"effective_skills"`
	Adjudications    []Adjudication   `json:"adjudications"`
	StateDigest      string           `json:"state_digest"`
	ContentHash      string           `json:"content_hash"`
	SourceFormat     FormatID         `json:"source_format"`
	SourceFormatVer  string           `json:"source_format_version"`
	ParamsVersion    string           `json:"params_version"`
	AlgorithmVersion string           `json:"algorithm_version"`
}

type Window struct {
	Index            int             `json:"index"`
	FirstRound       int             `json:"first_round"`
	LastRound        int             `json:"last_round"`
	NextOffset       int             `json:"next_offset"`
	StateDigest      string          `json:"state_digest"`
	ConsumedOffset   int             `json:"consumed_offset"`
	ContentHash      string          `json:"content_hash"`
	SourceFormatVer  string          `json:"source_format_version"`
	ParamsVersion    string          `json:"params_version"`
	AlgorithmVersion string          `json:"algorithm_version"`
	PrevDigest       string          `json:"prev_digest"`
	WindowDigest     string          `json:"window_digest"`
	Snapshots        []RoundSnapshot `json:"snapshots"`
}

type RoundTruth struct {
	Round           int              `json:"round"`
	EffectiveSkills []string         `json:"effective_skills"`
	Adjudications   []Adjudication   `json:"adjudications"`
	HP              map[string]int64 `json:"hp_whole"`
	Resource        map[string]int64 `json:"resource_whole"`
}

type Truth struct {
	GeneratorVersion string           `json:"generator_version"`
	Seed             int64            `json:"seed"`
	Rounds           []RoundTruth     `json:"rounds"`
	FinalHP          map[string]int64 `json:"final_hp_whole"`
	FinalResource    map[string]int64 `json:"final_resource_whole"`
	Verdict          string           `json:"verdict"`
	WinnerTeam       string           `json:"winner_team"`
	Adjudications    []Adjudication   `json:"adjudications"`
}

func SortedUnitIDs(units []Unit) []string {
	ids := make([]string, 0, len(units))
	for _, u := range units {
		ids = append(ids, u.ID)
	}
	sort.Strings(ids)
	return ids
}

func CanonicalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func SHA256Hex(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ContentHash(replay *Replay) string {
	copy := *replay
	copy.ContentHash = ""
	copy.Edits = append([]Edit(nil), replay.Edits...)
	return SHA256Hex(CanonicalJSON(copy))
}

func SnapshotDigest(s RoundSnapshot) string {
	copy := s
	copy.StateDigest = ""
	b := CanonicalJSON(copy)
	return SHA256Hex(b)
}

func (r *Replay) UnitByID(id string) (Unit, bool) {
	for _, u := range r.Units {
		if u.ID == id {
			return u, true
		}
	}
	return Unit{}, false
}

func (r *Replay) SkillByID(id string) (Skill, bool) {
	for _, s := range r.Skills {
		if s.ID == id {
			return s, true
		}
	}
	return Skill{}, false
}
