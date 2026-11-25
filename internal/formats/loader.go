package formats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"replayaudit/internal/errs"
	"replayaudit/internal/model"
	"replayaudit/internal/timeconv"
)

var validKinds = map[string]bool{"damage": true, "heal": true, "gain": true, "spend": true}

// LoadFile reads and decodes one distribution-format file.
func LoadFile(path string, params model.Params) (*model.Replay, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, errs.New(errs.EFormat, path, "", "read failed: %v", err)
	}
	r, f, err := Decode(data, params)
	if err != nil {
		return nil, f, err
	}
	return r, f, nil
}

// Decode dispatches on the format discriminator and normalizes one format.
func Decode(data []byte, params model.Params) (*model.Replay, int, error) {
	var peek PeekHeader
	if err := json.Unmarshal(data, &peek); err != nil {
		return nil, 0, errs.New(errs.EFormat, "$", "", "invalid JSON envelope: %v", err)
	}
	if peek.Format == nil {
		return nil, 0, errs.New(errs.ESchema, "format", "", "missing required field")
	}
	f := *peek.Format
	switch f {
	case 1:
		var d Format1
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, f, errs.New(errs.EFormat, "$", "", "invalid format-1 document: %v", err)
		}
		r, err := decode1(&d, params)
		return r, f, err
	case 2:
		var d Format2
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, f, errs.New(errs.EFormat, "$", "", "invalid format-2 document: %v", err)
		}
		r, err := decode2(&d, params)
		return r, f, err
	case 3:
		var d Format3
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, f, errs.New(errs.EFormat, "$", "", "invalid format-3 document: %v", err)
		}
		r, err := decode3(&d, params)
		return r, f, err
	default:
		return nil, f, errs.New(errs.EFormat, "format", "", "unknown format number %d", f)
	}
}

func baseReplay(id string, entities []model.EntitySpec) (*model.Replay, error) {
	if id == "" {
		return nil, errs.New(errs.ESchema, "replay_id", "", "missing required field")
	}
	if len(entities) == 0 {
		return nil, errs.New(errs.ESchema, "entities", "", "at least one entity is required")
	}
	seen := map[string]bool{}
	for i, en := range entities {
		p := fmt.Sprintf("entities[%d]", i)
		if en.ID == "" {
			return nil, errs.New(errs.ESchema, p+".id", "", "missing required field")
		}
		if seen[en.ID] {
			return nil, errs.New(errs.ESchema, p+".id", "", "duplicate entity id %q", en.ID)
		}
		seen[en.ID] = true
		if en.MaxHP <= 0 {
			return nil, errs.New(errs.ESchema, p+".max_hp", en.ID, "must be positive")
		}
		if en.MaxMP < 0 {
			return nil, errs.New(errs.ESchema, p+".max_mp", en.ID, "must be non-negative")
		}
	}
	return &model.Replay{ReplayID: id, Entities: append([]model.EntitySpec(nil), entities...)}, nil
}

func checkEventCommon(id, kind string, amount *int64, idx int) error {
	p := fmt.Sprintf("events[%d]", idx)
	if id == "" {
		return errs.New(errs.ESchema, p+".id", "", "missing required field")
	}
	if kind == "" {
		return errs.New(errs.ESchema, p+".kind", id, "missing required field")
	}
	if !validKinds[kind] {
		return errs.New(errs.ESchema, p+".kind", id, "unknown event kind %q", kind)
	}
	if amount == nil {
		return errs.New(errs.ESchema, p+".amount", id, "missing required field")
	}
	if *amount < 0 {
		return errs.New(errs.ESchema, p+".amount", id, "amount must be non-negative")
	}
	return nil
}

func checkSequences(e *model.Event, pathPrefix string) error {
	switch e.Kind {
	case "damage", "heal":
		if len(e.Targets) == 0 {
			return errs.New(errs.ESchema, pathPrefix+".targets", e.ID, "%s event requires target sequence", e.Kind)
		}
	case "gain", "spend":
		if len(e.Casters) == 0 {
			return errs.New(errs.ESchema, pathPrefix+".casters", e.ID, "%s event requires caster/resource sequence", e.Kind)
		}
	}
	return nil
}

