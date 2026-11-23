package generator

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"stageworkbench/pkg/stage"
)

type Preset struct {
	Name        string
	CueCount    int
	LightCount  int
	GroupCount  int
	DurationMS  int64
	FPS         int
	WindowMS    int64
	SampleCount int
}

var presets = map[string]Preset{
	"tiny":   {Name: "tiny", CueCount: 48, LightCount: 6, GroupCount: 3, DurationMS: 12_000, FPS: 30, WindowMS: 1_000, SampleCount: 12},
	"small":  {Name: "small", CueCount: 600, LightCount: 16, GroupCount: 5, DurationMS: 30_000, FPS: 60, WindowMS: 1_000, SampleCount: 32},
	"medium": {Name: "medium", CueCount: 5_000, LightCount: 40, GroupCount: 8, DurationMS: 60_000, FPS: 60, WindowMS: 2_000, SampleCount: 48},
	"large":  {Name: "large", CueCount: 30_000, LightCount: 80, GroupCount: 12, DurationMS: 120_000, FPS: 60, WindowMS: 5_000, SampleCount: 64},
	"stress": {Name: "stress", CueCount: 50_000, LightCount: 120, GroupCount: 16, DurationMS: 180_000, FPS: 60, WindowMS: 5_000, SampleCount: 80},
}

type Options struct {
	Preset     string
	Seed       int64
	ProjectID  string
	DurationMS int64
	FPS        int
	WindowMS   int64
	CueCount   int
	LightCount int
	GroupCount int
}

func Presets() []Preset {
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Preset, 0, len(names))
	for _, name := range names {
		out = append(out, presets[name])
	}
	return out
}

func Generate(opts Options) (*stage.Project, error) {
	presetName := opts.Preset
	if presetName == "" {
		presetName = "small"
	}
	preset, ok := presets[presetName]
	if !ok {
		return nil, fmt.Errorf("unknown preset %q", presetName)
	}
	if opts.CueCount > 0 {
		preset.CueCount = opts.CueCount
	}
	if opts.LightCount > 0 {
		preset.LightCount = opts.LightCount
	}
	if opts.GroupCount > 0 {
		preset.GroupCount = opts.GroupCount
	}
	if opts.DurationMS > 0 {
		preset.DurationMS = opts.DurationMS
	}
	if opts.FPS > 0 {
		preset.FPS = opts.FPS
	}
	if opts.WindowMS > 0 {
		preset.WindowMS = opts.WindowMS
	}
	if preset.LightCount <= 0 || preset.CueCount <= 0 || preset.GroupCount <= 0 {
		return nil, fmt.Errorf("invalid preset dimensions")
	}
	if preset.GroupCount > preset.LightCount {
		return nil, fmt.Errorf("group count cannot exceed light count")
	}
	rng := newSplitMix(uint64(opts.Seed) ^ 0x9e3779b97f4a7c15)
	projectID := opts.ProjectID
	if projectID == "" {
		projectID = fmt.Sprintf("show-%s-%d", preset.Name, opts.Seed)
	}
	p := &stage.Project{
		SchemaVersion: stage.SchemaVersion,
		ID:            projectID,
		Name:          fmt.Sprintf("Deterministic Stage %s (seed %d)", preset.Name, opts.Seed),
		Seed:          opts.Seed,
		FPS:           preset.FPS,
		DurationMS:    preset.DurationMS,
		WindowMS:      preset.WindowMS,
	}
	p.Lights = generateLights(preset.LightCount, rng)
	p.Groups = generateGroups(preset.GroupCount, preset.LightCount, rng)
	p.Cues = generateCues(preset, p, rng)
	p.Scenes = generateScenes(p, rng)

	groupsByID := make(map[string]*stage.Group, len(p.Groups))
	for i := range p.Groups {
		groupsByID[p.Groups[i].ID] = &p.Groups[i]
	}
	p.Truth = stage.Truth{
		TotalDurationMS: p.DurationMS,
		GroupOrder:      stage.GroupPrecedenceOrder(groupsByID),
		Tolerance:       stage.DefaultTolerance(),
	}
	sampleFrames := chooseSampleFrames(p, preset.SampleCount)
	reference, err := newReference(p)
	if err != nil {
		return nil, err
	}
	for _, frame := range sampleFrames {
		timeUS := stage.FrameTimeUS(frame, p.FPS)
		timeMS := int64(math.Round(float64(timeUS) / 1000))
		sample := stage.TruthSample{
			Frame:  frame,
			TimeMS: timeMS,
			Lights: reference.sample(frame),
		}
		p.Truth.Samples = append(p.Truth.Samples, sample)
		p.Truth.SampleTimesMS = append(p.Truth.SampleTimesMS, timeMS)
	}
	if err := stage.ValidateProject(p, stage.DefaultValidateOptions()); err != nil {
		return nil, fmt.Errorf("generated project failed validation: %w", err)
	}
	return p, nil
}

