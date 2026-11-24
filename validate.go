package syncgame

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

var supportedIntentKinds = map[string]bool{
	IntentMove:   true,
	IntentSkill:  true,
	IntentGather: true,
	IntentItem:   true,
}

func ParseSample(data []byte) (*Sample, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, tickErr(0, "*", "sample", "sample is empty")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, tickErr(0, "*", "sample", "invalid JSON: "+err.Error())
	}
	required := []string{
		"schema_version", "algorithm_version", "seed", "tick_rate", "render_fps",
		"max_ticks", "arena", "resource_goal", "units", "clients", "latency_tiers",
		"intents", "truth",
	}
	for _, key := range required {
		if _, ok := raw[key]; !ok {
			return nil, tickErr(0, "*", key, "required field is missing")
		}
	}
	if err := validateRawStructure(raw); err != nil {
		return nil, err
	}

	var sample Sample
	if err := json.Unmarshal(data, &sample); err != nil {
		return nil, tickErr(0, "*", "sample", "invalid sample: "+err.Error())
	}
	if err := ValidateSample(&sample); err != nil {
		return nil, err
	}
	return &sample, nil
}

func validateRawStructure(raw map[string]json.RawMessage) error {
	var units []map[string]json.RawMessage
	if err := json.Unmarshal(raw["units"], &units); err != nil {
		return tickErr(0, "*", "units", "units must be an array of objects")
	}
	unitFields := []string{
		"player_id", "display_name", "spawn_x", "spawn_y", "direction", "health",
		"max_health", "resource", "move_speed", "respawn_ticks", "skills",
	}
	for _, unit := range units {
		playerID := rawString(unit["player_id"])
		for _, field := range unitFields {
			if _, ok := unit[field]; !ok {
				return tickErr(0, playerID, "units."+field, "required field is missing")
			}
		}
		var skills []map[string]json.RawMessage
		if err := json.Unmarshal(unit["skills"], &skills); err != nil {
			return tickErr(0, playerID, "units.skills", "skills must be an array")
		}
		for _, skill := range skills {
			for _, field := range []string{"id", "cost", "cooldown_ticks", "range", "damage", "heal"} {
				if _, ok := skill[field]; !ok {
					return tickErr(0, playerID, "units.skills."+field, "required field is missing")
				}
			}
		}
	}

	var clients []map[string]json.RawMessage
	if err := json.Unmarshal(raw["clients"], &clients); err != nil {
		return tickErr(0, "*", "clients", "clients must be an array")
	}
	for _, client := range clients {
		for _, field := range []string{"id", "player_id", "latency_tier"} {
			if _, ok := client[field]; !ok {
				return tickErr(0, rawString(client["player_id"]), "clients."+field, "required field is missing")
			}
		}
	}

	var tiers []map[string]json.RawMessage
	if err := json.Unmarshal(raw["latency_tiers"], &tiers); err != nil {
		return tickErr(0, "*", "latency_tiers", "latency tiers must be an array")
	}
	for _, tier := range tiers {
		for _, field := range []string{
			"id", "observation_delay_frames", "max_input_latency_frames",
			"max_correction_frames", "max_consecutive_rollbacks", "max_rollback_depth",
		} {
			if _, ok := tier[field]; !ok {
				return tickErr(0, "*", "latency_tiers."+field, "required field is missing")
			}
		}
	}

	var intents []map[string]json.RawMessage
	if err := json.Unmarshal(raw["intents"], &intents); err != nil {
		return tickErr(0, "*", "intents", "intents must be an array")
	}
	for i, tickIntent := range intents {
		tick := i
		if tickRaw, ok := tickIntent["tick"]; ok {
			_ = json.Unmarshal(tickRaw, &tick)
		} else {
			return tickErr(i, "*", "intents.tick", "required field is missing")
		}
		inputRaw, ok := tickIntent["inputs"]
		if !ok {
			return tickErr(tick, "*", "intents.inputs", "required field is missing")
		}
		var inputs []map[string]json.RawMessage
		if err := json.Unmarshal(inputRaw, &inputs); err != nil {
			return tickErr(tick, "*", "intents.inputs", "inputs must be an array")
		}
		for _, input := range inputs {
			playerID := rawString(input["player_id"])
			for _, field := range []string{"player_id", "kind"} {
				if _, ok := input[field]; !ok {
					return tickErr(tick, playerID, "intents.inputs."+field, "required field is missing")
				}
			}
		}
	}

	var truth map[string]json.RawMessage
	if err := json.Unmarshal(raw["truth"], &truth); err != nil {
		return tickErr(0, "*", "truth", "truth must be an object")
	}
	for _, field := range []string{"key_ticks", "authoritative", "clients", "settlement"} {
		if _, ok := truth[field]; !ok {
			return tickErr(0, "*", "truth."+field, "required field is missing")
		}
	}
	return nil
}

