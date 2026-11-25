package lightshow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

func canonicalShow(show *Show) any {
	return struct {
		ID           string       `json:"id"`
		Name         string       `json:"name"`
		FPS          int64        `json:"fps"`
		DurationUS   int64        `json:"duration_us"`
		WindowUS     int64        `json:"window_us"`
		TimeRounding TimeRounding `json:"time_rounding"`
		ColorStep    string       `json:"color_step"`
		Fixtures     []Fixture    `json:"fixtures"`
		Changes      []Change     `json:"changes"`
		Scenes       []Scene      `json:"scenes"`
		Groups       []Group      `json:"groups"`
		Defaults     Defaults     `json:"defaults"`
	}{
		ID: show.ID, Name: show.Name, FPS: show.FPS, DurationUS: show.DurationUS,
		WindowUS: show.WindowUS, TimeRounding: show.TimeRounding, ColorStep: show.ColorStep,
		Fixtures: show.Fixtures, Changes: show.Changes, Scenes: show.Scenes,
		Groups: show.Groups, Defaults: show.Defaults,
	}
}

func hashJSON(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ProjectHash(show *Show) string {
	return hashJSON(canonicalShow(show))
}

func (w *WindowResult) ComputeContentHash() string {
	payload := struct {
		Index             int                 `json:"index"`
		StartUS           int64               `json:"start_us"`
		EndUS             int64               `json:"end_us"`
		FrameStart        int64               `json:"frame_start"`
		FrameEnd          int64               `json:"frame_end"`
		Outputs           []FrameOutput       `json:"outputs"`
		Decisions         []Decision          `json:"decisions"`
		Consumption       []ChangeConsumption `json:"consumption"`
		GroupSnapshot     GroupSnapshot       `json:"group_snapshot"`
		Dependencies      []Dependency        `json:"dependencies"`
		ChangeOffsetStart int                 `json:"change_offset_start"`
		ChangeOffsetEnd   int                 `json:"change_offset_end"`
	}{
		Index: w.Index, StartUS: w.StartUS, EndUS: w.EndUS,
		FrameStart: w.FrameStart, FrameEnd: w.FrameEnd,
		Outputs: w.Outputs, Decisions: w.Decisions, Consumption: w.Consumption,
		GroupSnapshot: w.GroupSnapshot, Dependencies: w.Dependencies,
		ChangeOffsetStart: w.ChangeOffsetStart, ChangeOffsetEnd: w.ChangeOffsetEnd,
	}
	return hashJSON(payload)
}

func sortDependencies(deps []Dependency) {
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].Kind != deps[j].Kind {
			return deps[i].Kind < deps[j].Kind
		}
		return deps[i].Key < deps[j].Key
	})
}