func generateLights(count int, rng *splitMix) []stage.Light {
	kinds := []stage.LightKind{stage.LightPar, stage.LightMoving, stage.LightWash}
	lights := make([]stage.Light, count)
	columns := int(math.Ceil(math.Sqrt(float64(count))))
	for i := 0; i < count; i++ {
		column := i % columns
		row := i / columns
		base := stage.LightValues{
			RGB: stage.RGB{
				R: rng.rangeFloat(80, 240),
				G: rng.rangeFloat(80, 240),
				B: rng.rangeFloat(80, 240),
			},
			Intensity: rng.rangeFloat(0.65, 1),
			Pan:       rng.rangeFloat(-35, 35),
			Tilt:      rng.rangeFloat(-20, 20),
			Focus:     rng.rangeFloat(35, 75),
			ColorTemp: rng.rangeFloat(3200, 6500),
			StrobeHz:  0,
		}
		kind := kinds[i%len(kinds)]
		lights[i] = stage.Light{
			ID:   fmt.Sprintf("L%03d", i+1),
			Name: fmt.Sprintf("%s %02d", kind, i+1),
			Kind: kind,
			Layout: stage.Vec3{
				X: float64(column-columns/2)*1.6 + rng.rangeFloat(-0.12, 0.12),
				Y: float64(row)*1.2 + rng.rangeFloat(-0.08, 0.08),
				Z: rng.rangeFloat(3.5, 7.5),
			},
			Base:   base,
			Ranges: stage.DefaultParamRanges(),
		}
	}
	return lights
}

func generateGroups(count, lightCount int, rng *splitMix) []stage.Group {
	groups := make([]stage.Group, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("G%02d", i+1)
		group := stage.Group{
			ID:       id,
			Name:     fmt.Sprintf("Group %02d", i+1),
			Priority: 100 + rng.Intn(160),
		}
		if i >= 3 && i%3 == 0 {
			group.ParentID = fmt.Sprintf("G%02d", i)
		}
		memberCount := 2 + rng.Intn(4)
		if memberCount > lightCount {
			memberCount = lightCount
		}
		seen := map[int]struct{}{}
		for len(group.Members) < memberCount {
			index := (i*3 + rng.Intn(lightCount)) % lightCount
			if _, ok := seen[index]; ok {
				index = (index + 1) % lightCount
			}
			if _, ok := seen[index]; ok {
				for candidate := 0; candidate < lightCount; candidate++ {
					if _, exists := seen[candidate]; !exists {
						index = candidate
						break
					}
				}
			}
			seen[index] = struct{}{}
			group.Members = append(group.Members, fmt.Sprintf("L%03d", index+1))
		}
		sort.Strings(group.Members)
		groups = append(groups, group)
	}
	return groups
}

