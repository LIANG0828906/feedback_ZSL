package tactical

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
)

// LoadScenarioFile reads and validates a deterministic local scenario.
func LoadScenarioFile(path string) (*Scenario, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: CodeSchema, Field: "scenario", Message: err.Error()}
	}
	return ParseScenario(payload)
}

// ParseScenario performs strict required-field checks before semantic
// validation. All failures carry E_SCHEMA and a dotted field path.
func ParseScenario(payload []byte) (*Scenario, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, schemaError("scenario", "invalid JSON: "+err.Error())
	}
	if root == nil {
		return nil, schemaError("scenario", "top-level value must be an object")
	}
	if err := requireFields(root, "", "schema_version", "seed", "map", "formation", "instructions", "ground_truth"); err != nil {
		return nil, err
	}
	if err := validateRawDocument(root); err != nil {
		return nil, err
	}
	var scenario Scenario
	if err := json.Unmarshal(payload, &scenario); err != nil {
		return nil, schemaError("scenario", "invalid field type: "+err.Error())
	}
	if err := ValidateScenario(&scenario); err != nil {
		return nil, err
	}
	return &scenario, nil
}

func validateRawDocument(root map[string]json.RawMessage) error {
	mapRaw, err := objectField(root, "map", "")
	if err != nil {
		return err
	}
	if err := requireFields(mapRaw, "map", "id", "width", "height", "cells", "blocked_regions"); err != nil {
		return err
	}
	cells, err := arrayField(mapRaw, "cells", "map")
	if err != nil {
		return err
	}
	for i, raw := range cells {
		obj, err := objectValue(raw, fmt.Sprintf("map.cells[%d]", i))
		if err != nil {
			return err
		}
		if err := requireFields(obj, fmt.Sprintf("map.cells[%d]", i), "cost", "terrain", "passable"); err != nil {
			return err
		}
	}
	regions, err := arrayField(mapRaw, "blocked_regions", "map")
	if err != nil {
		return err
	}
	for i, raw := range regions {
		obj, err := objectValue(raw, fmt.Sprintf("map.blocked_regions[%d]", i))
		if err != nil {
			return err
		}
		if err := requireFields(obj, fmt.Sprintf("map.blocked_regions[%d]", i), "id", "x", "y", "width", "height", "active"); err != nil {
			return err
		}
	}

	formationRaw, err := objectField(root, "formation", "")
	if err != nil {
		return err
	}
	if err := requireFields(formationRaw, "formation", "id", "slot_version", "slots", "members", "max_spacing_milli", "default_epsilon_milli", "pair_tolerance_multiplier"); err != nil {
		return err
	}
	slots, err := arrayField(formationRaw, "slots", "formation")
	if err != nil {
		return err
	}
	for i, raw := range slots {
		path := fmt.Sprintf("formation.slots[%d]", i)
		obj, err := objectValue(raw, path)
		if err != nil {
			return err
		}
		if err := requireFields(obj, path, "id", "anchor", "dx", "dy", "tolerance", "facing_milli"); err != nil {
			return err
		}
	}
	members, err := arrayField(formationRaw, "members", "formation")
	if err != nil {
		return err
	}
	for i, raw := range members {
		path := fmt.Sprintf("formation.members[%d]", i)
		obj, err := objectValue(raw, path)
		if err != nil {
			return err
		}
		if err := requireFields(obj, path, "id", "callsign", "group", "slot_id", "start", "end", "step_milli", "turn_rate_milli", "heuristic_weight_milli", "neighbors", "depends_on"); err != nil {
			return err
		}
		for _, pointField := range []string{"start", "end"} {
			pointRaw, err := objectField(obj, pointField, path)
			if err != nil {
				return err
			}
			if err := requireFields(pointRaw, path+"."+pointField, "x", "y"); err != nil {
				return err
			}
		}
	}

	instructions, err := arrayField(root, "instructions", "")
	if err != nil {
		return err
	}
	for i, raw := range instructions {
		path := fmt.Sprintf("instructions[%d]", i)
		obj, err := objectValue(raw, path)
		if err != nil {
			return err
		}
		if err := requireFields(obj, path, "tick", "member_id", "kind", "value"); err != nil {
			return err
		}
	}

	truthRaw, err := objectField(root, "ground_truth", "")
	if err != nil {
		return err
	}
	if err := requireFields(truthRaw, "ground_truth", "min_costs", "path_lengths", "corners", "formation_deviations"); err != nil {
		return err
	}
	minCosts, err := arrayField(truthRaw, "min_costs", "ground_truth")
	if err != nil {
		return err
	}
	for i, raw := range minCosts {
		path := fmt.Sprintf("ground_truth.min_costs[%d]", i)
		obj, err := objectValue(raw, path)
		if err != nil {
			return err
		}
		if err := requireFields(obj, path, "member_id", "from", "to", "cost"); err != nil {
			return err
		}
		for _, pointField := range []string{"from", "to"} {
			pointRaw, err := objectField(obj, pointField, path)
			if err != nil {
				return err
			}
			if err := requireFields(pointRaw, path+"."+pointField, "x", "y"); err != nil {
				return err
			}
		}
	}
	pathLengths, err := arrayField(truthRaw, "path_lengths", "ground_truth")
	if err != nil {
		return err
	}
	for i, raw := range pathLengths {
		path := fmt.Sprintf("ground_truth.path_lengths[%d]", i)
		obj, err := objectValue(raw, path)
		if err != nil {
			return err
		}
		if err := requireFields(obj, path, "member_id", "cost", "steps", "corners", "length_milli"); err != nil {
			return err
		}
	}
	corners, err := arrayField(truthRaw, "corners", "ground_truth")
	if err != nil {
		return err
	}
	for i, raw := range corners {
		path := fmt.Sprintf("ground_truth.corners[%d]", i)
		obj, err := objectValue(raw, path)
		if err != nil {
			return err
		}
		if err := requireFields(obj, path, "member_id", "grid", "world"); err != nil {
			return err
		}
		for _, pointField := range []string{"grid", "world"} {
			pointRaw, err := objectField(obj, pointField, path)
			if err != nil {
				return err
			}
			if err := requireFields(pointRaw, path+"."+pointField, "x", "y"); err != nil {
				return err
			}
		}
	}
	deviations, err := arrayField(truthRaw, "formation_deviations", "ground_truth")
	if err != nil {
		return err
	}
	for i, raw := range deviations {
		path := fmt.Sprintf("ground_truth.formation_deviations[%d]", i)
		obj, err := objectValue(raw, path)
		if err != nil {
			return err
		}
		if err := requireFields(obj, path, "tick", "member_id", "deviation_milli"); err != nil {
			return err
		}
	}
	return nil
}

