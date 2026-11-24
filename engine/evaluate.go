package engine

import (
	"math"
	"sort"

	"performanceexpressions/workspace/domain"
)

func EvaluateProject(project domain.Project, params domain.Parameters) (domain.ResultSet, error) {
	if params.ParamVersion == "" {
		params.ParamVersion = domain.DefaultParamVersion
	}
	if params.AlgorithmVersion == "" {
		params.AlgorithmVersion = domain.AlgorithmVersion
	}
	if params.Interpolation == "" {
		params.Interpolation = domain.InterpolationLinear
	}
	if !KnownInterpolation(params.Interpolation) {
		return domain.ResultSet{}, domain.NewError(domain.CodeSchema, "unsupported interpolation mode %q", params.Interpolation)
	}
	if err := ValidateProject(project, ValidateOptions{RequireTruth: false}); err != nil {
		return domain.ResultSet{}, err
	}
	result := domain.ResultSet{
		ProjectID:        project.ID,
		ParamVersion:     params.ParamVersion,
		AlgorithmVersion: params.AlgorithmVersion,
		Interpolation:    params.Interpolation,
		Tolerance:        params.Tolerance,
		Segments:         make([]domain.SegmentResult, 0, len(project.Segments)),
	}
	for _, seg := range project.Segments {
		segResult, err := EvaluateSegment(project.SamplesPerBeat, seg, params)
		if err != nil {
			return domain.ResultSet{}, err
		}
		result.Segments = append(result.Segments, segResult)
	}
	return result, nil
}

func EvaluateSegment(samplesPerBeat int, seg domain.Segment, params domain.Parameters) (domain.SegmentResult, error) {
	topology, err := Topology(seg)
	if err != nil {
		return domain.SegmentResult{}, err
	}
	type orderedChannel struct {
		channel domain.Channel
		rank    int
	}
	byTarget := make(map[string][]orderedChannel)
	targets := make([]string, 0, len(seg.Channels))
	seenTarget := make(map[string]bool)
	for _, ch := range seg.Channels {
		rank := topology.Ranks[ch.ID]
		byTarget[ch.Target] = append(byTarget[ch.Target], orderedChannel{channel: ch, rank: rank})
		if !seenTarget[ch.Target] {
			seenTarget[ch.Target] = true
			targets = append(targets, ch.Target)
		}
	}
	sort.Strings(targets)
	for target := range byTarget {
		sort.Slice(byTarget[target], func(i, j int) bool {
			a, b := byTarget[target][i], byTarget[target][j]
			if a.rank != b.rank {
				return a.rank < b.rank
			}
			return a.channel.ID < b.channel.ID
		})
	}

	startTick := int(math.Ceil(seg.StartBeat*float64(samplesPerBeat) - 1e-9))
	endTick := int(math.Ceil(seg.EndBeat*float64(samplesPerBeat) - 1e-9))
	samples := make([]domain.SampleResult, 0, max(0, endTick-startTick))
	for tick := startTick; tick < endTick; tick++ {
		beat := float64(tick) / float64(samplesPerBeat)
		channelValues := make(map[string]float64, len(seg.Channels))
		for _, ch := range seg.Channels {
			value, err := ChannelValue(ch, beat, params.Interpolation)
			if err != nil {
				return domain.SegmentResult{}, err
			}
			channelValues[ch.ID] = value
		}
		values := make(map[string]float64, len(targets))
		sources := make(map[string]domain.SourceRef, len(targets))
		var decisions []domain.Decision
		for _, target := range targets {
			candidates := byTarget[target]
			if len(candidates) == 0 {
				continue
			}
			winner := candidates[0]
			winnerValue := channelValues[winner.channel.ID]
			values[target] = winnerValue
			sources[target] = domain.SourceRef{
				Channel:  winner.channel.ID,
				Priority: winner.rank,
				Value:    winnerValue,
			}
			for i := 0; i < len(candidates)-1; i++ {
				higher := candidates[i]
				higherValue := channelValues[higher.channel.ID]
				for _, lower := range candidates[i+1:] {
					lowerValue := channelValues[lower.channel.ID]
					if higherValue == lowerValue {
						continue
					}
					decisions = append(decisions, domain.Decision{
						Tick:            tick,
						Beat:            beat,
						Target:          target,
						Channel:         higher.channel.ID,
						Overridden:      lower.channel.ID,
						OverriddenValue: lowerValue,
						Basis:           PriorityBasis(seg, higher.channel.ID, lower.channel.ID),
					})
				}
			}
		}
		sort.SliceStable(decisions, func(i, j int) bool {
			a, b := decisions[i], decisions[j]
			if a.Target != b.Target {
				return a.Target < b.Target
			}
			if a.Channel != b.Channel {
				return a.Channel < b.Channel
			}
			return a.Overridden < b.Overridden
		})
		samples = append(samples, domain.SampleResult{
			Tick:      tick,
			Beat:      beat,
			Values:    values,
			Sources:   sources,
			Decisions: decisions,
		})
	}
	return domain.SegmentResult{
		SegmentID:       seg.ID,
		StartBeat:       seg.StartBeat,
		EndBeat:         seg.EndBeat,
		PriorityVersion: domain.PriorityVersion(seg),
		Samples:         samples,
	}, nil
}

func PlaybackAt(project domain.Project, params domain.Parameters, segmentID string, beat float64) (domain.SampleResult, error) {
	var seg *domain.Segment
	for i := range project.Segments {
		if project.Segments[i].ID == segmentID {
			seg = &project.Segments[i]
			break
		}
	}
	if seg == nil {
		return domain.SampleResult{}, domain.NewError(domain.CodeSchema, "unknown segment %q", segmentID).WithSegment(segmentID)
	}
	if beat < seg.StartBeat || beat >= seg.EndBeat {
		return domain.SampleResult{}, domain.NewError(domain.CodePosition, "playback beat is outside segment").WithSegment(segmentID).WithBeat(beat)
	}
	topology, err := Topology(*seg)
	if err != nil {
		return domain.SampleResult{}, err
	}
	channelValues := make(map[string]float64, len(seg.Channels))
	for _, ch := range seg.Channels {
		v, err := ChannelValue(ch, beat, params.Interpolation)
		if err != nil {
			return domain.SampleResult{}, err
		}
		channelValues[ch.ID] = v
	}
	targetSet := make(map[string]bool)
	for _, ch := range seg.Channels {
		targetSet[ch.Target] = true
	}
	targets := make([]string, 0, len(targetSet))
	for target := range targetSet {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	values := make(map[string]float64, len(targets))
	sources := make(map[string]domain.SourceRef, len(targets))
	for _, target := range targets {
		bestChannel := ""
		bestRank := math.MaxInt
		for _, ch := range seg.Channels {
			if ch.Target != target {
				continue
			}
			if topology.Ranks[ch.ID] < bestRank || (topology.Ranks[ch.ID] == bestRank && ch.ID < bestChannel) {
				bestChannel = ch.ID
				bestRank = topology.Ranks[ch.ID]
			}
		}
		values[target] = channelValues[bestChannel]
		sources[target] = domain.SourceRef{Channel: bestChannel, Priority: bestRank, Value: channelValues[bestChannel]}
	}
	tick := int(math.Round(beat * float64(project.SamplesPerBeat)))
	return domain.SampleResult{Tick: tick, Beat: beat, Values: values, Sources: sources}, nil
}