func generateCues(preset Preset, p *stage.Project, rng *splitMix) []stage.Cue {
	kinds := []stage.CueKind{
		stage.CueColorTemp, stage.CueMove, stage.CueStrobe,
		stage.CueFade, stage.CueSet,
	}
	cues := make([]stage.Cue, 0, preset.CueCount+preset.CueCount/8)
	serial := 0
	nextID := func() string {
		serial++
		return fmt.Sprintf("C%06d", serial)
	}
	duration := preset.DurationMS
	for i := 0; i < preset.CueCount; i++ {
		timeMS := duration * int64(i) / int64(preset.CueCount)
		kind := kinds[i%len(kinds)]
		groupCue := i%7 == 3
		if groupCue {
			kind = stage.CueGroup
		}
		cue := stage.Cue{
			ID:           nextID(),
			Label:        fmt.Sprintf("%s @ %dms", kind, timeMS),
			TimeMS:       timeMS,
			Kind:         kind,
			TransitionMS: pickTransition(i, preset.DurationMS/int64(preset.CueCount), rng),
			Easing:       pickEasing(i),
			CurvePower:   rng.rangeFloat(0.8, 1.4),
			Priority:     100 + rng.Intn(5),
		}
		if groupCue {
			cue.GroupID = p.Groups[rng.Intn(len(p.Groups))].ID
			cue.Values = groupValues(rng)
		} else {
			cue.LightID = p.Lights[rng.Intn(len(p.Lights))].ID
			cue.Values = singleValues(kind, rng)
		}
		cues = append(cues, cue)

		if i%13 == 9 {
			overrideGroupIndex := rng.Intn(len(p.Groups))
			if cue.GroupID != "" && p.Groups[overrideGroupIndex].ID == cue.GroupID {
				overrideGroupIndex = (overrideGroupIndex + 1) % len(p.Groups)
			}
			override := stage.Cue{
				ID:           nextID(),
				Label:        "deterministic group/single arbitration",
				TimeMS:       timeMS,
				GroupID:      p.Groups[overrideGroupIndex].ID,
				Kind:         stage.CueGroup,
				TransitionMS: 0,
				Easing:       stage.EaseStep,
				CurvePower:   1,
				Priority:     90 + rng.Intn(30),
				Values: stage.CueValues{
					Intensity: floatPtr(rng.rangeFloat(0.18, 0.5)),
					RGB:       &stage.RGB{R: rng.rangeFloat(10, 90), G: rng.rangeFloat(10, 90), B: rng.rangeFloat(10, 90)},
				},
			}
			cues = append(cues, override)
			single := stage.Cue{
				ID:           nextID(),
				Label:        "single light override",
				TimeMS:       timeMS,
				LightID:      p.Lights[rng.Intn(len(p.Lights))].ID,
				Kind:         stage.CueSet,
				TransitionMS: 0,
				Easing:       stage.EaseStep,
				CurvePower:   1,
				Priority:     150,
				Values: stage.CueValues{
					Intensity: floatPtr(rng.rangeFloat(0.7, 1)),
				},
			}
			cues = append(cues, single)
		}
	}
	sort.SliceStable(cues, func(i, j int) bool {
		if cues[i].TimeMS != cues[j].TimeMS {
			return cues[i].TimeMS < cues[j].TimeMS
		}
		return cues[i].ID < cues[j].ID
	})
	return cues
}

func pickTransition(index int, averageGapMS int64, rng *splitMix) int64 {
	if index%5 == 0 {
		return 0
	}
	max := averageGapMS * 2
	if max < 12 {
		max = 12
	}
	if max > 600 {
		max = 600
	}
	return rng.Int63n(max + 1)
}

func pickEasing(index int) stage.Easing {
	switch index % 4 {
	case 1:
		return stage.EaseStep
	case 2:
		return stage.EaseInOut
	default:
		return stage.EaseLinear
	}
}

func singleValues(kind stage.CueKind, rng *splitMix) stage.CueValues {
	switch kind {
	case stage.CueColorTemp:
		return stage.CueValues{ColorTemp: floatPtr(rng.rangeFloat(2500, 9200))}
	case stage.CueMove:
		return stage.CueValues{
			Pan:   floatPtr(rng.rangeFloat(-150, 150)),
			Tilt:  floatPtr(rng.rangeFloat(-110, 110)),
			Focus: floatPtr(rng.rangeFloat(5, 95)),
		}
	case stage.CueStrobe:
		return stage.CueValues{StrobeHz: floatPtr(rng.rangeFloat(0, 12))}
	case stage.CueFade:
		return stage.CueValues{
			RGB: &stage.RGB{
				R: rng.rangeFloat(0, 255),
				G: rng.rangeFloat(0, 255),
				B: rng.rangeFloat(0, 255),
			},
			Intensity: floatPtr(rng.rangeFloat(0, 1)),
		}
	default:
		return stage.CueValues{
			RGB: &stage.RGB{
				R: rng.rangeFloat(0, 255),
				G: rng.rangeFloat(0, 255),
				B: rng.rangeFloat(0, 255),
			},
		}
	}
}