func rawString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 {
		return ""
	}
	_ = json.Unmarshal(raw, &value)
	return value
}

func ValidateSample(s *Sample) error {
	if s == nil {
		return tickErr(0, "*", "sample", "sample is nil")
	}
	if s.SchemaVersion != SchemaVersion {
		return tickErr(0, "*", "schema_version", fmt.Sprintf("unsupported schema version %q", s.SchemaVersion))
	}
	if s.AlgorithmVersion == "" {
		return tickErr(0, "*", "algorithm_version", "algorithm version is required")
	}
	if s.TickRate <= 0 {
		return tickErr(0, "*", "tick_rate", "tick rate must be positive")
	}
	if s.RenderFPS <= 0 {
		return tickErr(0, "*", "render_fps", "render FPS must be positive")
	}
	if s.MaxTicks <= 0 || s.MaxTicks > 100000 {
		return tickErr(0, "*", "max_ticks", "max ticks must be in [1,100000]")
	}
	if s.Arena.Width <= 0 || s.Arena.Height <= 0 {
		return tickErr(0, "*", "arena", "arena dimensions must be positive")
	}
	if s.ResourceGoal <= 0 {
		return tickErr(0, "*", "resource_goal", "resource goal must be positive")
	}
	if len(s.Units) < 2 {
		return tickErr(0, "*", "units", "at least two units are required")
	}

	playerIDs := make(map[string]bool, len(s.Units))
	for _, u := range s.Units {
		if u.PlayerID == "" {
			return tickErr(0, "*", "units.player_id", "unit player ID is required")
		}
		if playerIDs[u.PlayerID] {
			return tickErr(0, u.PlayerID, "units.player_id", "duplicate player ID")
		}
		playerIDs[u.PlayerID] = true
		if err := validateUnitNumbers(u); err != nil {
			return err
		}
	}

	tiers := make(map[string]LatencyTier, len(s.LatencyTiers))
	for _, tier := range s.LatencyTiers {
		if !isDeclaredTier(tier.ID) {
			return tickErr(0, "*", "latency_tiers.id", fmt.Sprintf("latency tier %q is not in the declared enum", tier.ID))
		}
		if _, exists := tiers[tier.ID]; exists {
			return tickErr(0, "*", "latency_tiers.id", fmt.Sprintf("duplicate latency tier %q", tier.ID))
		}
		if tier.ObservationDelayFrames < 0 {
			return tickErr(0, "*", "latency_tiers.observation_delay_frames", "observation delay must be non-negative")
		}
		if tier.MaxInputLatencyFrames <= 0 || tier.MaxCorrectionFrames <= 0 ||
			tier.MaxConsecutiveRollbacks <= 0 || tier.MaxRollbackDepth <= 0 {
			return tickErr(0, "*", "latency_tiers", fmt.Sprintf("latency tier %q has non-positive bounds", tier.ID))
		}
		tiers[tier.ID] = tier
	}
	if len(tiers) == 0 {
		return tickErr(0, "*", "latency_tiers", "at least one latency tier is required")
	}

	clientIDs := make(map[string]bool, len(s.Clients))
	for _, c := range s.Clients {
		if c.ID == "" {
			return tickErr(0, "*", "clients.id", "client ID is required")
		}
		if clientIDs[c.ID] {
			return tickErr(0, "*", "clients.id", fmt.Sprintf("duplicate client ID %q", c.ID))
		}
		clientIDs[c.ID] = true
		if !playerIDs[c.PlayerID] {
			return tickErr(0, c.PlayerID, "clients.player_id", "client references an unknown player")
		}
		if _, ok := tiers[c.LatencyTier]; !ok {
			return tickErr(0, c.PlayerID, "clients.latency_tier", "client references an undeclared latency tier")
		}
	}
	if len(s.Clients) == 0 {
		return tickErr(0, "*", "clients", "at least one client is required")
	}

	if len(s.Intents) == 0 {
		return tickErr(0, "*", "intents", "intent stream is empty")
	}
	if len(s.Intents) > s.MaxTicks {
		return tickErr(s.MaxTicks, "*", "intents", "intent stream is longer than max ticks")
	}
	for i, ti := range s.Intents {
		if ti.Tick != i {
			player := "*"
			if len(ti.Inputs) > 0 && ti.Inputs[0].PlayerID != "" {
				player = ti.Inputs[0].PlayerID
			}
			return tickErr(ti.Tick, player, "intents.tick",
				fmt.Sprintf("intent ticks must start at 0 and be contiguous; expected %d, got %d", i, ti.Tick))
		}
		seenAction := make(map[string]bool, len(ti.Inputs))
		for _, in := range ti.Inputs {
			if in.PlayerID == "" {
				return tickErr(ti.Tick, "*", "intents.inputs.player_id", "intent player ID is required")
			}
			if !playerIDs[in.PlayerID] {
				return tickErr(ti.Tick, in.PlayerID, "intents.inputs.player_id", "intent references an unknown player")
			}
			if !supportedIntentKinds[in.Kind] {
				return tickErr(ti.Tick, in.PlayerID, "intents.inputs.kind", fmt.Sprintf("unsupported intent kind %q", in.Kind))
			}
			if seenAction[in.PlayerID] {
				return tickErr(ti.Tick, in.PlayerID, "intents.inputs", "at most one intent per player per tick is supported")
			}
			seenAction[in.PlayerID] = true
			if in.Kind == IntentSkill && in.Skill == "" {
				return tickErr(ti.Tick, in.PlayerID, "intents.inputs.skill", "skill ID is required")
			}
			if in.Kind == IntentItem && in.Item == "" {
				return tickErr(ti.Tick, in.PlayerID, "intents.inputs.item", "item ID is required")
			}
			if in.Kind == IntentSkill && in.TargetID != "" && !playerIDs[in.TargetID] {
				return tickErr(ti.Tick, in.PlayerID, "intents.inputs.target_id", "skill references an unknown target")
			}
		}
	}
	return nil
}

