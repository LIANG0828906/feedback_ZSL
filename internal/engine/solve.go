package engine

import (
	"fmt"
	"math"
	"sort"

	"combatjudge/internal/conv"
	"combatjudge/internal/geom"
	"combatjudge/internal/model"
)

func BuildIntervals(project *model.Project, size int) []IntervalSpec {
	if size <= 0 {
		size = DefaultIntervalFrames
	}
	minFrame, maxFrame := 1, 1
	if len(project.Requests) > 0 {
		minFrame = project.Requests[0].Frame
		maxFrame = project.Requests[0].Frame
		for _, r := range project.Requests {
			minFrame = min(minFrame, r.Frame)
			maxFrame = max(maxFrame, r.Frame)
		}
	} else {
		for _, s := range project.Sweeps {
			minFrame = min(minFrame, s.StartFrame)
			maxFrame = max(maxFrame, s.EndFrame)
		}
	}
	out := []IntervalSpec{}
	for start := minFrame; start <= maxFrame; start += size {
		end := min(start+size-1, maxFrame)
		out = append(out, IntervalSpec{No: len(out) + 1, StartFrame: start, EndFrame: end})
	}
	return out
}

func requestsInInterval(project *model.Project, spec IntervalSpec) []model.FrameRequest {
	out := []model.FrameRequest{}
	for _, r := range project.Requests {
		if r.Frame >= spec.StartFrame && r.Frame <= spec.EndFrame {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Frame != out[j].Frame {
			return out[i].Frame < out[j].Frame
		}
		return out[i].SweepID < out[j].SweepID
	})
	return out
}

func positionAt(s model.Sweep, frame int, version string) (model.Vec3, []TruncationRecord, error) {
	truncs := []TruncationRecord{}
	if frame <= s.StartFrame {
		return s.From, truncs, nil
	}
	if frame >= s.EndFrame {
		return s.To, truncs, nil
	}
	den := int64(s.EndFrame - s.StartFrame)
	num := int64(frame - s.StartFrame)
	out := model.Vec3{}
	axes := []struct {
		name string
		d    int64
		set  func(int64)
		add  func(model.Vec3, int64) (model.Vec3, error)
	}{
		{"x", s.To.X - s.From.X, func(v int64) { out.X = v }, nil},
	}
	_ = axes

	values := [3]int64{}
	from := s.From.Array()
	to := s.To.Array()
	names := [3]string{"x", "y", "z"}
	current := s.From
	for axis := 0; axis < 3; axis++ {
		delta := to[axis] - from[axis]
		offset, remainder, err := conv.MulDiv(fmt.Sprintf("$.sweeps.%s.from.%s", s.ID, names[axis]), version, delta, num, den, conv.RoundFloor)
		if err != nil {
			return model.Vec3{}, nil, err
		}
		switch axis {
		case 0:
			current.X, err = conv.CheckedAdd("position."+names[axis], version, s.From.X, offset)
			values[0] = current.X
		case 1:
			current.Y, err = conv.CheckedAdd("position."+names[axis], version, s.From.Y, offset)
			values[1] = current.Y
		default:
			current.Z, err = conv.CheckedAdd("position."+names[axis], version, s.From.Z, offset)
			values[2] = current.Z
		}
		if err != nil {
			return model.Vec3{}, nil, err
		}
		truncs = append(truncs, TruncationRecord{
			Frame: frame, SweepID: s.ID, FieldPath: fmt.Sprintf("sweep.%s.position.%s", s.ID, names[axis]), Axis: names[axis],
			Op: "integer_interpolate_floor", ValueQ: offset, Remainder: remainder, Denominator: den,
			Position: current, RuleVersion: model.TransformRuleVersion,
		})
	}
	return current, truncs, nil
}

func broadphaseCandidate(seg geom.Segment, sweepRadiusQ int64, c model.Collider) bool {
	sMin, sMax := geom.BoundsFromSegment(seg)
	weaponR := float64(sweepRadiusQ+DependencyMarginQ) / float64(model.InternalScale)
	for i := range sMin {
		sMin[i] -= weaponR
		sMax[i] += weaponR
	}
	cMin, cMax := geom.ColliderBounds(c)
	return geom.ExpandedBoundsIntersect(sMin, sMax, cMin, cMax)
}

