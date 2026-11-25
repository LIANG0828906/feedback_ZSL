// Package sample builds the deterministic cross-format validation show.
package sample

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"lightshow"
)

type Manifest struct {
	AlgorithmVersion string                      `json:"algorithm_version"`
	ExportLosses     map[string][]lightshow.Loss `json:"export_losses"`
	ProjectID        string                      `json:"project_id"`
	Seed             int64                       `json:"seed"`
}

type Tolerance struct {
	TimeUS       int64 `json:"time_us"`
	ColorChannel int   `json:"color_channel"`
}

type TruthLight struct {
	FixtureID string        `json:"fixture_id"`
	Color     lightshow.RGB `json:"color"`
	Intensity int           `json:"intensity"`
	Position  [3]int        `json:"position"`
	Focus     int           `json:"focus"`
}

type TruthSample struct {
	Frame  int          `json:"frame"`
	AtUS   int64        `json:"at_us"`
	Lights []TruthLight `json:"lights"`
}

type GroupResolution struct {
	Frame      int      `json:"frame"`
	AtUS       int64    `json:"at_us"`
	FixtureID  string   `json:"fixture_id"`
	Parameter  string   `json:"parameter"`
	Winner     string   `json:"winner"`
	Overridden []string `json:"overridden"`
	Basis      string   `json:"basis"`
}

type Truth struct {
	Seed                   int64             `json:"seed"`
	ProjectID              string            `json:"project_id"`
	AlgorithmVersion       string            `json:"algorithm_version"`
	FPS                    int64             `json:"fps"`
	DurationUS             int64             `json:"duration_us"`
	WindowUS               int64             `json:"window_us"`
	Tolerance              Tolerance         `json:"tolerance"`
	Samples                []TruthSample     `json:"samples"`
	ChangeConsumptionOrder []string          `json:"change_consumption_order"`
	GroupResolutions       []GroupResolution `json:"group_resolutions"`
}

type Generated struct {
	Show     *lightshow.Show
	Truth    Truth
	Manifest Manifest
	Files    map[string][]byte
}

var sampleFrames = []int{0, 15, 30, 39, 45, 51, 60, 75, 78, 90, 93, 105, 117, 120, 135, 150, 165}

func rgb8(r, g, b uint8) lightshow.RGB {
	return lightshow.RGB8To16(r, g, b)
}

func ptr(v int) *int { return &v }

func pos(x, y, z int) *[3]int { return &[3]int{x, y, z} }

func values(color lightshow.RGB, intensity int, position [3]int, focus int) lightshow.Values {
	c := color
	p := position
	return lightshow.Values{Color: &c, Intensity: ptr(intensity), Position: &p, Focus: ptr(focus)}
}