func groupValues(rng *splitMix) stage.CueValues {
	return stage.CueValues{
		RGB: &stage.RGB{
			R: rng.rangeFloat(0, 255),
			G: rng.rangeFloat(0, 255),
			B: rng.rangeFloat(0, 255),
		},
		Intensity: floatPtr(rng.rangeFloat(0.2, 1)),
	}
}

func generateScenes(p *stage.Project, rng *splitMix) []stage.Scene {
	sceneCount := 4 + rng.Intn(5)
	if sceneCount > len(p.Cues) {
		sceneCount = len(p.Cues)
	}
	scenes := make([]stage.Scene, 0, sceneCount)
	for i := 0; i < sceneCount; i++ {
		start := len(p.Cues) * i / sceneCount
		end := len(p.Cues) * (i + 1) / sceneCount
		ids := make([]string, 0, end-start)
		for j := start; j < end; j++ {
			ids = append(ids, p.Cues[j].ID)
		}
		scenes = append(scenes, stage.Scene{
			ID:     fmt.Sprintf("S%02d", i+1),
			Name:   fmt.Sprintf("Scene %02d", i+1),
			CueIDs: ids,
		})
	}
	return scenes
}

func chooseSampleFrames(p *stage.Project, requested int) []int {
	frameCount := stage.FrameCount(p.DurationMS, p.FPS)
	if requested < 8 {
		requested = 8
	}
	set := map[int]struct{}{0: {}, frameCount - 1: {}}
	for sample := 0; sample < requested; sample++ {
		frame := 0
		if requested > 1 {
			frame = (frameCount - 1) * sample / (requested - 1)
		}
		set[frame] = struct{}{}
	}
	stride := len(p.Cues) / requested
	if stride < 1 {
		stride = 1
	}
	for index := 0; index < len(p.Cues); index += stride {
		frame := int(math.Round(float64(p.Cues[index].TimeMS) * float64(p.FPS) / 1000))
		if frame >= frameCount {
			frame = frameCount - 1
		}
		if frame < 0 {
			frame = 0
		}
		set[frame] = struct{}{}
		if len(set) >= requested*2 {
			break
		}
	}
	frames := make([]int, 0, len(set))
	for frame := range set {
		frames = append(frames, frame)
	}
	sort.Ints(frames)
	return frames
}

func floatPtr(v float64) *float64 { return &v }

func SaveProject(dir string, p *stage.Project) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	projectData, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"), append(projectData, '\n'), 0o644); err != nil {
		return err
	}
	truthData, err := json.MarshalIndent(p.Truth, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), append(truthData, '\n'), 0o644); err != nil {
		return err
	}
	manifest := map[string]any{
		"project_id":     p.ID,
		"preset_seed":    p.Seed,
		"fps":            p.FPS,
		"duration_ms":    p.DurationMS,
		"window_ms":      p.WindowMS,
		"lights":         len(p.Lights),
		"groups":         len(p.Groups),
		"cues":           len(p.Cues),
		"content_hash":   stage.ContentHash(p),
		"algorithm":      stage.AlgorithmVersion,
		"truth_total_ms": p.Truth.TotalDurationMS,
		"truth_samples":  len(p.Truth.Samples),
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(manifestData, '\n'), 0o644)
}

func LoadProject(path string) (*stage.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p stage.Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if err := stage.ValidateProject(&p, stage.DefaultValidateOptions()); err != nil {
		return nil, err
	}
	return &p, nil
}