func SolveIntervalGeometry(project *model.Project, spec IntervalSpec, injection Injection, buf *GeomBuffer) (*GeometryResult, error) {
	if buf == nil {
		buf = NewGeomBuffer(DefaultBufferBytes)
	}
	if err := buf.Resize(max(1, len(project.Colliders))); err != nil {
		return nil, err
	}
	result := &GeometryResult{IntervalNo: spec.No, Frames: []FrameGeometry{}}
	for _, req := range requestsInInterval(project, spec) {
		sweep := project.SweepByID(req.SweepID)
		if sweep == nil {
			return nil, model.NewError(model.CodeSchema, "$.requests", project.SchemaVersion, "unknown sweep id %q", req.SweepID)
		}
		cur, truncs, err := positionAt(*sweep, req.Frame, project.SchemaVersion)
		if err != nil {
			return nil, err
		}
		prevFrame := req.Frame - 1
		if prevFrame < sweep.StartFrame {
			prevFrame = sweep.StartFrame
		}
		prev, _, err := positionAt(*sweep, prevFrame, project.SchemaVersion)
		if err != nil {
			return nil, err
		}
		moveSeg := geom.SegmentFromModel(prev, cur)
		fg := FrameGeometry{
			Frame: req.Frame, SweepID: sweep.ID, AtMS: req.AtMS,
			MovementFrom: prev, MovementTo: cur, Truncations: truncs,
			Contacts: []ContactResult{}, NearestDistanceQ: -1,
		}
		for i, c := range project.Colliders {
			if !broadphaseCandidate(moveSeg, sweep.Radius, c) {
				continue
			}
			if injection.CorruptColliderNo == c.Index {
				return nil, &model.Error{Code: model.CodeDecode, FieldPath: fmt.Sprintf("$.colliders[%d].payload", i), SourceVersion: project.SchemaVersion, IntervalIndex: spec.No, Message: "injected collider payload corruption/checksum failure"}
			}
			contact := geom.Analyze(moveSeg, sweep.Radius, c)
			buf.floats[i] = contact.SurfaceGap
			distanceQ := geom.QuantizeDistance(contact.SurfaceGap)
			if distanceQ <= HitToleranceQ {
				distanceQ = 0
			}
			toiQ := int64(contact.TOI * float64(model.InternalScale))
			fg.Truncations = append(fg.Truncations, TruncationRecord{
				Frame: req.Frame, SweepID: sweep.ID, FieldPath: fmt.Sprintf("contact.%d.toi", c.Index),
				Op: "toi_fixed_truncate", ValueQ: toiQ, Denominator: model.InternalScale,
				Remainder: int64((contact.TOI*1e6 - float64(toiQ)) * 1e6), RuleVersion: model.AlgoVersion,
			})
			cr := ContactResult{
				ColliderNo: c.Index, ColliderID: c.ID, Part: c.Part, Surface: contact.Surface,
				TOI: contact.TOI, TOIQ: toiQ, DistanceQ: distanceQ, Hit: distanceQ == 0,
				Point: model.Vec3{
					X: int64(math.Round(contact.ClosestWeapon[0] * float64(model.InternalScale))),
					Y: int64(math.Round(contact.ClosestWeapon[1] * float64(model.InternalScale))),
					Z: int64(math.Round(contact.ClosestWeapon[2] * float64(model.InternalScale))),
				},
			}
			fg.Contacts = append(fg.Contacts, cr)
			if fg.NearestDistanceQ < 0 || distanceQ < fg.NearestDistanceQ {
				fg.NearestDistanceQ = distanceQ
			}
		}
		sort.Slice(fg.Contacts, func(i, j int) bool { return fg.Contacts[i].ColliderNo < fg.Contacts[j].ColliderNo })
		result.Frames = append(result.Frames, fg)
	}
	return result, nil
}

func NewInitialState(project *model.Project) State {
	st := State{Sweeps: map[string]SweepState{}}
	for _, s := range project.Sweeps {
		st.Sweeps[s.ID] = SweepState{DamageQ: s.DamageQ, SegmentCount: 0, HitColliders: []int{}}
	}
	return st
}

func cloneState(in State) State {
	out := State{Sweeps: map[string]SweepState{}}
	for k, v := range in.Sweeps {
		hit := append([]int{}, v.HitColliders...)
		v.HitColliders = hit
		out.Sweeps[k] = v
	}
	return out
}

