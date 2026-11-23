package samplegen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"storyworkbench/engine"
	"storyworkbench/story"
)

type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	Generator     string          `json:"generator"`
	BaseSeed      int64           `json:"base_seed"`
	Projects      []ManifestEntry `json:"projects"`
}

type ManifestEntry struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	File            string `json:"file"`
	Seed            int64  `json:"seed"`
	ContentHash     string `json:"content_hash"`
	ChoiceCount     int    `json:"choice_count"`
	ShotCount       int    `json:"shot_count"`
	TotalDurationMS int64  `json:"total_duration_ms"`
	ReachableCount  int    `json:"reachable_count"`
}

type deterministicRNG struct {
	state uint64
}

func newRNG(seed int64) *deterministicRNG {
	return &deterministicRNG{state: uint64(seed) ^ 0x9e3779b97f4a7c15}
}

func (r *deterministicRNG) next() uint64 {
	x := r.state
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.state = x
	return x
}

func (r *deterministicRNG) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func (r *deterministicRNG) duration() int64 {
	values := []int64{420, 560, 680, 760, 880, 1040, 1210, 1380}
	return values[r.intn(len(values))]
}

func GenerateProject(seed int64, variant int) (*story.Project, error) {
	rng := newRNG(seed)
	names := []struct {
		id    string
		title string
	}{
		{"glass-meridian", "Glass Meridian"},
		{"salt-orchard", "The Salt Orchard"},
		{"quiet-relay", "The Quiet Relay"},
	}
	base := names[variant%len(names)]
	projectID := fmt.Sprintf("%s-%d", base.id, seed)
	title := fmt.Sprintf("%s / Seed %d", base.title, seed)
	aspectSets := [][]string{
		{"16:9", "4:3"},
		{"2.39:1", "16:9"},
		{"9:16", "1:1"},
	}

	makeShots := func(label string) []story.Shot {
		count := 2 + rng.intn(3)
		shots := make([]story.Shot, 0, count)
		aspects := aspectSets[rng.intn(len(aspectSets))]
		for i := 1; i <= count; i++ {
			prio := append([]string(nil), aspects...)
			if rng.intn(2) == 0 && len(prio) > 1 {
				prio[0], prio[1] = prio[1], prio[0]
			}
			shots = append(shots, story.Shot{
				DurationMS:     rng.duration(),
				AspectPriority: prio,
				Description:    fmt.Sprintf("%s: composition %d%s", label, i, strings.Repeat(" / held", rng.intn(2))),
			})
		}
		return shots
	}

	nodes := []story.Node{
		{
			ID:       "arrival",
			Title:    "Arrival at the Relay",
			Requires: []story.Condition{},
			Mutex:    []string{},
			Join:     []string{},
			Events: []story.Event{
				{ID: "ev-arrival-flag", Kind: "set_flag", Key: "arrived", Value: true},
				{ID: "ev-arrival-count", Kind: "set_flag", Key: "fragment_count", Value: int64(0)},
				{ID: "ev-arrival-trust", Kind: "inc_character", Character: "mara", Key: "trust", Value: int64(1)},
			},
			Shots: makeShots("arrival"),
		},
		{
			ID:    "whisper",
			Title: "The Copper Whisper",
			Requires: []story.Condition{
				{Scope: "flag", Key: "arrived", Op: "eq", Value: true},
				{Scope: "character", Character: "mara", Key: "focus", Op: "gte", Value: int64(2)},
			},
			Mutex: []string{"alt_betrayal"},
			Join:  []string{"arrival"},
			Events: []story.Event{
				{ID: "ev-whisper-flag", Kind: "set_flag", Key: "heard_whisper", Value: true},
				{ID: "ev-whisper-count", Kind: "inc_flag", Key: "fragment_count", Value: int64(1)},
				{ID: "ev-whisper-resolve", Kind: "inc_character", Character: "mara", Key: "resolve", Value: int64(1)},
			},
			Shots: makeShots("whisper"),
		},
		{
			ID:    "alt_betrayal",
			Title: "The Betrayal Corridor",
			Requires: []story.Condition{
				{Scope: "flag", Key: "arrived", Op: "eq", Value: true},
			},
			Mutex: []string{"whisper"},
			Join:  []string{"arrival"},
			Events: []story.Event{
				{ID: "ev-betray-flag", Kind: "set_flag", Key: "betrayed", Value: true},
				{ID: "ev-betray-trust", Kind: "inc_character", Character: "ivo", Key: "trust", Value: int64(-2)},
			},
			Shots: makeShots("betrayal"),
		},
		{
			ID:    "archive",
			Title: "Archive of Rain",
			Requires: []story.Condition{
				{Scope: "flag", Key: "fragment_count", Op: "gte", Value: int64(1)},
				{Scope: "character", Character: "mara", Key: "resolve", Op: "gte", Value: int64(2)},
			},
			Mutex: []string{},
			Join:  []string{"whisper"},
			Events: []story.Event{
				{ID: "ev-archive-key", Kind: "set_flag", Key: "archive_key", Value: "brass"},
				{ID: "ev-archive-count", Kind: "inc_flag", Key: "fragment_count", Value: int64(2)},
				{ID: "ev-archive-trust", Kind: "inc_character", Character: "ivo", Key: "trust", Value: int64(1)},
			},
			Shots: makeShots("archive"),
		},
		{
			ID:    "archive_echo",
			Title: "Echo Map",
			Requires: []story.Condition{
				{Scope: "flag", Key: "archive_key", Op: "eq", Value: "brass"},
			},
			Mutex: []string{},
			Join:  []string{"archive"},
			Events: []story.Event{
				{ID: "ev-echo-flag", Kind: "set_flag", Key: "echo_mapped", Value: true},
				{ID: "ev-echo-focus", Kind: "inc_character", Character: "ivo", Key: "focus", Value: int64(1)},
			},
			Shots: makeShots("echo"),
		},
		{
			ID:    "gate",
			Title: "The Convergence Gate",
			Requires: []story.Condition{
				{Scope: "flag", Key: "fragment_count", Op: "gte", Value: int64(3)},
				{Scope: "flag", Key: "echo_mapped", Op: "eq", Value: true},
				{Scope: "character", Character: "ivo", Key: "trust", Op: "gte", Value: int64(1)},
			},
			Mutex: []string{"false_gate"},
			Join:  []string{"whisper", "archive", "archive_echo"},
			Events: []story.Event{
				{ID: "ev-gate-flag", Kind: "set_flag", Key: "gate_open", Value: true},
				{ID: "ev-gate-resolve", Kind: "inc_character", Character: "mara", Key: "resolve", Value: int64(2)},
			},
			Shots: makeShots("gate"),
		},
		{
			ID:    "false_gate",
			Title: "The Dry Gate",
			Requires: []story.Condition{
				{Scope: "flag", Key: "archive_key", Op: "exists"},
			},
			Mutex: []string{"gate"},
			Join:  []string{"whisper", "archive"},
			Events: []story.Event{
				{ID: "ev-false-gate", Kind: "set_flag", Key: "false_gate_seen", Value: true},
				{ID: "ev-false-resolve", Kind: "inc_character", Character: "mara", Key: "resolve", Value: int64(-1)},
			},
			Shots: makeShots("false-gate"),
		},
		{
			ID:    "descent",
			Title: "Descent Through Static",
			Requires: []story.Condition{
				{Scope: "flag", Key: "gate_open", Op: "eq", Value: true},
				{Scope: "character", Character: "ivo", Key: "focus", Op: "gte", Value: int64(2)},
			},
			Mutex: []string{},
			Join:  []string{"gate"},
			Events: []story.Event{
				{ID: "ev-descent-flag", Kind: "set_flag", Key: "descent", Value: true},
				{ID: "ev-descent-focus", Kind: "inc_character", Character: "ivo", Key: "focus", Value: int64(1)},
			},
			Shots: makeShots("descent"),
		},
		{
			ID:    "dawn",
			Title: "Dawn Signal",
			Requires: []story.Condition{
				{Scope: "flag", Key: "descent", Op: "eq", Value: true},
				{Scope: "character", Character: "mara", Key: "resolve", Op: "gte", Value: int64(4)},
			},
			Mutex: []string{"ash_end"},
			Join:  []string{"descent"},
			Events: []story.Event{
				{ID: "ev-dawn-flag", Kind: "set_flag", Key: "dawn", Value: true},
				{ID: "ev-dawn-ending", Kind: "set_flag", Key: "ending", Value: "dawn"},
				{ID: "ev-dawn-trust", Kind: "inc_character", Character: "mara", Key: "trust", Value: int64(1)},
			},
			Shots: makeShots("dawn"),
		},
		{
			ID:    "ash_end",
			Title: "Ash Ending",
			Requires: []story.Condition{
				{Scope: "flag", Key: "betrayed", Op: "eq", Value: true},
			},
			Mutex: []string{"dawn"},
			Join:  []string{"descent"},
			Events: []story.Event{
				{ID: "ev-ash-flag", Kind: "set_flag", Key: "ash", Value: true},
				{ID: "ev-ash-ending", Kind: "set_flag", Key: "ending", Value: "ash"},
			},
			Shots: makeShots("ash"),
		},
	}

	choices := []story.Choice{
		{AtMS: 120, Target: "arrival", Label: "Step through the relay door"},
		{AtMS: 1280, Target: "whisper", Label: "Listen to the copper line"},
		{AtMS: 2440, Target: "alt_betrayal", Label: "Consider abandoning the signal"},
		{AtMS: 3610, Target: "archive", Label: "Open the rain archive"},
		{AtMS: 4870, Target: "archive_echo", Label: "Map the reflected corridor"},
		{AtMS: 6240, Target: "gate", Label: "Unseal the convergence gate"},
		{AtMS: 7310, Target: "false_gate", Label: "Try the dry gate anyway"},
		{AtMS: 8420, Target: "descent", Label: "Descend through the static"},
		{AtMS: 9680, Target: "dawn", Label: "Transmit the dawn sequence"},
		{AtMS: 10910, Target: "ash_end", Label: "Burn the ending protocol"},
	}

	project := &story.Project{
		SchemaVersion: story.ProjectSchemaVersion,
		ID:            projectID,
		Title:         title,
		Seed:          seed,
		InitialFlags: map[string]any{
			"arrived":        false,
			"heard_whisper":  false,
			"fragment_count": int64(0),
			"gate_open":      false,
			"descent":        false,
			"dawn":           false,
			"ending":         "unresolved",
		},
		Characters: map[string]story.Character{
			"mara": {
				Name: "Mara Venn",
				State: map[string]any{
					"resolve": int64(1),
					"trust":   int64(0),
					"focus":   int64(2),
				},
			},
			"ivo": {
				Name: "Ivo Sable",
				State: map[string]any{
					"resolve": int64(0),
					"trust":   int64(0),
					"focus":   int64(1),
				},
			},
		},
		Nodes:   nodes,
		Choices: choices,
	}
	if err := story.ValidateProject(project); err != nil {
		return nil, err
	}
	run, err := engine.Play(project, project.Choices)
	if err != nil {
		return nil, err
	}
	project.Truth = &story.Truth{
		FinalFlags:      story.CloneAnyMap(run.FinalFlags),
		FinalCharacters: story.CloneCharacterState(run.FinalCharacters),
		EventOrder:      append([]string(nil), run.EventOrder...),
		ReachableNodes:  append([]string(nil), run.ReachableNodes...),
		ShotCount:       run.ShotCount,
		TotalDurationMS: run.TotalDurationMS,
	}
	return project, nil
}

