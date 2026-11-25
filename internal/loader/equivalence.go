package loader

import (
	"fmt"

	"audiobus/domain"
	"audiobus/errs"
	"audiobus/internal/convert"
)

func CrossBranchEquivalent(set *domain.SampleSet) error {
	byBranch := map[string]map[int64]domain.Event{}
	for _, e := range set.Events {
		if byBranch[e.SourceBranch] == nil {
			byBranch[e.SourceBranch] = map[int64]domain.Event{}
		}
		if _, exists := byBranch[e.SourceBranch][e.ID]; exists {
			continue
		}
		byBranch[e.SourceBranch][e.ID] = e
	}
	alpha := byBranch["alpha"]
	for _, branch := range []string{"beta", "gamma"} {
		other := byBranch[branch]
		for id, a := range alpha {
			b, ok := other[id]
			if !ok {
				return equivalenceError(a, branch, "missing event")
			}
			if a.Channel != b.Channel || a.Start != b.Start || a.Duration != b.Duration || a.Gain != b.Gain ||
				a.Priority != b.Priority || a.NoInterrupt != b.NoInterrupt || a.Conditional != b.Conditional || a.FlagTrue != b.FlagTrue {
				return equivalenceError(a, branch, fmt.Sprintf("semantic mismatch alpha={ch%d start%d dur%d gain%d pr%d ni%v cond%v flag%v} %s={ch%d start%d dur%d gain%d pr%d ni%v cond%v flag%v}",
					a.Channel, a.Start, a.Duration, a.Gain, a.Priority, a.NoInterrupt, a.Conditional, a.FlagTrue,
					branch, b.Channel, b.Start, b.Duration, b.Gain, b.Priority, b.NoInterrupt, b.Conditional, b.FlagTrue))
			}
		}
	}
	return nil
}

func equivalenceError(e domain.Event, branch, reason string) error {
	return &errs.Error{Code: errs.EDiff, Branch: branch, EventID: e.ID, Channel: e.Channel, Frame: e.Start, Reason: reason}
}

func RoundTripConversion(e domain.Event, m domain.Manifest) (domain.Event, error) {
	frame, err := convert.SamplesToFrame(e.Start, FrameSamples, convert.Floor)
	if err != nil {
		return e, err
	}
	start, err2 := convert.FrameToSamples(frame, FrameSamples)
	if err2 != nil {
		return e, err2
	}
	if start != e.Start {
		return e, &errs.Error{Code: errs.EDiff, EventID: e.ID, Channel: e.Channel, First: e.Start, Reason: "high-low-high start round trip changed"}
	}
	durationFrame, err := convert.SamplesToFrame(e.Duration, FrameSamples, convert.Floor)
	if err != nil {
		return e, err
	}
	duration, err2 := convert.FrameToSamples(durationFrame, FrameSamples)
	if err2 != nil {
		return e, err2
	}
	if duration != e.Duration {
		return e, &errs.Error{Code: errs.EDiff, EventID: e.ID, Channel: e.Channel, First: e.Duration, Reason: "high-low-high duration round trip changed"}
	}
	return e, nil
}