func SettleInterval(project *model.Project, spec IntervalSpec, geomResult *GeometryResult, upstream State, cacheKey, upstreamKey, contentHash string, deps []Dependency) (*IntervalResult, error) {
	state := cloneState(upstream)
	ir := &IntervalResult{
		No: spec.No, StartFrame: spec.StartFrame, EndFrame: spec.EndFrame,
		CacheKey: cacheKey, UpstreamKey: upstreamKey, ContentHash: contentHash,
		Frames: []FrameResult{}, Dependencies: deps, Status: "committed", AlgorithmVersion: model.AlgoVersion,
	}
	for _, gf := range geomResult.Frames {
		sweep := project.SweepByID(gf.SweepID)
		if sweep == nil {
			return nil, model.NewError(model.CodeSchema, "$.sweeps", project.SchemaVersion, "unknown sweep %q", gf.SweepID)
		}
		st := state.Sweeps[sweep.ID]
		fr := FrameResult{
			Frame: gf.Frame, SweepID: gf.SweepID, AtMS: gf.AtMS,
			Segments: []SegmentResult{}, NearestDistanceQ: gf.NearestDistanceQ,
			Truncations: gf.Truncations, RemainingDamageQ: st.DamageQ,
		}
		hits := make([]ContactResult, 0)
		for _, c := range gf.Contacts {
			if c.Hit {
				hits = append(hits, c)
			}
		}
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].TOIQ != hits[j].TOIQ {
				return hits[i].TOIQ < hits[j].TOIQ
			}
			return hits[i].ColliderNo < hits[j].ColliderNo
		})
		for _, contact := range hits {
			already := false
			for _, no := range st.HitColliders {
				if no == contact.ColliderNo {
					already = true
					break
				}
			}
			if already && sweep.RepeatPolicy == model.RepeatOncePerSweep {
				fr.Segments = append(fr.Segments, SegmentResult{
					Status: "repeat_ignored", Frame: gf.Frame, ColliderNo: contact.ColliderNo,
					ColliderID: contact.ColliderID, Part: contact.Part, Surface: contact.Surface,
					TOI: contact.TOI, TOIQ: contact.TOIQ, DistanceQ: contact.DistanceQ,
					RemainingDamageQ: st.DamageQ, Reason: "repeat_policy=once_per_collider",
				})
				continue
			}
			nextSegment := st.SegmentCount + 1
			if nextSegment > sweep.MaxSegments {
				fr.Segments = append(fr.Segments, SegmentResult{
					Segment: nextSegment, Status: "blocked", Frame: gf.Frame, ColliderNo: contact.ColliderNo,
					ColliderID: contact.ColliderID, Part: contact.Part, Surface: contact.Surface,
					RemainingDamageQ: st.DamageQ, Reason: "max_segments_exceeded",
				})
				st.SegmentCount = nextSegment
				continue
			}
			st.SegmentCount = nextSegment
			seg := SegmentResult{
				Segment: nextSegment, Frame: gf.Frame, ColliderNo: contact.ColliderNo,
				ColliderID: contact.ColliderID, Part: contact.Part, Surface: contact.Surface,
				TOI: contact.TOI, TOIQ: contact.TOIQ, DistanceQ: contact.DistanceQ,
				Point:         contact.Point,
				DamageBeforeQ: st.DamageQ,
			}
			if st.DamageQ <= 0 {
				seg.Status = "depleted"
				seg.Reason = "damage_exhausted_before_segment"
				seg.RemainingDamageQ = 0
				fr.Segments = append(fr.Segments, seg)
				continue
			}
			remaining, _, err := conv.MulDiv("damage.decay", project.SchemaVersion, st.DamageQ, sweep.DecayQ, model.InternalScale, conv.RoundFloor)
			if err != nil {
				return nil, err
			}
			st.DamageQ = remaining
			st.HitColliders = append(st.HitColliders, contact.ColliderNo)
			seg.Status = "hit"
			seg.RemainingDamageQ = remaining
			fr.Segments = append(fr.Segments, seg)
			if fr.FirstColliderNo == 0 {
				fr.FirstColliderNo = contact.ColliderNo
				fr.FirstPart = contact.Part
				fr.FirstSurface = contact.Surface
			}
			if !sweep.Penetration || !sweep.MultiHit {
				break
			}
		}
		fr.RemainingDamageQ = st.DamageQ
		state.Sweeps[sweep.ID] = st
		ir.Frames = append(ir.Frames, fr)
	}
	ir.StateOut = state
	return ir, nil
}
