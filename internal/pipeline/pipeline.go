package pipeline

import (
	"encoding/json"
	"fmt"

	"mocapworkbench/internal/checkpoint"
	"mocapworkbench/internal/model"
	"mocapworkbench/internal/retarget"
)

type Options struct {
	StorePath string
	StopAfter int
	ForceFull bool
}

func Run(clip model.Clip, target model.TargetSkeleton, cfg model.Config, opts Options) (*model.Animation, error) {
	if opts.StorePath == "" {
		return retarget.Run(clip, target, cfg)
	}
	plan, err := retarget.ChoosePlan(clip, target, cfg)
	if err != nil {
		return nil, err
	}
	planJSON, _ := json.Marshal(plan)
	meta := checkpoint.SnapshotMeta{
		ClipID: clip.ID, TargetID: target.ID,
		ParameterVersion: retarget.ParameterVersion(clip, target, cfg),
		AlgorithmVersion: model.AlgorithmVersion, PlanJSON: string(planJSON),
	}
	store, err := checkpoint.Open(opts.StorePath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	prepare := checkpoint.PrepareResult{Action: "fresh", Reason: "forced full recompute"}
	if opts.ForceFull {
		if err := store.Reset("forced full recompute"); err != nil {
			return nil, err
		}
		prepare, err = store.Prepare(meta)
	} else {
		prepare, err = store.Prepare(meta)
	}
	if err != nil {
		return nil, err
	}
	animation, err := retarget.Run(clip, target, cfg)
	if err != nil {
		return nil, err
	}
	committed := len(prepare.Frames)
	if committed > 0 {
		if diff := FirstDiff(prepare.Frames, animation.Frames[:committed], target); diff != nil {
			if err := store.Reset("checkpoint prefix diverged from deterministic recompute"); err != nil {
				return nil, err
			}
			prepare, err = store.Prepare(meta)
			if err != nil {
				return nil, err
			}
			prepare.Action = "recomputed"
			prepare.Reason = fmt.Sprintf("frame %d %s.%s diverged; whole snapshot discarded", diff.Frame, diff.Joint, diff.Field)
			committed = 0
		}
	}
	states := checkpoint.NewStateMachine(len(animation.Frames))
	states.MarkCommittedPrefix(committed)
	limit := len(animation.Frames)
	if opts.StopAfter > 0 && opts.StopAfter < limit {
		limit = opts.StopAfter
	}
	for frame := committed; frame < limit; frame++ {
		if err := states.Transition(frame, checkpoint.StateSolving); err != nil {
			return nil, err
		}
		if err := store.Append(animation.Frames[frame], meta); err != nil {
			_ = states.Transition(frame, checkpoint.StateFailed)
			return nil, err
		}
		if err := states.Transition(frame, checkpoint.StateCommitted); err != nil {
			return nil, err
		}
	}
	animation.ResumedFrames = committed
	animation.SnapshotAction = prepare.Action
	animation.SnapshotReason = prepare.Reason
	if limit < len(animation.Frames) {
		animation.SnapshotAction = "interrupted"
		animation.SnapshotReason = fmt.Sprintf("normal stop after %d committed frames; %s", limit, prepare.Reason)
		animation.Frames = append([]model.FrameResult(nil), animation.Frames[:limit]...)
		animation.ContactFrames = contactFrames(animation.Frames)
		animation.RootEnd = animation.Frames[len(animation.Frames)-1].RootTranslation
		animation.Summary = Summarize(animation.Frames)
	}
	return animation, nil
}

func Summarize(frames []model.FrameResult) model.Summary {
	summary := model.Summary{}
	for _, frame := range frames {
		if frame.Stats.Residual > summary.MaxResidual {
			summary.MaxResidual = frame.Stats.Residual
		}
		if frame.Stats.AngleViolationDeg > summary.MaxAngleViolation {
			summary.MaxAngleViolation = frame.Stats.AngleViolationDeg
		}
		if frame.Stats.ReachViolation > summary.MaxReachViolation {
			summary.MaxReachViolation = frame.Stats.ReachViolation
		}
		if frame.Stats.ContactDrift > summary.MaxContactDrift {
			summary.MaxContactDrift = frame.Stats.ContactDrift
		}
		summary.DecisionCount += len(frame.Decisions)
	}
	return summary
}

func contactFrames(frames []model.FrameResult) []int {
	var out []int
	for _, frame := range frames {
		if len(frame.Contacts) > 0 {
			out = append(out, frame.Frame)
		}
	}
	return out
}

type Difference struct {
	Frame int     `json:"frame"`
	Joint string  `json:"joint,omitempty"`
	Field string  `json:"field"`
	A     float64 `json:"a"`
	B     float64 `json:"b"`
	AText string  `json:"a_text,omitempty"`
	BText string  `json:"b_text,omitempty"`
}

// FirstDiff compares frame rotations, root translation, contact sets and
// constraint statistics in a stable field order.
func FirstDiff(a, b []model.FrameResult, target model.TargetSkeleton) *Difference {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for f := 0; f < n; f++ {
		if len(a[f].Rotations) != len(b[f].Rotations) {
			return &Difference{Frame: f, Field: "rotation_count", A: float64(len(a[f].Rotations)), B: float64(len(b[f].Rotations))}
		}
		for j := range a[f].Rotations {
			joint := ""
			if j < len(target.Joints) {
				joint = target.Joints[j].Name
			}
			qa, qb := a[f].Rotations[j], b[f].Rotations[j]
			checks := []struct {
				field string
				av    float64
				bv    float64
			}{
				{"rotations." + joint + ".x", qa.X, qb.X},
				{"rotations." + joint + ".y", qa.Y, qb.Y},
				{"rotations." + joint + ".z", qa.Z, qb.Z},
				{"rotations." + joint + ".w", qa.W, qb.W},
			}
			for _, check := range checks {
				if check.av != check.bv {
					return &Difference{Frame: f, Joint: joint, Field: check.field, A: check.av, B: check.bv}
				}
			}
		}
		ra, rb := a[f].RootTranslation, b[f].RootTranslation
		rootChecks := []struct {
			field string
			av    float64
			bv    float64
		}{
			{"root_translation.x", ra.X, rb.X},
			{"root_translation.y", ra.Y, rb.Y},
			{"root_translation.z", ra.Z, rb.Z},
		}
		for _, check := range rootChecks {
			if check.av != check.bv {
				return &Difference{Frame: f, Field: check.field, A: check.av, B: check.bv}
			}
		}
		if !equalStrings(a[f].Contacts, b[f].Contacts) {
			return &Difference{
				Frame: f, Field: "contacts",
				A: float64(len(a[f].Contacts)), B: float64(len(b[f].Contacts)),
				AText: join(a[f].Contacts), BText: join(b[f].Contacts),
			}
		}
		sa, sb := a[f].Stats, b[f].Stats
		stats := []struct {
			field string
			av    float64
			bv    float64
		}{
			{"stats.angle_violation_deg", sa.AngleViolationDeg, sb.AngleViolationDeg},
			{"stats.reach_violation", sa.ReachViolation, sb.ReachViolation},
			{"stats.contact_drift", sa.ContactDrift, sb.ContactDrift},
			{"stats.residual", sa.Residual, sb.Residual},
			{"stats.iterations", float64(sa.Iterations), float64(sb.Iterations)},
		}
		for _, check := range stats {
			if check.av != check.bv {
				return &Difference{Frame: f, Field: check.field, A: check.av, B: check.bv}
			}
		}
	}
	if len(a) != len(b) {
		return &Difference{Frame: n, Field: "frame_count", A: float64(len(a)), B: float64(len(b))}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func join(values []string) string {
	out := ""
	for i, value := range values {
		if i > 0 {
			out += ","
		}
		out += value
	}
	return out
}