func requireFields(obj map[string]json.RawMessage, prefix string, fields ...string) error {
	for _, field := range fields {
		if _, ok := obj[field]; !ok {
			path := field
			if prefix != "" {
				path = prefix + "." + field
			}
			return schemaError(path, "required field is missing")
		}
	}
	return nil
}

func objectField(obj map[string]json.RawMessage, field, prefix string) (map[string]json.RawMessage, error) {
	raw, ok := obj[field]
	if !ok {
		path := field
		if prefix != "" {
			path = prefix + "." + field
		}
		return nil, schemaError(path, "required object is missing")
	}
	return objectValue(raw, joinField(prefix, field))
}

func objectValue(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, schemaError(path, "must be an object")
	}
	return obj, nil
}

func arrayField(obj map[string]json.RawMessage, field, prefix string) ([]json.RawMessage, error) {
	raw, ok := obj[field]
	if !ok {
		return nil, schemaError(joinField(prefix, field), "required array is missing")
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, schemaError(joinField(prefix, field), "must be an array")
	}
	return values, nil
}

func joinField(prefix, field string) string {
	if prefix == "" {
		return field
	}
	return prefix + "." + field
}

// ValidateScenario checks structural invariants and returns E_SCHEMA with
// precise field context.
func ValidateScenario(s *Scenario) error {
	if s.SchemaVersion != 1 {
		return schemaError("schema_version", fmt.Sprintf("unsupported schema version %d", s.SchemaVersion))
	}
	if err := validateMap(&s.Map); err != nil {
		return err
	}
	if err := validateFormation(&s.Formation, s.Map.Width, s.Map.Height); err != nil {
		return err
	}
	if err := validateInstructions(s); err != nil {
		return err
	}
	return validateTruth(s)
}

