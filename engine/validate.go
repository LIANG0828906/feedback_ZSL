package engine

import (
	"container/heap"
	"fmt"
	"math"
	"sort"

	"performanceexpressions/workspace/domain"
)

type ValidateOptions struct {
	RequireTruth bool
}

func ValidateProject(project domain.Project, opts ValidateOptions) error {
	if project.SchemaVersion == "" {
		return domain.NewError(domain.CodeSchema, "schema_version is required")
	}
	if project.SchemaVersion != domain.SchemaVersion {
		return domain.NewError(domain.CodeSchema, "unsupported schema_version %q", project.SchemaVersion)
	}
	if project.ID == "" {
		return domain.NewError(domain.CodeSchema, "project id is required")
	}
	if project.Name == "" {
		return domain.NewError(domain.CodeSchema, "project name is required")
	}
	if project.SamplesPerBeat <= 0 {
		return domain.NewError(domain.CodeSchema, "samples_per_beat must be positive")
	}
	if !finite(project.DurationBeats) || project.DurationBeats <= 0 {
		return domain.NewError(domain.CodeSchema, "duration_beats must be positive and finite")
	}
	if len(project.Segments) == 0 {
		return domain.NewError(domain.CodeSchema, "at least one segment is required")
	}
	seenSegments := make(map[string]struct{}, len(project.Segments))
	for _, seg := range project.Segments {
		if _, exists := seenSegments[seg.ID]; exists {
			return domain.NewError(domain.CodeSchema, "duplicate segment id %q", seg.ID).WithSegment(seg.ID)
		}
		seenSegments[seg.ID] = struct{}{}
		if err := ValidateSegment(seg, opts); err != nil {
			return err
		}
		if seg.EndBeat > project.DurationBeats+1e-9 {
			return domain.NewError(domain.CodePosition, "segment end exceeds project duration").WithSegment(seg.ID).WithBeat(seg.EndBeat)
		}
	}
	return nil
}

func ValidateSegment(seg domain.Segment, opts ValidateOptions) error {
	if seg.ID == "" {
		return domain.NewError(domain.CodeSchema, "segment id is required")
	}
	if !finite(seg.StartBeat) || !finite(seg.EndBeat) || seg.EndBeat <= seg.StartBeat {
		return domain.NewError(domain.CodeSchema, "segment range is invalid").WithSegment(seg.ID)
	}
	if !finite(seg.TempoBPM) || seg.TempoBPM <= 0 {
		return domain.NewError(domain.CodeSchema, "tempo_bpm must be positive").WithSegment(seg.ID)
	}
	if !finite(seg.NoteDensity) || seg.NoteDensity < 0 {
		return domain.NewError(domain.CodeSchema, "note_density must be non-negative").WithSegment(seg.ID)
	}
	if len(seg.Channels) == 0 {
		return domain.NewError(domain.CodeSchema, "segment has no expression channels").WithSegment(seg.ID)
	}

	channelIDs := make(map[string]struct{}, len(seg.Channels))
	for _, ch := range seg.Channels {
		if ch.ID == "" {
			return domain.NewError(domain.CodeSchema, "channel id is required").WithSegment(seg.ID)
		}
		if ch.Name == "" {
			return domain.NewError(domain.CodeSchema, "channel name is required").WithSegment(seg.ID).WithChannel(ch.ID)
		}
		if ch.Target == "" {
			return domain.NewError(domain.CodeSchema, "channel target is required").WithSegment(seg.ID).WithChannel(ch.ID)
		}
		if !knownTarget(ch.Target) {
			return domain.NewError(domain.CodeSchema, "unknown expression target %q", ch.Target).WithSegment(seg.ID).WithChannel(ch.ID)
		}
		if _, exists := channelIDs[ch.ID]; exists {
			return domain.NewError(domain.CodeSchema, "duplicate channel id %q", ch.ID).WithSegment(seg.ID).WithChannel(ch.ID)
		}
		channelIDs[ch.ID] = struct{}{}
		if len(ch.Layers) == 0 {
			return domain.NewError(domain.CodeSchema, "channel has no expression layers").WithSegment(seg.ID).WithChannel(ch.ID)
		}
		layerIDs := make(map[string]struct{}, len(ch.Layers))
		for _, layer := range ch.Layers {
			if layer.ID == "" {
				return domain.NewError(domain.CodeSchema, "layer id is required").WithSegment(seg.ID).WithChannel(ch.ID)
			}
			if _, exists := layerIDs[layer.ID]; exists {
				return domain.NewError(domain.CodeSchema, "duplicate layer id %q in channel", layer.ID).
					WithSegment(seg.ID).WithChannel(ch.ID).WithLayer(layer.ID)
			}
			layerIDs[layer.ID] = struct{}{}
			if len(layer.Points) == 0 {
				return domain.NewError(domain.CodeSchema, "layer has no control points").
					WithSegment(seg.ID).WithChannel(ch.ID).WithLayer(layer.ID)
			}
			prev := math.Inf(-1)
			for pointIndex, point := range layer.Points {
				if !finite(point.Beat) || !finite(point.Value) {
					return domain.NewError(domain.CodeSchema, "control point %d is not finite", pointIndex).
						WithSegment(seg.ID).WithChannel(ch.ID).WithLayer(layer.ID)
				}
				if point.Beat < seg.StartBeat-1e-9 || point.Beat > seg.EndBeat+1e-9 {
					return domain.NewError(domain.CodePosition, "control point beat is outside segment range").
						WithSegment(seg.ID).WithChannel(ch.ID).WithLayer(layer.ID).WithBeat(point.Beat)
				}
				if point.Beat <= prev {
					return domain.NewError(domain.CodePosition, "control point beats must be strictly increasing").
						WithSegment(seg.ID).WithChannel(ch.ID).WithLayer(layer.ID).WithBeat(point.Beat)
				}
				prev = point.Beat
			}
		}
	}

	if err := validatePriorities(seg, channelIDs); err != nil {
		return err
	}
	if opts.RequireTruth {
		if err := validateTruth(seg); err != nil {
			return err
		}
	}
	return nil
}

