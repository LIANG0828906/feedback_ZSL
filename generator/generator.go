package generator

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"performanceexpressions/workspace/domain"
)

const DefaultSeed int64 = 20261124

type Theme struct {
	Slug        string
	Name        string
	Duration    float64
	BaseTempo   float64
	BaseDensity float64
}

var Themes = []Theme{
	{Slug: "aerial-race", Name: "Aerial Race", Duration: 12, BaseTempo: 138, BaseDensity: 3.2},
	{Slug: "ember-dialogue", Name: "Ember Dialogue", Duration: 12, BaseTempo: 92, BaseDensity: 1.6},
	{Slug: "tidal-sanctuary", Name: "Tidal Sanctuary", Duration: 12, BaseTempo: 74, BaseDensity: 0.9},
}

type Index struct {
	Seed     int64            `json:"seed"`
	Projects []IndexedProject `json:"projects"`
}

type IndexedProject struct {
	File string `json:"file"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Hash string `json:"sha256"`
}

// GenerateProjects creates the complete deterministic sample set.
func GenerateProjects(seed int64) []domain.Project {
	projects := make([]domain.Project, 0, len(Themes))
	for i, theme := range Themes {
		projects = append(projects, generateProject(seed, int64(i)*104729, theme))
	}
	return projects
}

func WriteSamples(dir string, seed int64) (Index, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Index{}, err
	}
	projects := GenerateProjects(seed)
	index := Index{Seed: seed}
	for _, project := range projects {
		b, err := json.MarshalIndent(project, "", "  ")
		if err != nil {
			return Index{}, err
		}
		b = append(b, '\n')
		file := project.ID + ".json"
		if err := os.WriteFile(filepath.Join(dir, file), b, 0o644); err != nil {
			return Index{}, err
		}
		hash, err := domain.HashJSON(project)
		if err != nil {
			return Index{}, err
		}
		index.Projects = append(index.Projects, IndexedProject{
			File: file,
			ID:   project.ID,
			Name: project.Name,
			Hash: hash,
		})
	}
	b, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return Index{}, err
	}
	b = append(b, '\n')
	if err := os.WriteFile(filepath.Join(dir, "index.json"), b, 0o644); err != nil {
		return Index{}, err
	}
	return index, nil
}

func LoadProject(path string) (domain.Project, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return domain.Project{}, err
	}
	var project domain.Project
	if err := json.Unmarshal(b, &project); err != nil {
		return domain.Project{}, fmt.Errorf("decode project: %w", err)
	}
	return project, nil
}

func generateProject(seed, sequence int64, theme Theme) domain.Project {
	rng := newRNG(uint64(seed) ^ uint64(sequence) ^ 0x9e3779b97f4a7c15)
	project := domain.Project{
		SchemaVersion:  domain.SchemaVersion,
		ID:             theme.Slug,
		Name:           theme.Name,
		Seed:           seed,
		SamplesPerBeat: 4,
		DurationBeats:  theme.Duration,
		Segments:       make([]domain.Segment, 0, 3),
	}
	for i := 0; i < 3; i++ {
		start := float64(i) * 4
		end := start + 4
		seg := generateSegment(rng, project.SamplesPerBeat, i, start, end, theme)
		project.Segments = append(project.Segments, seg)
	}
	return project
}

func generateSegment(rng *xorshift, samplesPerBeat, index int, start, end float64, theme Theme) domain.Segment {
	length := end - start
	tempo := round3(theme.BaseTempo + rng.floatRange(-7, 7))
	density := round3(math.Max(0.2, theme.BaseDensity+rng.floatRange(-0.35, 0.35)))
	shade := rng.floatRange(-0.04, 0.04)

	velocityMain := domain.Channel{
		ID:     "velocity.main",
		Name:   "Velocity Main",
		Target: domain.TargetVelocity,
		Layers: []domain.Layer{
			makeLayer("base", makeAnchors(rng, start, end, 0.42+shade, 0.18, 5)),
			makeLayer("phrase", makeAnchors(rng, start+length*0.08, end-length*0.05, 0.12, 0.10, 4)),
		},
	}
	velocityAccent := domain.Channel{
		ID:     "velocity.accent",
		Name:   "Velocity Accent Override",
		Target: domain.TargetVelocity,
		Layers: []domain.Layer{
			makeLayer("accents", []domain.ControlPoint{
				{Beat: round3(start), Value: round4(0.05 + rng.floatRange(0, 0.04))},
				{Beat: round3(start + length*0.25), Value: round4(0.16 + rng.floatRange(0, 0.06))},
				{Beat: round3(start + length*0.50), Value: round4(0.08 + rng.floatRange(0, 0.05))},
				{Beat: round3(start + length*0.75), Value: round4(0.20 + rng.floatRange(0, 0.07))},
				{Beat: round3(end), Value: round4(0.06 + rng.floatRange(0, 0.04))},
			}),
		},
	}
	vibratoMain := domain.Channel{
		ID:     "vibrato.main",
		Name:   "Vibrato Main",
		Target: domain.TargetVibrato,
		Layers: []domain.Layer{
			makeLayer("lfo", makeWave(rng, start, end, 0.18+shade, 0.08)),
			makeLayer("phrase", makeAnchors(rng, start, end, 0.10, 0.05, 4)),
		},
	}
	vibratoDelay := domain.Channel{
		ID:     "vibrato.delay",
		Name:   "Vibrato Delay Source",
		Target: domain.TargetVibrato,
		Layers: []domain.Layer{
			makeLayer("delayed", makeAnchors(rng, start+length*0.18, end, 0.11, 0.06, 4)),
		},
	}
	pitchbend := domain.Channel{
		ID:     "pitchbend.main",
		Name:   "Pitch Bend",
		Target: domain.TargetPitchbend,
		Layers: []domain.Layer{
			makeLayer("gesture", []domain.ControlPoint{
				{Beat: round3(start), Value: 0},
				{Beat: round3(start + length*0.35), Value: round4(rng.floatRange(-0.25, 0.25))},
				{Beat: round3(start + length*0.65), Value: round4(rng.floatRange(-0.25, 0.25))},
				{Beat: round3(end), Value: 0},
			}),
		},
	}
	sustain := domain.Channel{
		ID:     "sustain.main",
		Name:   "Sustain Pedal",
		Target: domain.TargetSustain,
		Layers: []domain.Layer{
			makeLayer("pedal", []domain.ControlPoint{
				{Beat: round3(start), Value: 0},
				{Beat: round3(start + length*0.12), Value: 1},
				{Beat: round3(start + length*0.58), Value: 0},
				{Beat: round3(start + length*0.76), Value: 1},
				{Beat: round3(end), Value: 0},
			}),
		},
	}
	priorities := []domain.PriorityRule{
		{Higher: "velocity.accent", Lower: "velocity.main", Basis: "accent-track-overrides-main"},
		{Higher: "vibrato.main", Lower: "vibrato.delay", Basis: "main-vibrato-overrides-delay"},
		{Higher: "vibrato.main", Lower: "velocity.accent", Basis: "expression-balance-rank"},
		{Higher: "pitchbend.main", Lower: "vibrato.main", Basis: "expression-balance-rank"},
		{Higher: "sustain.main", Lower: "pitchbend.main", Basis: "expression-balance-rank"},
	}
	seg := domain.Segment{
		ID:          fmt.Sprintf("seg-%02d", index+1),
		StartBeat:   start,
		EndBeat:     end,
		TempoBPM:    tempo,
		NoteDensity: density,
		Channels: []domain.Channel{
			velocityMain, velocityAccent, vibratoMain, vibratoDelay, pitchbend, sustain,
		},
		Priorities: priorities,
	}
	seg.Truth = buildTruth(seg, samplesPerBeat, priorities)
	return seg
}

func buildTruth(seg domain.Segment, samplesPerBeat int, priorities []domain.PriorityRule) domain.Truth {
	ranks := analyticRanks(seg.Channels, priorities)
	byTarget := make(map[string][]domain.Channel)
	for _, ch := range seg.Channels {
		byTarget[ch.Target] = append(byTarget[ch.Target], ch)
	}
	for target := range byTarget {
		sort.Slice(byTarget[target], func(i, j int) bool {
			a, b := byTarget[target][i], byTarget[target][j]
			if ranks[a.ID] != ranks[b.ID] {
				return ranks[a.ID] < ranks[b.ID]
			}
			return a.ID < b.ID
		})
	}
	targets := make([]string, 0, len(byTarget))
	for target := range byTarget {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	truth := domain.Truth{Interpolation: domain.InterpolationLinear, Tolerance: 1e-9}
	startTick := int(math.Ceil(seg.StartBeat*float64(samplesPerBeat) - 1e-9))
	endTick := int(math.Ceil(seg.EndBeat*float64(samplesPerBeat) - 1e-9))
	for tick := startTick; tick < endTick; tick++ {
		beat := float64(tick) / float64(samplesPerBeat)
		channelValues := make(map[string]float64, len(seg.Channels))
		for _, ch := range seg.Channels {
			chValue := 0.0
			for _, layer := range ch.Layers {
				chValue += analyticLinear(layer.Points, beat)
			}
			channelValues[ch.ID] = chValue
		}
		values := make(map[string]float64, len(targets))
		sources := make(map[string]string, len(targets))
		var decisions []domain.TruthDecision
		for _, target := range targets {
			candidates := byTarget[target]
			winner := candidates[0]
			winnerValue := channelValues[winner.ID]
			values[target] = winnerValue
			sources[target] = winner.ID
			for i := 0; i < len(candidates)-1; i++ {
				higher := candidates[i]
				higherValue := channelValues[higher.ID]
				for _, lower := range candidates[i+1:] {
					lowerValue := channelValues[lower.ID]
					if higherValue == lowerValue {
						continue
					}
					decisions = append(decisions, domain.TruthDecision{
						Tick:            tick,
						Beat:            beat,
						Target:          target,
						Higher:          higher.ID,
						Overridden:      lower.ID,
						OverriddenValue: lowerValue,
						Basis:           basisFor(priorities, higher.ID, lower.ID),
					})
				}
			}
		}
		sort.SliceStable(decisions, func(i, j int) bool {
			a, b := decisions[i], decisions[j]
			if a.Target != b.Target {
				return a.Target < b.Target
			}
			if a.Higher != b.Higher {
				return a.Higher < b.Higher
			}
			return a.Overridden < b.Overridden
		})
		truth.Samples = append(truth.Samples, domain.TruthSample{
			Tick:      tick,
			Beat:      beat,
			Values:    values,
			Sources:   sources,
			Decisions: decisions,
		})
	}
	return truth
}

func analyticRanks(channels []domain.Channel, rules []domain.PriorityRule) map[string]int {
	indegree := make(map[string]int, len(channels))
	adj := make(map[string][]string)
	for _, ch := range channels {
		indegree[ch.ID] = 0
	}
	for _, rule := range rules {
		adj[rule.Higher] = append(adj[rule.Higher], rule.Lower)
		indegree[rule.Lower]++
	}
	for id := range adj {
		sort.Strings(adj[id])
	}
	var ready []string
	for id, deg := range indegree {
		if deg == 0 {
			ready = append(ready, id)
		}
	}
	ranks := make(map[string]int, len(channels))
	for len(ready) > 0 {
		sort.Strings(ready)
		id := ready[0]
		ready = ready[1:]
		ranks[id] = len(ranks)
		for _, lower := range adj[id] {
			indegree[lower]--
			if indegree[lower] == 0 {
				ready = append(ready, lower)
			}
		}
	}
	return ranks
}

func basisFor(rules []domain.PriorityRule, higher, lower string) string {
	for _, rule := range rules {
		if rule.Higher == higher && rule.Lower == lower {
			return rule.Basis
		}
	}
	return fmt.Sprintf("transitive-priority:%s>%s", higher, lower)
}

func analyticLinear(points []domain.ControlPoint, beat float64) float64 {
	if beat <= points[0].Beat {
		return points[0].Value
	}
	last := points[len(points)-1]
	if beat >= last.Beat {
		return last.Value
	}
	for i := 1; i < len(points); i++ {
		right := points[i]
		if beat > right.Beat {
			continue
		}
		left := points[i-1]
		span := right.Beat - left.Beat
		if span <= 0 {
			return left.Value
		}
		t := (beat - left.Beat) / span
		return left.Value + (right.Value-left.Value)*t
	}
	return last.Value
}

func makeLayer(id string, points []domain.ControlPoint) domain.Layer {
	sort.Slice(points, func(i, j int) bool { return points[i].Beat < points[j].Beat })
	return domain.Layer{ID: id, Points: points}
}

func makeAnchors(rng *xorshift, start, end, base, spread float64, count int) []domain.ControlPoint {
	if count < 2 {
		count = 2
	}
	points := make([]domain.ControlPoint, 0, count)
	for i := 0; i < count; i++ {
		t := float64(i) / float64(count-1)
		beat := start + (end-start)*t
		points = append(points, domain.ControlPoint{
			Beat:  round3(clamp(beat, start, end)),
			Value: round4(math.Max(0, base+rng.floatRange(-spread, spread))),
		})
	}
	// Ensure a valid strict order even after 3-decimal rounding.
	for i := 1; i < len(points); i++ {
		if points[i].Beat <= points[i-1].Beat {
			points[i].Beat = points[i-1].Beat + 0.001
		}
	}
	return points
}

func makeWave(rng *xorshift, start, end, amplitude, spread float64) []domain.ControlPoint {
	points := make([]domain.ControlPoint, 0, 17)
	steps := 16
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		beat := start + (end-start)*t
		phase := float64(i) * 0.95
		value := amplitude + amplitude*0.72*math.Sin(phase) + rng.floatRange(-spread, spread)
		points = append(points, domain.ControlPoint{Beat: round4(beat), Value: round5(value)})
	}
	points[0].Beat = start
	points[len(points)-1].Beat = end
	return points
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }
func round5(v float64) float64 { return math.Round(v*100000) / 100000 }

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

type xorshift struct {
	state uint64
}

func newRNG(seed uint64) *xorshift {
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return &xorshift{state: seed}
}

func (r *xorshift) next() uint64 {
	x := r.state
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.state = x
	return x
}

func (r *xorshift) float64() float64 {
	return float64(r.next()>>11) / float64(uint64(1)<<53)
}

func (r *xorshift) floatRange(lo, hi float64) float64 {
	return lo + (hi-lo)*r.float64()
}