func validateMap(def *MapDef) error {
	if strings.TrimSpace(def.ID) == "" {
		return schemaError("map.id", "map id must not be empty")
	}
	if def.Width <= 0 || def.Height <= 0 || def.Width > 4096 || def.Height > 4096 {
		return schemaError("map.width", "width and height must be in 1..4096")
	}
	if len(def.Cells) != def.Width*def.Height {
		return schemaError("map.cells", fmt.Sprintf("expected %d cells, got %d", def.Width*def.Height, len(def.Cells)))
	}
	for i, cell := range def.Cells {
		field := fmt.Sprintf("map.cells[%d]", i)
		if strings.TrimSpace(cell.Terrain) == "" {
			return schemaError(field+".terrain", "terrain classification must not be empty")
		}
		if cell.Cost < 0 || cell.Cost > MaxCellCost {
			return schemaError(field+".cost", fmt.Sprintf("cost must be a non-negative integer no greater than %d", MaxCellCost))
		}
		if cell.Passable {
			if cell.Cost <= 0 {
				return schemaError(field+".cost", "passable cells require a positive integer cost")
			}
			if cell.BlockedBy != "" {
				return schemaError(field+".blocked_by", "passable cells must not carry a blocking marker")
			}
		} else if cell.BlockedBy == "" && !isExplicitImpassableTerrain(cell.Terrain) {
			return schemaError(field+".passable", "impassable cell needs blocked_by or an explicit impassable terrain")
		}
	}
	regionIDs := make(map[string]struct{}, len(def.BlockedRegions))
	for i, region := range def.BlockedRegions {
		field := fmt.Sprintf("map.blocked_regions[%d]", i)
		if strings.TrimSpace(region.ID) == "" {
			return schemaError(field+".id", "region id must not be empty")
		}
		if _, exists := regionIDs[region.ID]; exists {
			return schemaError(field+".id", "duplicate blocked region id "+region.ID)
		}
		regionIDs[region.ID] = struct{}{}
		if region.Width <= 0 || region.Height <= 0 {
			return schemaError(field+".width", "region width and height must be positive")
		}
		if region.X < 0 || region.Y < 0 || region.X+region.Width > def.Width || region.Y+region.Height > def.Height {
			return schemaError(field+".x", "blocked region exceeds map bounds")
		}
	}
	return nil
}

func isExplicitImpassableTerrain(terrain string) bool {
	switch strings.ToLower(strings.TrimSpace(terrain)) {
	case "impassable", "blocked", "void", "wall":
		return true
	default:
		return false
	}
}

func validateFormation(def *FormationDef, width, height int) error {
	if strings.TrimSpace(def.ID) == "" {
		return schemaError("formation.id", "formation id must not be empty")
	}
	if strings.TrimSpace(def.SlotVersion) == "" {
		return schemaError("formation.slot_version", "slot version must not be empty")
	}
	if def.MaxSpacingMilli <= 0 {
		return schemaError("formation.max_spacing_milli", "maximum spacing must be positive")
	}
	if def.DefaultEpsilon < 0 {
		return schemaError("formation.default_epsilon_milli", "default epsilon must be non-negative")
	}
	if def.PairToleranceMult < 1 {
		return schemaError("formation.pair_tolerance_multiplier", "pair tolerance multiplier must be at least 1")
	}
	if len(def.Members) == 0 {
		return schemaError("formation.members", "at least one member is required")
	}
	members := make(map[string]Member, len(def.Members))
	for i, member := range def.Members {
		field := fmt.Sprintf("formation.members[%d]", i)
		if strings.TrimSpace(member.ID) == "" {
			return schemaError(field+".id", "member id must not be empty")
		}
		if _, exists := members[member.ID]; exists {
			return schemaError(field+".id", "duplicate member id "+member.ID)
		}
		if strings.TrimSpace(member.Group) == "" {
			return schemaError(field+".group", "group must not be empty")
		}
		if !member.Start.InBounds(width, height) {
			return schemaError(field+".start", fmt.Sprintf("start cell (%d,%d) is outside bounds", member.Start.X, member.Start.Y))
		}
		if !member.End.InBounds(width, height) {
			return schemaError(field+".end", fmt.Sprintf("end cell (%d,%d) is outside bounds", member.End.X, member.End.Y))
		}
		if member.Neighbors != 4 && member.Neighbors != 8 {
			return schemaError(field+".neighbors", "neighbors must be 4 or 8")
		}
		members[member.ID] = member
	}

	slots := make(map[string]Slot, len(def.Slots))
	for i, slot := range def.Slots {
		field := fmt.Sprintf("formation.slots[%d]", i)
		if strings.TrimSpace(slot.ID) == "" {
			return schemaError(field+".id", "slot id must not be empty")
		}
		if _, exists := slots[slot.ID]; exists {
			return schemaError(field+".id", "duplicate slot id "+slot.ID)
		}
		if _, ok := members[slot.Anchor]; !ok {
			return schemaError(field+".anchor", "slot anchor references unknown member "+slot.Anchor)
		}
		dx, dxOK := MilliFloat(slot.DX)
		dy, dyOK := MilliFloat(slot.DY)
		if !dxOK {
			return schemaError(field+".dx", "slot relative x offset must be finite")
		}
		if !dyOK {
			return schemaError(field+".dy", "slot relative y offset must be finite")
		}
		if absInt64(dx) >= int64(width)*CoordinateScale || absInt64(dy) >= int64(height)*CoordinateScale {
			return schemaError(field+".dx", "slot relative offset is outside the map bounds")
		}
		if _, ok := MilliFloat(slot.Tolerance); !ok || slot.Tolerance < 0 {
			return schemaError(field+".tolerance", "slot tolerance must be finite and non-negative")
		}
		if _, ok := MilliFloat(slot.Tolerance * float64(def.PairToleranceMult)); !ok {
			return schemaError(field+".tolerance", "slot tolerance multiplier overflows fixed-point range")
		}
		anchor := members[slot.Anchor]
		if !anchorPointInBounds(anchor.Start, slot, width, height) {
			return schemaError(field+".dx", "slot offset places anchor start outside the map")
		}
		if !anchorPointInBounds(anchor.End, slot, width, height) {
			return schemaError(field+".dy", "slot offset places anchor end outside the map")
		}
		slots[slot.ID] = slot
	}

	for i, member := range def.Members {
		field := fmt.Sprintf("formation.members[%d]", i)
		if _, ok := slots[member.SlotID]; !ok {
			return schemaError(field+".slot_id", "unknown formation slot "+member.SlotID)
		}
		for _, dependency := range member.DependsOn {
			if dependency == member.ID {
				return schemaError(field+".depends_on", "member must not depend on itself")
			}
			if _, ok := members[dependency]; !ok {
				return schemaError(field+".depends_on", "unknown dependency member "+dependency)
			}
		}
	}
	if err := validateDependencyAcyclic(def.Members); err != nil {
		return err
	}
	return nil
}

