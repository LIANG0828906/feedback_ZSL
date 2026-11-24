package syncgame

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	SchemaVersion    = "1"
	AlgorithmVersion = "sync-v1"
)

var LatencyTierEnum = []string{"direct", "low", "medium", "high"}

type Sample struct {
	SchemaVersion    string        `json:"schema_version"`
	AlgorithmVersion string        `json:"algorithm_version"`
	Seed             uint64        `json:"seed"`
	TickRate         int           `json:"tick_rate"`
	RenderFPS        int           `json:"render_fps"`
	MaxTicks         int           `json:"max_ticks"`
	Arena            Arena         `json:"arena"`
	ResourceGoal     int           `json:"resource_goal"`
	Units            []UnitSeed    `json:"units"`
	Clients          []ClientSeed  `json:"clients"`
	LatencyTiers     []LatencyTier `json:"latency_tiers"`
	Intents          []TickIntent  `json:"intents"`
	Truth            Truth         `json:"truth"`
}

type Arena struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type UnitSeed struct {
	PlayerID     string     `json:"player_id"`
	DisplayName  string     `json:"display_name"`
	SpawnX       int        `json:"spawn_x"`
	SpawnY       int        `json:"spawn_y"`
	Direction    int        `json:"direction"`
	Health       int        `json:"health"`
	MaxHealth    int        `json:"max_health"`
	Resource     int        `json:"resource"`
	MoveSpeed    int        `json:"move_speed"`
	RespawnTicks int        `json:"respawn_ticks"`
	Skills       []SkillDef `json:"skills"`
}

type SkillDef struct {
	ID            string `json:"id"`
	Cost          int    `json:"cost"`
	CooldownTicks int    `json:"cooldown_ticks"`
	Range         int    `json:"range"`
	Damage        int    `json:"damage"`
	Heal          int    `json:"heal"`
}

type ClientSeed struct {
	ID          string `json:"id"`
	PlayerID    string `json:"player_id"`
	LatencyTier string `json:"latency_tier"`
}

type LatencyTier struct {
	ID                      string `json:"id"`
	ObservationDelayFrames  int    `json:"observation_delay_frames"`
	MaxInputLatencyFrames   int    `json:"max_input_latency_frames"`
	MaxCorrectionFrames     int    `json:"max_correction_frames"`
	MaxConsecutiveRollbacks int    `json:"max_consecutive_rollbacks"`
	MaxRollbackDepth        int    `json:"max_rollback_depth"`
}

type TickIntent struct {
	Tick   int      `json:"tick"`
	Inputs []Intent `json:"inputs"`
}

type Intent struct {
	PlayerID string `json:"player_id"`
	Kind     string `json:"kind"`
	DX       int    `json:"dx,omitempty"`
	DY       int    `json:"dy,omitempty"`
	Skill    string `json:"skill,omitempty"`
	TargetID string `json:"target_id,omitempty"`
	Item     string `json:"item,omitempty"`
}

const (
	IntentMove   = "move"
	IntentSkill  = "skill"
	IntentGather = "gather"
	IntentItem   = "item"
)

type WorldState struct {
	Tick       int         `json:"tick"`
	Units      []UnitState `json:"units"`
	Finished   bool        `json:"finished"`
	Winner     string      `json:"winner,omitempty"`
	LockedTick int         `json:"locked_tick,omitempty"`
}

type UnitState struct {
	PlayerID      string         `json:"player_id"`
	X             int            `json:"x"`
	Y             int            `json:"y"`
	Direction     int            `json:"direction"`
	Health        int            `json:"health"`
	MaxHealth     int            `json:"max_health"`
	Resource      int            `json:"resource"`
	Alive         bool           `json:"alive"`
	RespawnAt     int            `json:"respawn_at,omitempty"`
	Kills         int            `json:"kills"`
	SkillCooldown map[string]int `json:"skill_cooldown"`
}

