package formats

import (
	"fmt"
	"sort"

	"replayaudit/internal/errs"
	"replayaudit/internal/model"
)

// Merge combines the three partial normalized formats of the same match into
// one full timeline, retaining source-format provenance.
func Merge(p1, p2, p3 *model.Replay) (*model.Replay, error) {
	if p1.ReplayID != p2.ReplayID || p1.ReplayID != p3.ReplayID {
		return nil, errs.New(errs.ESchema, "replay_id", "",
			"format replay ids disagree: %q %q %q", p1.ReplayID, p2.ReplayID, p3.ReplayID)
	}
	if model.HashOf(p1.Entities) != model.HashOf(p2.Entities) ||
		model.HashOf(p1.Entities) != model.HashOf(p3.Entities) {
		return nil, errs.New(errs.ESchema, "entities", "", "entity tables disagree across formats")
	}

	idx1 := p1.EventIndex()
	idx2 := p2.EventIndex()
	idx3 := p3.EventIndex()
	if len(idx1) != len(idx2) || len(idx1) != len(idx3) {
		return nil, errs.New(errs.ESchema, "events", "",
			"event count disagreement across formats: f1=%d f2=%d f3=%d", len(idx1), len(idx2), len(idx3))
	}

	merged := &model.Replay{ReplayID: p1.ReplayID, Entities: append([]model.EntitySpec(nil), p1.Entities...)}

	ids := make([]string, 0, len(idx1))
	for id := range idx1 {
		if _, ok := idx2[id]; !ok {
			return nil, errs.New(errs.ESchema, "events", id, "event missing from format 2")
		}
		if _, ok := idx3[id]; !ok {
			return nil, errs.New(errs.ESchema, "events", id, "event missing from format 3")
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		e1, e2, e3 := idx1[id], idx2[id], idx3[id]
		if err := agree(id, "kind", e1.Kind, e2.Kind, e3.Kind); err != nil {
			return nil, err
		}
		if err := agreeInt64(id, "at", e1.At, e2.At, e3.At); err != nil {
			return nil, err
		}
		if err := agreeInt64(id, "amount", e1.Amount, e2.Amount, e3.Amount); err != nil {
			return nil, err
		}
		if err := agreeString(id, "skill", e1.Skill, e2.Skill, e3.Skill); err != nil {
			return nil, err
		}
		if err := agreeInt64(id, "cooldown_units", e1.CooldownUnits, e2.CooldownUnits, e3.CooldownUnits); err != nil {
			return nil, err
		}
		if err := agreeInt(id, "priority", e1.Priority, e2.Priority, e3.Priority); err != nil {
			return nil, err
		}
		if err := agreeBool(id, "mandatory", e1.Mandatory, e2.Mandatory, e3.Mandatory); err != nil {
			return nil, err
		}
		if err := agreeBool(id, "fault", e1.Fault, e2.Fault, e3.Fault); err != nil {
			return nil, err
		}
		if err := agreeStrings(id, "casters", e1.Casters, e2.Casters, e3.Casters); err != nil {
			return nil, err
		}
		if err := agreeStrings(id, "targets", e1.Targets, e2.Targets, e3.Targets); err != nil {
			return nil, err
		}
		// Conditions are encoded by formats 1 and 3 only.
		if (e1.Cond == nil) != (e3.Cond == nil) {
			return nil, errs.New(errs.ESchema, fmt.Sprintf("events(%s).cond", id), id,
				"condition presence disagrees between format 1 and 3")
		}
		if e1.Cond != nil && (e1.Cond.Flag != e3.Cond.Flag || e1.Cond.Want != e3.Cond.Want) {
			return nil, errs.New(errs.ESchema, fmt.Sprintf("events(%s).cond", id), id,
				"condition disagrees between format 1 and 3")
		}

		ce := e3.Clone()
		ce.SourceFormats = []int{1, 2, 3}
		ce.FieldSource = map[string]int{
			"at": 1, "kind": 1, "amount": 1, "casters": 1, "targets": 2,
			"cond": 1, "priority": 1, "skill": 1, "cooldown_units": 1,
		}
		merged.Events = append(merged.Events, ce)
	}

	// Flag declarations: union by id, rejecting contradictory same-id defs.
	flagByID := map[string]model.FlagDef{}
	for _, src := range []*model.Replay{p1, p3} {
		for _, f := range src.Flags {
			if prev, ok := flagByID[f.ID]; ok {
				if model.HashOf(prev) != model.HashOf(f) {
					return nil, errs.New(errs.ESchema, "flags", f.ID,
						"flag declaration disagrees across formats: %+v vs %+v", prev, f)
				}
				continue
			}
			flagByID[f.ID] = f
			merged.Flags = append(merged.Flags, f)
		}
	}
	sort.Slice(merged.Flags, func(i, j int) bool { return merged.Flags[i].ID < merged.Flags[j].ID })

	// Mutex groups are carried by format 1.
	merged.Mutex = append(merged.Mutex, p1.Mutex...)

	// Sync points are carried by formats 2 and 3; merge by timestamp.
	syncByAt := map[int64]model.SyncPoint{}
	for _, src := range []*model.Replay{p2, p3} {
		for _, s := range src.Sync {
			if prev, ok := syncByAt[s.At]; ok && prev.At != s.At {
				continue
			}
			syncByAt[s.At] = model.SyncPoint{ID: SyncID(s.At), At: s.At}
		}
	}
	for _, s := range syncByAt {
		merged.Sync = append(merged.Sync, s)
	}
	sort.Slice(merged.Sync, func(i, j int) bool { return merged.Sync[i].At < merged.Sync[j].At })

	return merged, nil
}

// SyncID is the stable identifier of a forced sync point at time at.
func SyncID(at int64) string { return fmt.Sprintf("sync@%d", at) }

func agree(id, field, a, b, c string) error {
	if a != b || a != c {
		return errs.New(errs.ESchema, fmt.Sprintf("events(%s).%s", id, field), id,
			"disagreement across formats: %q / %q / %q", a, b, c)
	}
	return nil
}

func agreeString(id, field string, a, b, c string) error { return agree(id, field, a, b, c) }

func agreeInt64(id, field string, a, b, c int64) error {
	if a != b || a != c {
		return errs.New(errs.ESchema, fmt.Sprintf("events(%s).%s", id, field), id,
			"disagreement across formats: %d / %d / %d", a, b, c)
	}
	return nil
}

func agreeInt(id, field string, a, b, c int) error {
	if a != b || a != c {
		return errs.New(errs.ESchema, fmt.Sprintf("events(%s).%s", id, field), id,
			"disagreement across formats: %d / %d / %d", a, b, c)
	}
	return nil
}

func agreeBool(id, field string, a, b, c bool) error {
	if a != b || a != c {
		return errs.New(errs.ESchema, fmt.Sprintf("events(%s).%s", id, field), id,
			"disagreement across formats: %v / %v / %v", a, b, c)
	}
	return nil
}

func agreeStrings(id, field string, a, b, c []string) error {
	if !equalStrings(a, b) || !equalStrings(a, c) {
		return errs.New(errs.ESchema, fmt.Sprintf("events(%s).%s", id, field), id,
			"sequence disagreement across formats: %v / %v / %v", a, b, c)
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