func anchorPointInBounds(anchor GridPoint, slot Slot, width, height int) bool {
	dx, _ := MilliFloat(slot.DX)
	dy, _ := MilliFloat(slot.DY)
	x := int64(anchor.X)*CoordinateScale + dx
	y := int64(anchor.Y)*CoordinateScale + dy
	return x >= 0 && y >= 0 && x <= int64(width-1)*CoordinateScale && y <= int64(height-1)*CoordinateScale
}

func validateDependencyAcyclic(members []Member) error {
	byID := make(map[string]Member, len(members))
	for _, member := range members {
		byID[member.ID] = member
	}
	state := make(map[string]uint8, len(members))
	var visit func(string, map[string]bool) error
	visit = func(id string, stack map[string]bool) error {
		if stack[id] {
			return schemaError("formation.members.depends_on", "dependency cycle includes member "+id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		stack[id] = true
		for _, dep := range byID[id].DependsOn {
			if err := visit(dep, stack); err != nil {
				return err
			}
		}
		delete(stack, id)
		state[id] = 2
		return nil
	}
	for _, member := range members {
		if err := visit(member.ID, make(map[string]bool)); err != nil {
			return err
		}
	}
	return nil
}

func validateInstructions(s *Scenario) error {
	lastTick := -1
	for i, instruction := range s.Instructions {
		field := fmt.Sprintf("instructions[%d]", i)
		if instruction.Tick < 0 {
			return schemaError(field+".tick", "tick must be non-negative")
		}
		if instruction.Tick < lastTick {
			return schemaError(field+".tick", "instructions must be ordered by logical tick")
		}
		lastTick = instruction.Tick
		if _, ok := MemberByID(*s, instruction.MemberID); !ok {
			return schemaError(field+".member_id", "unknown member "+instruction.MemberID)
		}
		if strings.TrimSpace(instruction.Kind) == "" {
			return schemaError(field+".kind", "instruction kind must not be empty")
		}
	}
	return nil
}

func validateTruth(s *Scenario) error {
	for i, truth := range s.GroundTruth.MinCosts {
		if _, ok := MemberByID(*s, truth.MemberID); !ok {
			return schemaError(fmt.Sprintf("ground_truth.min_costs[%d].member_id", i), "unknown member "+truth.MemberID)
		}
		if !truth.From.InBounds(s.Map.Width, s.Map.Height) || !truth.To.InBounds(s.Map.Width, s.Map.Height) {
			return schemaError(fmt.Sprintf("ground_truth.min_costs[%d]", i), "truth probe is outside map bounds")
		}
	}
	for i, truth := range s.GroundTruth.PathLengths {
		if _, ok := MemberByID(*s, truth.MemberID); !ok {
			return schemaError(fmt.Sprintf("ground_truth.path_lengths[%d].member_id", i), "unknown member "+truth.MemberID)
		}
	}
	for i, truth := range s.GroundTruth.Corners {
		if _, ok := MemberByID(*s, truth.MemberID); !ok {
			return schemaError(fmt.Sprintf("ground_truth.corners[%d].member_id", i), "unknown member "+truth.MemberID)
		}
	}
	for i, truth := range s.GroundTruth.FormationDeviations {
		if _, ok := MemberByID(*s, truth.MemberID); !ok {
			return schemaError(fmt.Sprintf("ground_truth.formation_deviations[%d].member_id", i), "unknown member "+truth.MemberID)
		}
	}
	return nil
}

func absInt64(v int64) int64 {
	if v < 0 {
		if v == math.MinInt64 {
			return math.MaxInt64
		}
		return -v
	}
	return v
}