// Build returns the same logical show for a given seed. The seed names the
// production; the artistic values remain fixed so cross-version truth is stable.
func Build(seed int64) *lightshow.Show {
	id := fmt.Sprintf("show-aurora-%d", seed)
	fixtures := []lightshow.Fixture{
		{
			ID: "L1", Name: "Key Front", Color: rgb8(51, 102, 153), Intensity: 600,
			Position: [3]int{100, 200, 300}, Focus: 5000,
			Ranges: defaultRanges(),
		},
		{
			ID: "L2", Name: "Fill Front", Color: rgb8(204, 80, 40), Intensity: 500,
			Position: [3]int{-100, 200, 300}, Focus: 5200,
			Ranges: defaultRanges(),
		},
		{
			ID: "L3", Name: "Back Wash", Color: rgb8(30, 220, 120), Intensity: 700,
			Position: [3]int{0, -150, 250}, Focus: 4500,
			Ranges: defaultRanges(),
		},
		{
			ID: "L4", Name: "Side Accent", Color: rgb8(240, 210, 20), Intensity: 400,
			Position: [3]int{150, -200, 100}, Focus: 6000,
			Ranges: defaultRanges(),
		},
	}
	scenes := []lightshow.Scene{
		{
			ID: "S0", Name: "Opening Base", AtUS: 0,
			Values: map[string]lightshow.Values{
				"L1": values(rgb8(51, 102, 153), 700, [3]int{100, 200, 300}, 5000),
				"L2": values(rgb8(204, 80, 40), 610, [3]int{-100, 200, 300}, 5200),
				"L3": values(rgb8(30, 220, 120), 820, [3]int{0, -150, 250}, 4500),
				"L4": values(rgb8(240, 210, 20), 530, [3]int{150, -200, 100}, 6000),
			},
		},
		{
			ID: "S1", Name: "Late Base Drift", AtUS: lightshow.MSToUS(4200),
			Values: map[string]lightshow.Values{
				"L1": values(rgb8(51, 102, 153), 720, [3]int{100, 200, 340}, 5200),
				"L2": values(rgb8(204, 80, 40), 630, [3]int{-100, 200, 340}, 5400),
				"L3": values(rgb8(30, 220, 120), 840, [3]int{0, -150, 290}, 4700),
				"L4": values(rgb8(240, 210, 20), 550, [3]int{150, -200, 140}, 6200),
			},
		},
	}
	changes := []lightshow.Change{
		{ID: "C00_ALL", TargetKind: "group", TargetID: "G_ALL", AtUS: 0, Interpolation: lightshow.Linear,
			Values: lightshow.Values{Color: rgbp(30, 80, 200), Intensity: ptr(500)}},
		{ID: "C01_FRONT", TargetKind: "group", TargetID: "G_FRONT", AtUS: 0, Interpolation: lightshow.Linear,
			Values: lightshow.Values{Color: rgbp(0, 120, 255), Intensity: ptr(700)}},
		{ID: "C02_L1", TargetKind: "fixture", TargetID: "L1", AtUS: 0, Interpolation: lightshow.Linear,
			Values: lightshow.Values{Color: rgbp(255, 40, 10)}},
		{ID: "C03_BACK", TargetKind: "group", TargetID: "G_BACK", AtUS: 0, Interpolation: lightshow.Linear,
			Values: lightshow.Values{Position: pos(-300, -100, 250), Focus: ptr(4500)}},
		{ID: "C20_L1", TargetKind: "fixture", TargetID: "L1", AtUS: lightshow.MSToUS(1300), Interpolation: lightshow.Linear,
			Values: lightshow.Values{Color: rgbp(255, 255, 0)}},
		{ID: "C10_ALL", TargetKind: "group", TargetID: "G_ALL", AtUS: lightshow.MSToUS(1700), Interpolation: lightshow.Linear,
			Values: lightshow.Values{Color: rgbp(200, 100, 57), Intensity: ptr(450)}},
		{ID: "C24_BACK", TargetKind: "group", TargetID: "G_BACK", AtUS: lightshow.MSToUS(2300), Interpolation: lightshow.Linear,
			Values: lightshow.Values{Focus: ptr(7000)}},
		{ID: "C11_FRONT", TargetKind: "group", TargetID: "G_FRONT", AtUS: lightshow.MSToUS(2600), Interpolation: lightshow.Linear,
			Values: lightshow.Values{Color: rgbp(20, 230, 120), Intensity: ptr(800)}},
		{ID: "C23_L2", TargetKind: "fixture", TargetID: "L2", AtUS: lightshow.MSToUS(3100), Interpolation: lightshow.Linear,
			Values: lightshow.Values{Intensity: ptr(900)}},
		{ID: "C21_ALL", TargetKind: "group", TargetID: "G_ALL", AtUS: lightshow.MSToUS(3900), Interpolation: lightshow.Linear,
			Values: lightshow.Values{Color: rgbp(60, 40, 180), Intensity: ptr(600)}},
		{ID: "C22_FRONT", TargetKind: "group", TargetID: "G_FRONT", AtUS: lightshow.MSToUS(4500), Interpolation: lightshow.Hold,
			Values: lightshow.Values{Color: rgbp(255, 0, 120), Intensity: ptr(600)}},
		{ID: "C30_L4", TargetKind: "fixture", TargetID: "L4", AtUS: lightshow.MSToUS(5100), Interpolation: lightshow.Linear,
			Values: lightshow.Values{Position: pos(300, -300, 500)}},
	}
	show := &lightshow.Show{
		ID: id, Name: "Aurora Cross-format Validation Show",
		FPS: 30, DurationUS: lightshow.MSToUS(6000), WindowUS: lightshow.MSToUS(500),
		TimeRounding: lightshow.RoundNearest, ColorStep: "1/255",
		Fixtures: fixtures, Changes: changes, Scenes: scenes,
		Groups: []lightshow.Group{
			{
				ID: "G_ALL", Name: "All Controlled", Priority: 10, Enabled: true, Mode: "override",
				Fixtures: []string{"L4"}, Groups: []string{"G_FRONT"},
			},
			{
				ID: "G_BACK", Name: "Back", Priority: 20, Enabled: true, Mode: "override",
				Fixtures: []string{"L3"}, Groups: []string{},
			},
			{
				ID: "G_FRONT", Name: "Front", Priority: 30, Enabled: true, Mode: "override",
				Fixtures: []string{"L1", "L2"}, Groups: []string{},
			},
		},
		Defaults:      lightshow.Defaults{GroupEnabled: true, GroupMode: "override", Interpolation: lightshow.Hold},
		SourceVersion: lightshow.Version3,
	}
	return show
}

