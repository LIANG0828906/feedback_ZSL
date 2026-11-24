package pipeline

import (
	"sort"

	"cadpipe/internal/cad"
)

type PatchVersions struct {
	Control     int64 `json:"control"`
	Knot        int64 `json:"knot"`
	Weight      int64 `json:"weight"`
	Clip        int64 `json:"clip"`
	Material    int64 `json:"material"`
	Subdivision int64 `json:"subdivision"`
}

type Versions struct {
	SceneStructure int64                    `json:"scene_structure"`
	Camera         int64                    `json:"camera"`
	Patches        map[string]PatchVersions `json:"patches"`
}

func NewVersions(scene *cad.Scene) Versions {
	v := Versions{SceneStructure: 1, Camera: 1, Patches: make(map[string]PatchVersions, len(scene.Patches))}
	for _, p := range scene.Patches {
		v.Patches[p.ID] = PatchVersions{
			Control: 1, Knot: 1, Weight: 1, Clip: 1, Material: 1, Subdivision: 1,
		}
	}
	return v
}

func (v Versions) Clone() Versions {
	out := Versions{SceneStructure: v.SceneStructure, Camera: v.Camera, Patches: make(map[string]PatchVersions, len(v.Patches))}
	for id, pv := range v.Patches {
		out.Patches[id] = pv
	}
	return out
}

type dependencyDocument struct {
	SceneStructure int64                    `json:"scene_structure"`
	Patches        map[string]PatchVersions `json:"patches"`
}

func (p *Pipeline) DependencyHashForPatches(ids []string) string {
	subset := make(map[string]PatchVersions, len(ids))
	for _, id := range ids {
		if v, ok := p.Versions.Patches[id]; ok {
			subset[id] = v
		}
	}
	return cad.HashJSON(dependencyDocument{SceneStructure: p.Versions.SceneStructure, Patches: subset})
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
