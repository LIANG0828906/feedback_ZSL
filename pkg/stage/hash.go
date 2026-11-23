package stage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

type hashProject struct {
	SchemaVersion int     `json:"schema_version"`
	ID            string  `json:"id"`
	FPS           int     `json:"fps"`
	DurationMS    int64   `json:"duration_ms"`
	WindowMS      int64   `json:"window_ms"`
	Lights        []Light `json:"lights"`
	Groups        []Group `json:"groups"`
	Cues          []Cue   `json:"cues"`
	Scenes        []Scene `json:"scenes"`
}

func ContentHash(p *Project) string {
	h := hashProject{
		SchemaVersion: p.SchemaVersion,
		ID:            p.ID,
		FPS:           p.FPS,
		DurationMS:    p.DurationMS,
		WindowMS:      p.WindowMS,
		Lights:        append([]Light(nil), p.Lights...),
		Groups:        append([]Group(nil), p.Groups...),
		Cues:          append([]Cue(nil), p.Cues...),
		Scenes:        append([]Scene(nil), p.Scenes...),
	}
	sort.Slice(h.Lights, func(i, j int) bool { return h.Lights[i].ID < h.Lights[j].ID })
	sort.Slice(h.Groups, func(i, j int) bool { return h.Groups[i].ID < h.Groups[j].ID })
	for i := range h.Groups {
		members := append([]string(nil), h.Groups[i].Members...)
		sort.Strings(members)
		h.Groups[i].Members = members
	}
	sort.Slice(h.Cues, func(i, j int) bool {
		if h.Cues[i].TimeMS != h.Cues[j].TimeMS {
			return h.Cues[i].TimeMS < h.Cues[j].TimeMS
		}
		return h.Cues[i].ID < h.Cues[j].ID
	})
	sort.Slice(h.Scenes, func(i, j int) bool { return h.Scenes[i].ID < h.Scenes[j].ID })
	for i := range h.Scenes {
		ids := append([]string(nil), h.Scenes[i].CueIDs...)
		sort.Strings(ids)
		h.Scenes[i].CueIDs = ids
	}
	raw, _ := json.Marshal(h)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func ParamVersion(params Params) string {
	c := struct {
		WindowMS          int64   `json:"window_ms"`
		PreviewFPS        int     `json:"preview_fps"`
		MaxExactCues      int     `json:"max_exact_cues"`
		MaxExactOps       int64   `json:"max_exact_ops"`
		ColorTolerance    float64 `json:"color_tolerance"`
		PositionTolerance float64 `json:"position_tolerance"`
		OtherTolerance    float64 `json:"other_tolerance"`
		ForceMode         Mode    `json:"force_mode"`
		AlgorithmVersion  string  `json:"algorithm_version"`
	}{
		WindowMS:          params.WindowMS,
		PreviewFPS:        params.PreviewFPS,
		MaxExactCues:      params.MaxExactCues,
		MaxExactOps:       params.MaxExactOps,
		ColorTolerance:    params.ColorTolerance,
		PositionTolerance: params.PositionTolerance,
		OtherTolerance:    params.OtherTolerance,
		ForceMode:         params.ForceMode,
		AlgorithmVersion:  AlgorithmVersion,
	}
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