func rgbp(r, g, b uint8) *lightshow.RGB {
	c := rgb8(r, g, b)
	return &c
}

func defaultRanges() lightshow.FixtureRanges {
	return lightshow.FixtureRanges{
		Intensity: lightshow.IntRange{Min: 0, Max: 1000},
		Position: [3]lightshow.IntRange{
			{Min: -1000, Max: 1000},
			{Min: -1000, Max: 1000},
			{Min: -1000, Max: 1000},
		},
		Focus: lightshow.IntRange{Min: 0, Max: 10000},
	}
}

// Generate writes all four local validation artifacts.
func Generate(dir string, seed int64) (*Generated, error) {
	show := Build(seed)
	if err := show.Normalize(); err != nil {
		return nil, err
	}
	schedule, err := lightshow.Schedule(show)
	if err != nil {
		return nil, err
	}
	v3, err := show.Export(lightshow.Version3)
	if err != nil {
		return nil, err
	}
	v2, err := show.Export(lightshow.Version2)
	if err != nil {
		return nil, err
	}
	v1, err := show.Export(lightshow.Version1)
	if err != nil {
		return nil, err
	}
	truth := makeTruth(show, schedule, seed)
	manifest := Manifest{
		AlgorithmVersion: lightshow.AlgorithmVersion,
		ExportLosses: map[string][]lightshow.Loss{
			"v1": v1.Losses,
			"v2": v2.Losses,
			"v3": v3.Losses,
		},
		ProjectID: show.ID,
		Seed:      seed,
	}
	files := map[string][]byte{}
	for name, result := range map[string]lightshow.ExportResult{"show.v1.json": v1, "show.v2.json": v2, "show.v3.json": v3} {
		files[name] = result.Data
	}
	manifestData, err := marshal(manifest)
	if err != nil {
		return nil, err
	}
	truthData, err := marshal(truth)
	if err != nil {
		return nil, err
	}
	files["manifest.json"] = manifestData
	files["truth.json"] = truthData
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return nil, err
		}
	}
	return &Generated{Show: show, Truth: truth, Manifest: manifest, Files: files}, nil
}

func marshal(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return data, nil
}

func makeTruth(show *lightshow.Show, schedule *lightshow.ScheduleResult, seed int64) Truth {
	frames := make(map[int]lightshow.FrameOutput, len(schedule.Frames))
	for _, frame := range schedule.Frames {
		frames[frame.Frame] = frame
	}
	decisions := make(map[int][]lightshow.Decision)
	for _, d := range schedule.Decisions {
		decisions[d.Frame] = append(decisions[d.Frame], d)
	}
	truth := Truth{
		Seed: seed, ProjectID: show.ID, AlgorithmVersion: lightshow.AlgorithmVersion,
		FPS: show.FPS, DurationUS: show.DurationUS, WindowUS: show.WindowUS,
		Tolerance: Tolerance{TimeUS: lightshow.TimeTolerance(show.FPS), ColorChannel: 0},
	}
	for _, ch := range schedule.Consumption {
		truth.ChangeConsumptionOrder = append(truth.ChangeConsumptionOrder, ch.ChangeID)
	}
	for _, frameNo := range sampleFrames {
		frame, ok := frames[frameNo]
		if !ok {
			continue
		}
		sample := TruthSample{Frame: frame.Frame, AtUS: frame.AtUS}
		for _, light := range frame.Lights {
			sample.Lights = append(sample.Lights, TruthLight{
				FixtureID: light.FixtureID, Color: light.Color, Intensity: light.Intensity,
				Position: light.Position, Focus: light.Focus,
			})
		}
		truth.Samples = append(truth.Samples, sample)
		for _, d := range decisions[frameNo] {
			resolution := GroupResolution{
				Frame: d.Frame, AtUS: d.AtUS, FixtureID: d.FixtureID, Parameter: d.Parameter,
				Winner: d.Winner.Source, Basis: d.Basis,
			}
			for _, overridden := range d.Overridden {
				resolution.Overridden = append(resolution.Overridden, overridden.Source)
			}
			truth.GroupResolutions = append(truth.GroupResolutions, resolution)
		}
	}
	sort.SliceStable(truth.GroupResolutions, func(i, j int) bool {
		a, b := truth.GroupResolutions[i], truth.GroupResolutions[j]
		if a.Frame != b.Frame {
			return a.Frame < b.Frame
		}
		if a.FixtureID != b.FixtureID {
			return a.FixtureID < b.FixtureID
		}
		return a.Parameter < b.Parameter
	})
	return truth
}
