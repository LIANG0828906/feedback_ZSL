package solver

import (
	"fmt"
	"math"
	"reflect"
	"sort"

	"retargetlab/internal/geom"
	"retargetlab/internal/model"
)

type Difference struct {
	Frame int     `json:"frame"`
	Joint string  `json:"joint,omitempty"`
	Field string  `json:"field"`
	Left  any     `json:"left"`
	Right any     `json:"right"`
	Delta float64 `json:"delta,omitempty"`
}

func CompareTruth(target model.Skeleton, truth model.TargetTruth, results []FrameResult) error {
	return CompareTruthWithSource(model.Skeleton{}, target, truth, results)
}

func CompareTruthWithSource(source, target model.Skeleton, truth model.TargetTruth, results []FrameResult) error {
	if len(truth.SourceBoneLengths) > 0 {
		sourceLengths := targetBoneLengths(source)
		for _, joint := range sortedKeys(truth.SourceBoneLengths) {
			expected := truth.SourceBoneLengths[joint]
			actual, ok := sourceLengths[joint]
			if !ok || math.Abs(actual-expected) > truth.Tolerance {
				return &model.CodedError{
					Code: model.E_DIFF, Op: "compare_truth", Joint: joint,
					Message: "source bone length differs from analytic truth", Left: actual, Right: expected,
				}
			}
		}
	}
	lengths := targetBoneLengths(target)
	for _, joint := range sortedKeys(truth.BoneLengths) {
		expected := truth.BoneLengths[joint]
		actual, ok := lengths[joint]
		if !ok {
			return &model.CodedError{
				Code: model.E_DIFF, Op: "compare_truth", Joint: joint,
				Message: "bone length is missing", Left: actual, Right: expected,
			}
		}
		if math.Abs(actual-expected) > truth.Tolerance {
			return &model.CodedError{
				Code: model.E_DIFF, Op: "compare_truth", Joint: joint,
				Message: "bone length differs from analytic truth", Left: actual, Right: expected,
			}
		}
	}
	byFrame := make(map[int]FrameResult, len(results))
	for _, r := range results {
		byFrame[r.Frame] = r
	}
	for _, kf := range truth.KeyFrames {
		actual, ok := byFrame[kf.Frame]
		if !ok {
			return model.ErrFrame(model.E_DIFF, "compare_truth", kf.Frame, "", "result frame is missing")
		}
		if diff := compareVec("root", actual.RootTrajectory, kf.Root, truth.Tolerance); diff > truth.Tolerance {
			return &model.CodedError{
				Code: model.E_DIFF, Op: "compare_truth", Frame: kf.Frame, HasFrame: true, Field: "root",
				Message: "root trajectory differs from analytic truth",
				Left:    actual.RootTrajectory, Right: kf.Root, Delta: diff,
			}
		}
		if !reflect.DeepEqual(actual.Contacts, kf.Contacts) {
			return &model.CodedError{
				Code: model.E_DIFF, Op: "compare_truth", Frame: kf.Frame, HasFrame: true, Field: "contacts",
				Message: "contact state differs from analytic truth", Left: actual.Contacts, Right: kf.Contacts,
			}
		}
		actualWorld := make(map[string]geom.Vec3, len(actual.World))
		for _, jw := range actual.World {
			actualWorld[jw.Joint] = jw.Position
		}
		for _, expected := range kf.World {
			got, ok := actualWorld[expected.Joint]
			if !ok {
				return &model.CodedError{
					Code: model.E_DIFF, Op: "compare_truth", Frame: kf.Frame, HasFrame: true,
					Joint: expected.Joint, Message: "world joint is missing",
				}
			}
			diff := compareVec("world", got, expected.Position, truth.Tolerance)
			if diff > truth.Tolerance {
				return &model.CodedError{
					Code: model.E_DIFF, Op: "compare_truth", Frame: kf.Frame, HasFrame: true,
					Joint: expected.Joint, Message: "world position differs from analytic truth",
					Left: got, Right: expected.Position, Delta: diff,
				}
			}
		}
	}
	return nil
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func CompareFrameResults(left, right FrameResult) error {
	return CompareFrameResultsEpsilon(left, right, 0)
}

func CompareFrameResultsEpsilon(left, right FrameResult, eps float64) error {
	if left.Frame != right.Frame {
		return &model.CodedError{
			Code: model.E_DIFF, Op: "compare_frames", Frame: left.Frame, HasFrame: true,
			Field: "frame", Message: fmt.Sprintf("frame identity differs: %d vs %d", left.Frame, right.Frame),
			Left: left.Frame, Right: right.Frame,
		}
	}
	if len(left.TargetRotations) != len(right.TargetRotations) {
		return &model.CodedError{
			Code: model.E_DIFF, Op: "compare_frames", Frame: left.Frame, HasFrame: true,
			Field: "targetRotations", Message: "rotation count differs",
			Left: len(left.TargetRotations), Right: len(right.TargetRotations),
		}
	}
	if len(left.World) != len(right.World) {
		return &model.CodedError{
			Code: model.E_DIFF, Op: "compare_frames", Frame: left.Frame, HasFrame: true,
			Field: "world", Message: "world summary size differs",
			Left: len(left.World), Right: len(right.World),
		}
	}
	for i := range left.TargetRotations {
		if qdiff(left.TargetRotations[i], right.TargetRotations[i]) > eps {
			joint := ""
			if i < len(left.World) {
				joint = left.World[i].Joint
			}
			return &model.CodedError{
				Code: model.E_DIFF, Op: "compare_frames", Frame: left.Frame, HasFrame: true,
				Joint: joint, Field: "rotation", Message: "target local rotation differs",
				Left: left.TargetRotations[i], Right: right.TargetRotations[i],
				Delta: qdiff(left.TargetRotations[i], right.TargetRotations[i]),
			}
		}
	}
	for i := range left.World {
		if left.World[i].Joint != right.World[i].Joint {
			return &model.CodedError{
				Code: model.E_DIFF, Op: "compare_frames", Frame: left.Frame, HasFrame: true,
				Joint: left.World[i].Joint, Field: "world.joint", Message: "world joint order differs",
				Left: left.World[i].Joint, Right: right.World[i].Joint,
			}
		}
		d := compareVec("world", left.World[i].Position, right.World[i].Position, eps)
		if d > eps {
			return &model.CodedError{
				Code: model.E_DIFF, Op: "compare_frames", Frame: left.Frame, HasFrame: true,
				Joint: left.World[i].Joint, Field: "world.position", Message: "world position differs",
				Left: left.World[i].Position, Right: right.World[i].Position, Delta: d,
			}
		}
		if d := qdiff(left.World[i].Rotation, right.World[i].Rotation); d > eps {
			return &model.CodedError{
				Code: model.E_DIFF, Op: "compare_frames", Frame: left.Frame, HasFrame: true,
				Joint: left.World[i].Joint, Field: "world.rotation", Message: "world rotation differs",
				Left: left.World[i].Rotation, Right: right.World[i].Rotation, Delta: d,
			}
		}
	}
	if d := compareVec("root", left.RootTrajectory, right.RootTrajectory, eps); d > eps {
		rootJoint := ""
		if len(left.World) > 0 {
			rootJoint = left.World[0].Joint
		}
		return &model.CodedError{
			Code: model.E_DIFF, Op: "compare_frames", Frame: left.Frame, HasFrame: true,
			Joint: rootJoint, Field: "rootTrajectory", Message: "root trajectory differs",
			Left: left.RootTrajectory, Right: right.RootTrajectory, Delta: d,
		}
	}
	if !reflect.DeepEqual(left.Constraint, right.Constraint) {
		rootJoint := ""
		if len(left.World) > 0 {
			rootJoint = left.World[0].Joint
		}
		return &model.CodedError{
			Code: model.E_DIFF, Op: "compare_frames", Frame: left.Frame, HasFrame: true,
			Joint: rootJoint, Field: "constraint", Message: "constraint violation statistics differ",
			Left: left.Constraint, Right: right.Constraint,
		}
	}
	if !reflect.DeepEqual(left.Contacts, right.Contacts) {
		return &model.CodedError{
			Code: model.E_DIFF, Op: "compare_frames", Frame: left.Frame, HasFrame: true,
			Field: "contacts", Message: "contact state differs",
			Left: left.Contacts, Right: right.Contacts,
		}
	}
	return nil
}

func FirstDifference(frames []int, left, right map[int]FrameResult) (int, error) {
	for _, frame := range frames {
		l, lok := left[frame]
		r, rok := right[frame]
		if !lok || !rok {
			return frame, model.ErrFrame(model.E_DIFF, "compare_all", frame, "", "frame exists on only one side")
		}
		if err := CompareFrameResults(l, r); err != nil {
			return frame, err
		}
	}
	return -1, nil
}

func compareVec(field string, a, b geom.Vec3, eps float64) float64 {
	_ = field
	dx := math.Abs(a.X - b.X)
	dy := math.Abs(a.Y - b.Y)
	dz := math.Abs(a.Z - b.Z)
	m := dx
	if dy > m {
		m = dy
	}
	if dz > m {
		m = dz
	}
	_ = eps
	return m
}

func qdiff(a, b geom.Quat) float64 {
	d1 := math.Abs(a.W-b.W) + math.Abs(a.X-b.X) + math.Abs(a.Y-b.Y) + math.Abs(a.Z-b.Z)
	d2 := math.Abs(a.W+b.W) + math.Abs(a.X+b.X) + math.Abs(a.Y+b.Y) + math.Abs(a.Z+b.Z)
	if d2 < d1 {
		return d2
	}
	return d1
}

func targetBoneLengths(s model.Skeleton) map[string]float64 {
	out := make(map[string]float64, len(s.Joints))
	for i := range s.Joints {
		p := s.Joints[i].Parent
		if p < 0 {
			out[s.Joints[i].Name] = 0
			continue
		}
		out[s.Joints[i].Name] = s.Joints[i].LocalOffset.Length()
	}
	return out
}