func GenerateSet(outDir string, baseSeed int64, count int) (*Manifest, error) {
	if count <= 0 {
		return nil, fmt.Errorf("count must be positive")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	manifest := &Manifest{
		SchemaVersion: 1,
		Generator:     "storygen/1",
		BaseSeed:      baseSeed,
		Projects:      []ManifestEntry{},
	}
	for i := 0; i < count; i++ {
		seed := baseSeed + int64(i)*7919
		project, err := GenerateProject(seed, i)
		if err != nil {
			return nil, fmt.Errorf("generate project %d: %w", i, err)
		}
		data, err := json.MarshalIndent(project, "", "  ")
		if err != nil {
			return nil, err
		}
		fileName := project.ID + ".json"
		path := filepath.Join(outDir, fileName)
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return nil, err
		}
		manifest.Projects = append(manifest.Projects, ManifestEntry{
			ID:              project.ID,
			Title:           project.Title,
			File:            fileName,
			Seed:            project.Seed,
			ContentHash:     story.ContentHash(project),
			ChoiceCount:     len(project.Choices),
			ShotCount:       project.Truth.ShotCount,
			TotalDurationMS: project.Truth.TotalDurationMS,
			ReachableCount:  len(project.Truth.ReachableNodes),
		})
	}
	sort.Slice(manifest.Projects, func(i, j int) bool {
		return manifest.Projects[i].ID < manifest.Projects[j].ID
	})
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}