func condFromDTO(flag string, want *bool, idx int, id string) (*model.Cond, error) {
	if flag == "" {
		return nil, nil
	}
	if want == nil {
		return nil, errs.New(errs.ESchema, fmt.Sprintf("events[%d].cond_want", idx), id,
			"cond_flag %q requires cond_want", flag)
	}
	return &model.Cond{Flag: flag, Want: *want}, nil
}

func flagsFromDTO(flat map[string]bool, defs []FlagDefDTO) []model.FlagDef {
	out := make([]model.FlagDef, 0, len(flat)+len(defs))
	for name, v := range flat {
		out = append(out, model.FlagDef{ID: "flagdef:" + name, Name: name, Value: v, Priority: 0})
	}
	for _, d := range defs {
		id := d.ID
		if id == "" {
			id = "flagdef:" + d.Name
		}
		out = append(out, model.FlagDef{ID: id, Name: d.Name, Value: d.Value, Priority: d.Priority})
	}
	return out
}

func decode1(d *Format1, params model.Params) (*model.Replay, error) {
	if d.Format != 1 {
		return nil, errs.New(errs.EFormat, "format", "", "discriminator mismatch: %d", d.Format)
	}
	if d.MsPerTurn > 0 {
		params.MsPerTurn = d.MsPerTurn
	}
	r, err := baseReplay(d.ReplayID, d.Entities)
	if err != nil {
		return nil, err
	}
	r.Flags = flagsFromDTO(d.Flags, d.FlagDefs)
	for i, g := range d.MutexGroups {
		id := g.Group
		if id == "" {
			id = fmt.Sprintf("mutex-%d", i)
		}
		r.Mutex = append(r.Mutex, model.MutexGroup{ID: id, EventIDs: append([]string(nil), g.EventIDs...)})
	}
	seen := map[string]bool{}
	for i, ev := range d.Events {
		if err := checkEventCommon(ev.ID, ev.Kind, ev.Amount, i); err != nil {
			return nil, err
		}
		if seen[ev.ID] {
			return nil, errs.New(errs.ESchema, fmt.Sprintf("events[%d].id", i), ev.ID, "duplicate event id")
		}
		seen[ev.ID] = true
		if ev.At == nil {
			return nil, errs.New(errs.ESchema, fmt.Sprintf("events[%d].at", i), ev.ID, "missing required field (turn serial)")
		}
		at, err := timeconv.ToInternal(*ev.At, timeconv.BasisTurn, params, ev.ID)
		if err != nil {
			return nil, err
		}
		cond, err := condFromDTO(ev.CondFlag, ev.CondWant, i, ev.ID)
		if err != nil {
			return nil, err
		}
		prio := 0
		if ev.Priority != nil {
			prio = *ev.Priority
		}
		var cd int64
		if ev.CooldownMS != nil {
			cd = *ev.CooldownMS
		}
		ne := &model.Event{
			ID: ev.ID, Kind: ev.Kind, At: at,
			Casters: append([]string(nil), ev.Chain...), // chain = CASTERS in format 1
			Targets: append([]string(nil), ev.Targets...),
			Amount:  *ev.Amount, Skill: ev.Skill, CooldownUnits: cd,
			Cond: cond, Priority: prio, Mandatory: ev.Mandatory, Fault: ev.Fault,
			SourceFormats: []int{1},
		}
		if err := checkSequences(ne, fmt.Sprintf("events[%d]", i)); err != nil {
			return nil, err
		}
		r.Events = append(r.Events, ne)
	}
	return r, nil
}