func validateUnitNumbers(u UnitSeed) error {
	if u.MaxHealth <= 0 {
		return tickErr(0, u.PlayerID, "units.max_health", "max health must be positive")
	}
	if u.Health < 0 || u.Health > u.MaxHealth {
		return tickErr(0, u.PlayerID, "units.health", "health must be within [0,max_health]")
	}
	if u.MoveSpeed < 0 || u.MoveSpeed > 10000 {
		return tickErr(0, u.PlayerID, "units.move_speed", "move speed is outside the finite supported range")
	}
	if u.Resource < 0 || u.Resource > 1000000 {
		return tickErr(0, u.PlayerID, "units.resource", "resource is outside the finite supported range")
	}
	if u.RespawnTicks < 0 || u.RespawnTicks > 10000 {
		return tickErr(0, u.PlayerID, "units.respawn_ticks", "respawn ticks is outside the supported range")
	}
	if u.SpawnX < 0 || u.SpawnY < 0 {
		return tickErr(0, u.PlayerID, "units.spawn", "spawn coordinates must be non-negative")
	}
	seen := make(map[string]bool, len(u.Skills))
	for _, skill := range u.Skills {
		if skill.ID == "" {
			return tickErr(0, u.PlayerID, "units.skills.id", "skill ID is required")
		}
		if seen[skill.ID] {
			return tickErr(0, u.PlayerID, "units.skills.id", fmt.Sprintf("duplicate skill %q", skill.ID))
		}
		seen[skill.ID] = true
		if skill.Cost < 0 || skill.CooldownTicks < 0 || skill.Range < 0 ||
			skill.Damage < 0 || skill.Heal < 0 {
			return tickErr(0, u.PlayerID, "units.skills", fmt.Sprintf("skill %q has a negative field", skill.ID))
		}
	}
	return nil
}

func isDeclaredTier(id string) bool {
	for _, candidate := range LatencyTierEnum {
		if candidate == id {
			return true
		}
	}
	return false
}

// SortedPlayerIDs is used by every deterministic phase.
func SortedPlayerIDs(s *Sample) []string {
	ids := make([]string, 0, len(s.Units))
	for _, u := range s.Units {
		ids = append(ids, u.PlayerID)
	}
	sort.Strings(ids)
	return ids
}