type PlayerResult struct {
	PlayerID string `json:"player_id"`
	Health   int    `json:"health"`
	Resource int    `json:"resource"`
	Alive    bool   `json:"alive"`
	Kills    int    `json:"kills"`
}

type Settlement struct {
	LockedTick int            `json:"locked_tick"`
	Winner     string         `json:"winner,omitempty"`
	Reason     string         `json:"reason"`
	Players    []PlayerResult `json:"players"`
}

type Truth struct {
	KeyTicks      []int         `json:"key_ticks"`
	Authoritative []TruthTick   `json:"authoritative"`
	Clients       []ClientTruth `json:"clients"`
	Settlement    *Settlement   `json:"settlement,omitempty"`
	ContentHash   string        `json:"content_hash,omitempty"`
}

type TruthTick struct {
	Tick  int        `json:"tick"`
	State WorldState `json:"state"`
}

type ClientTruth struct {
	ClientID string         `json:"client_id"`
	TierID   string         `json:"tier_id"`
	Frames   []VisibleFrame `json:"frames"`
}

type VisibleFrame struct {
	Frame         int        `json:"frame"`
	Tick          int        `json:"tick"`
	Mode          string     `json:"mode"`
	Basis         string     `json:"basis"`
	AuthorityTick int        `json:"authority_tick"`
	Correction    int        `json:"correction"`
	Rollback      bool       `json:"rollback"`
	State         WorldState `json:"state"`
}

const (
	FrameModePredicted = "predicted"
	FrameModeAuthority = "authority"
)

func (s *Sample) Unit(playerID string) (UnitSeed, bool) {
	for _, u := range s.Units {
		if u.PlayerID == playerID {
			return u, true
		}
	}
	return UnitSeed{}, false
}

func (s *Sample) Tier(id string) (LatencyTier, bool) {
	for _, t := range s.LatencyTiers {
		if t.ID == id {
			return t, true
		}
	}
	return LatencyTier{}, false
}

func (s *Sample) Client(id string) (ClientSeed, bool) {
	for _, c := range s.Clients {
		if c.ID == id {
			return c, true
		}
	}
	return ClientSeed{}, false
}

// InitialState constructs the canonical initial world. Units are always sorted
// by player ID so map iteration can never influence a simulation.
func (s *Sample) InitialState() WorldState {
	units := make([]UnitState, 0, len(s.Units))
	for _, u := range s.Units {
		cd := make(map[string]int, len(u.Skills))
		for _, skill := range u.Skills {
			cd[skill.ID] = 0
		}
		units = append(units, UnitState{
			PlayerID:      u.PlayerID,
			X:             u.SpawnX,
			Y:             u.SpawnY,
			Direction:     u.Direction,
			Health:        u.Health,
			MaxHealth:     u.MaxHealth,
			Resource:      u.Resource,
			Alive:         u.Health > 0,
			SkillCooldown: cd,
		})
	}
	sort.Slice(units, func(i, j int) bool { return units[i].PlayerID < units[j].PlayerID })
	return WorldState{Units: units}
}

func (s *Sample) StreamFingerprint() string {
	return hashCanonical(struct {
		Seed    uint64       `json:"seed"`
		Intents []TickIntent `json:"intents"`
	}{Seed: s.Seed, Intents: s.Intents})
}

func (s *Sample) TruthFingerprint() string {
	copy := *s
	copy.Truth = Truth{}
	return hashCanonical(copy)
}

func hashCanonical(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("canonical JSON marshal failed: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashBytes(parts ...[]byte) string {
	h := sha256.New()
	var size [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(p)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func CloneWorld(in WorldState) WorldState {
	out := in
	out.Units = make([]UnitState, len(in.Units))
	for i, u := range in.Units {
		out.Units[i] = u
		out.Units[i].SkillCooldown = make(map[string]int, len(u.SkillCooldown))
		for k, v := range u.SkillCooldown {
			out.Units[i].SkillCooldown[k] = v
		}
	}
	return out
}