func decode2(d *Format2, params model.Params) (*model.Replay, error) {
	if d.Format != 2 {
		return nil, errs.New(errs.EFormat, "format", "", "discriminator mismatch: %d", d.Format)
	}
	r, err := baseReplay(d.ReplayID, d.Entities)
	if err != nil {
		return nil, err
	}
	for i, t := range d.SyncPoints {
		r.Sync = append(r.Sync, model.SyncPoint{ID: fmt.Sprintf("sync-f2-%d", i+1), At: t})
	}
	seen := map[string]bool{}
	for i, ev := range d.Events {
		if err := checkEventCommon(ev.ID, ev.Kind, ev.Amount, i); err != nil {
			return nil, err
		}
		if seen[ev.ID] {
			return nil, errs.New(errs.ESchema, fmt.Sprintf("events[%d].id", i), ev.ID, "duplicate event id")
		}
		seen[ev.ID] = true
		if ev.At == nil {
			return nil, errs.New(errs.ESchema, fmt.Sprintf("events[%d].at", i), ev.ID, "missing required field (ms timestamp)")
		}
		at, err := timeconv.ToInternal(*ev.At, timeconv.BasisMS, params, ev.ID)
		if err != nil {
			return nil, err
		}
		prio := 0
		if ev.Priority != nil {
			prio = *ev.Priority
		}
		var cd int64
		if ev.CooldownMS != nil {
			cd = *ev.CooldownMS
		}
		ne := &model.Event{
			ID: ev.ID, Kind: ev.Kind, At: at,
			Targets: append([]string(nil), ev.Chain...), // chain = TARGETS in format 2 (swap)
			Casters: append([]string(nil), ev.Casters...),
			Amount:  *ev.Amount, Skill: ev.Skill, CooldownUnits: cd,
			Priority: prio, Mandatory: ev.Mandatory, Fault: ev.Fault,
			SourceFormats: []int{2},
		}
		if err := checkSequences(ne, fmt.Sprintf("events[%d]", i)); err != nil {
			return nil, err
		}
		r.Events = append(r.Events, ne)
	}
	return r, nil
}

func decode3(d *Format3, params model.Params) (*model.Replay, error) {
	if d.Format != 3 {
		return nil, errs.New(errs.EFormat, "format", "", "discriminator mismatch: %d", d.Format)
	}
	r, err := baseReplay(d.ReplayID, d.Entities)
	if err != nil {
		return nil, err
	}
	r.Flags = flagsFromDTO(d.Flags, d.FlagDefs)
	for i, t := range d.SyncPoints {
		r.Sync = append(r.Sync, model.SyncPoint{ID: fmt.Sprintf("sync-f3-%d", i+1), At: t})
	}
	seen := map[string]bool{}
	for i, ev := range d.Events {
		if err := checkEventCommon(ev.ID, ev.Kind, ev.Amount, i); err != nil {
			return nil, err
		}
		if seen[ev.ID] {
			return nil, errs.New(errs.ESchema, fmt.Sprintf("events[%d].id", i), ev.ID, "duplicate event id")
		}
		seen[ev.ID] = true
		if ev.AtMS == nil {
			return nil, errs.New(errs.ESchema, fmt.Sprintf("events[%d].at_ms", i), ev.ID, "missing required field")
		}
		at, err := timeconv.ToInternal(*ev.AtMS, timeconv.BasisMS, params, ev.ID)
		if err != nil {
			return nil, err
		}
		cond, err := condFromDTO(ev.CondFlag, ev.CondWant, i, ev.ID)
		if err != nil {
			return nil, err
		}
		prio := 0
		if ev.Priority != nil {
			prio = *ev.Priority
		}
		var cd int64
		if ev.CooldownMS != nil {
			cd = *ev.CooldownMS
		}
		ne := &model.Event{
			ID: ev.ID, Kind: ev.Kind, At: at,
			Casters: append([]string(nil), ev.CasterSeq...),
			Targets: append([]string(nil), ev.TargetSeq...),
			Amount:  *ev.Amount, Skill: ev.Skill, CooldownUnits: cd,
			Cond: cond, Priority: prio, Mandatory: ev.Mandatory, Fault: ev.Fault,
			SourceFormats: []int{3},
		}
		if err := checkSequences(ne, fmt.Sprintf("events[%d]", i)); err != nil {
			return nil, err
		}
		r.Events = append(r.Events, ne)
	}
	return r, nil
}

// LoadParams reads params.json when present, falling back to defaults.
func LoadParams(dir string) (model.Params, error) {
	p := model.DefaultParams()
	path := filepath.Join(dir, FileParams)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return p, errs.New(errs.EFormat, path, "", "read failed: %v", err)
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, errs.New(errs.EFormat, path, "", "invalid params document: %v", err)
	}
	if p.MsPerTurn <= 0 || p.SegmentUnits <= 0 {
		return p, errs.New(errs.ESchema, path, "", "ms_per_turn and segment_units must be positive")
	}
	if p.MaxParallel <= 0 {
		p.MaxParallel = model.DefaultParams().MaxParallel
	}
	if p.WorkerBuffer <= 0 {
		p.WorkerBuffer = model.DefaultParams().WorkerBuffer
	}
	return p, nil
}