func validatePriorities(seg domain.Segment, channelIDs map[string]struct{}) error {
	if len(seg.Priorities) == 0 && len(channelIDs) == 1 {
		return nil
	}
	covered := make(map[string]bool, len(channelIDs))
	for _, rule := range seg.Priorities {
		if rule.Higher == "" || rule.Lower == "" {
			return domain.NewError(domain.CodePriority, "priority rule must name higher and lower channels").WithSegment(seg.ID)
		}
		if _, ok := channelIDs[rule.Higher]; !ok {
			return domain.NewError(domain.CodePriority, "priority rule references unknown channel %q", rule.Higher).
				WithSegment(seg.ID).WithChannel(rule.Higher)
		}
		if _, ok := channelIDs[rule.Lower]; !ok {
			return domain.NewError(domain.CodePriority, "priority rule references unknown channel %q", rule.Lower).
				WithSegment(seg.ID).WithChannel(rule.Lower)
		}
		if rule.Higher == rule.Lower {
			return domain.NewError(domain.CodeCycle, "self priority edge is a cycle").
				WithSegment(seg.ID).WithChannel(rule.Higher)
		}
		covered[rule.Higher] = true
		covered[rule.Lower] = true
	}
	var missing []string
	for id := range channelIDs {
		if !covered[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		err := domain.NewError(domain.CodePriority, "priority declarations do not cover all channels: %v", missing).
			WithSegment(seg.ID)
		err.Channels = missing
		err.ChannelID = missing[0]
		return err
	}
	if _, err := Topology(seg); err != nil {
		return err
	}
	return nil
}

func validateTruth(seg domain.Segment) error {
	if seg.Truth.Interpolation == "" {
		return domain.NewError(domain.CodeSchema, "truth interpolation is required").WithSegment(seg.ID)
	}
	if !KnownInterpolation(seg.Truth.Interpolation) {
		return domain.NewError(domain.CodeSchema, "unsupported truth interpolation %q", seg.Truth.Interpolation).WithSegment(seg.ID)
	}
	if !finite(seg.Truth.Tolerance) || seg.Truth.Tolerance < 0 {
		return domain.NewError(domain.CodeSchema, "truth tolerance must be non-negative").WithSegment(seg.ID)
	}
	if len(seg.Truth.Samples) == 0 {
		return domain.NewError(domain.CodeSchema, "truth samples are required").WithSegment(seg.ID)
	}
	prevTick := -1
	for _, sample := range seg.Truth.Samples {
		if sample.Tick < prevTick {
			return domain.NewError(domain.CodePosition, "truth samples must be ordered by tick").WithSegment(seg.ID)
		}
		if sample.Beat < seg.StartBeat-1e-9 || sample.Beat >= seg.EndBeat+1e-9 {
			return domain.NewError(domain.CodePosition, "truth sample beat is outside segment range").
				WithSegment(seg.ID).WithBeat(sample.Beat)
		}
		if len(sample.Values) == 0 || len(sample.Sources) == 0 {
			return domain.NewError(domain.CodeSchema, "truth sample values and sources are required").WithSegment(seg.ID)
		}
		for target, value := range sample.Values {
			if !knownTarget(target) || !finite(value) {
				return domain.NewError(domain.CodeSchema, "truth value for target %q is invalid", target).WithSegment(seg.ID)
			}
		}
		for target, source := range sample.Sources {
			if !knownTarget(target) {
				return domain.NewError(domain.CodeSchema, "truth source target %q is invalid", target).WithSegment(seg.ID)
			}
			ch, _, ok := seg.ChannelByID(source)
			if !ok {
				return domain.NewError(domain.CodeSchema, "truth source references unknown channel %q", source).
					WithSegment(seg.ID).WithChannel(source)
			}
			if ch.Target != target {
				return domain.NewError(domain.CodeSchema, "truth source channel %q does not target %q", source, target).
					WithSegment(seg.ID).WithChannel(source)
			}
		}
		prevTick = sample.Tick
	}
	return nil
}

func knownTarget(target string) bool {
	switch target {
	case domain.TargetVelocity, domain.TargetVibrato, domain.TargetPitchbend, domain.TargetSustain:
		return true
	default:
		return false
	}
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func KnownInterpolation(mode string) bool {
	switch mode {
	case domain.InterpolationLinear, domain.InterpolationHold, domain.InterpolationSmoothstep:
		return true
	default:
		return false
	}
}

type PriorityTopology struct {
	Ranks map[string]int
	Order []string
}

type channelHeap []string

func (h channelHeap) Len() int           { return len(h) }
func (h channelHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h channelHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *channelHeap) Push(x any)        { *h = append(*h, x.(string)) }
func (h *channelHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// Topology produces a deterministic global channel rank. Ties are broken by
// channel ID, never by map iteration order.
func Topology(seg domain.Segment) (PriorityTopology, error) {
	indegree := make(map[string]int, len(seg.Channels))
	adj := make(map[string][]string, len(seg.Channels))
	for _, ch := range seg.Channels {
		indegree[ch.ID] = 0
	}
	for _, rule := range seg.Priorities {
		if _, ok := indegree[rule.Higher]; !ok {
			continue
		}
		if _, ok := indegree[rule.Lower]; !ok {
			continue
		}
		adj[rule.Higher] = append(adj[rule.Higher], rule.Lower)
		indegree[rule.Lower]++
	}
	for id := range adj {
		sort.Strings(adj[id])
	}
	h := &channelHeap{}
	heap.Init(h)
	for id, deg := range indegree {
		if deg == 0 {
			heap.Push(h, id)
		}
	}
	order := make([]string, 0, len(seg.Channels))
	ranks := make(map[string]int, len(seg.Channels))
	for h.Len() > 0 {
		id := heap.Pop(h).(string)
		ranks[id] = len(order)
		order = append(order, id)
		for _, lower := range adj[id] {
			indegree[lower]--
			if indegree[lower] == 0 {
				heap.Push(h, lower)
			}
		}
	}
	if len(order) != len(seg.Channels) {
		cycle := findCycle(seg)
		sort.Strings(cycle)
		err := domain.NewError(domain.CodeCycle, "priority graph contains a cycle involving %v", cycle).WithSegment(seg.ID)
		err.Channels = cycle
		return PriorityTopology{}, err
	}
	return PriorityTopology{Ranks: ranks, Order: order}, nil
}

func findCycle(seg domain.Segment) []string {
	adj := make(map[string][]string)
	for _, rule := range seg.Priorities {
		adj[rule.Higher] = append(adj[rule.Higher], rule.Lower)
	}
	for id := range adj {
		sort.Strings(adj[id])
	}
	const (
		unseen = iota
		visiting
		done
	)
	state := make(map[string]int)
	var stack []string
	var cycle []string
	var visit func(string) bool
	visit = func(id string) bool {
		state[id] = visiting
		stack = append(stack, id)
		for _, next := range adj[id] {
			if state[next] == visiting {
				for i := len(stack) - 1; i >= 0; i-- {
					cycle = append(cycle, stack[i])
					if stack[i] == next {
						break
					}
				}
				return true
			}
			if state[next] == unseen && visit(next) {
				return true
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = done
		return false
	}
	ids := make([]string, 0, len(seg.Channels))
	for _, ch := range seg.Channels {
		ids = append(ids, ch.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if state[id] == unseen && visit(id) {
			break
		}
	}
	if len(cycle) == 0 {
		return []string{"<unknown>"}
	}
	return cycle
}

func PriorityBasis(seg domain.Segment, higher, lower string) string {
	var bases []string
	for _, rule := range seg.Priorities {
		if rule.Higher == higher && rule.Lower == lower {
			if rule.Basis != "" {
				bases = append(bases, rule.Basis)
			} else {
				bases = append(bases, fmt.Sprintf("declared:%s>%s", higher, lower))
			}
		}
	}
	if len(bases) > 0 {
		sort.Strings(bases)
		return bases[0]
	}
	return fmt.Sprintf("transitive-priority:%s>%s", higher, lower)
}
