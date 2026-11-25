package codec

import (
	"encoding/json"
	"fmt"

	"skeletonretarget/internal/fixed"
	"skeletonretarget/internal/model"
)

type legacyDoc struct {
	Format        string         `json:"format"`
	Version       string         `json:"version"`
	SourceVersion string         `json:"sourceVersion"`
	Timeline      legacyTimeline `json:"timeline"`
	Skeleton      legacySkeleton `json:"skeleton"`
	Motion        legacyMotion   `json:"motion"`
	Retarget      wireRetarget   `json:"retarget"`
	Target        legacyTarget   `json:"target"`
	Truth         []wireTruth    `json:"truth"`
	Tolerance     wireTolerance  `json:"tolerance"`
}

type legacyTimeline struct {
	Unit       string `json:"unit"`
	FrameRate  int64  `json:"frameRate"`
	Truncation string `json:"truncation"`
}

type legacySkeleton struct {
	Fixed int64         `json:"fixed"`
	Flat  []legacyJoint `json:"flat"`
}

type legacyJoint struct {
	Joint      string          `json:"joint"`
	Parent     string          `json:"parent"`
	Rest       legacyFixedVec3 `json:"rest"`
	Constraint legacyLimit     `json:"constraint"`
}

type legacyFixedVec3 struct {
	Fixed int64   `json:"fixed"`
	V     []int64 `json:"v"`
}

type legacyLimit struct {
	Unit string `json:"unit"`
	Min  int64  `json:"min"`
	Max  int64  `json:"max"`
}

type legacyMotion struct {
	Fixed  int64         `json:"fixed"`
	Frames []legacyFrame `json:"frames"`
}

type legacyFrame struct {
	Index int            `json:"index"`
	Root  []int64        `json:"root"`
	Q     []legacyQValue `json:"rotations"`
}

type legacyQValue struct {
	Joint string   `json:"joint"`
	Value wireQuat `json:"value"`
}

type legacyTarget struct {
	Fixed int64               `json:"fixed"`
	Flat  []legacyTargetJoint `json:"flat"`
}

type legacyTargetJoint struct {
	Joint      string  `json:"joint"`
	Parent     string  `json:"parent"`
	Rest       []int64 `json:"rest"`
	Proportion []int64 `json:"proportion"`
}

func decodeLegacy(data []byte) (*decoded, *model.Error) {
	var raw legacyDoc
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, &model.Error{Code: model.CodeDecode, Path: "$", Version: "legacy-1", Message: "invalid legacy binding", Cause: err}
	}
	if raw.SourceVersion == "" {
		return nil, schema("legacy", "$.sourceVersion", raw.Version, "required field is missing")
	}
	if raw.Timeline.Unit != "frame" {
		return nil, schema("legacy", "$.timeline.unit", raw.Version, "legacy timeline must use frames")
	}
	round, ok := roundMode(raw.Timeline.Truncation, model.RoundHalfUp)
	if !ok {
		return nil, schema("legacy", "$.timeline.truncation", raw.Version, "unknown truncation mode")
	}
	if raw.Timeline.FrameRate <= 0 {
		return nil, schema("legacy", "$.timeline.frameRate", raw.Version, "frame rate must be positive")
	}
	if raw.Skeleton.Fixed <= 0 || len(raw.Skeleton.Flat) == 0 {
		return nil, schema("legacy", "$.skeleton", raw.Version, "positive fixed scale and flat skeleton are required")
	}
	if raw.Motion.Fixed <= 0 || raw.Target.Fixed <= 0 {
		return nil, schema("legacy", "$.motion.fixed", raw.Version, "positive motion and target scales are required")
	}
	out := &decoded{
		format: "legacy", version: "1", sourceVersion: raw.SourceVersion,
		timeBase: model.TimeBase{Kind: "frame", FPS: raw.Timeline.FrameRate, Round: round},
	}
	for i, joint := range raw.Skeleton.Flat {
		path := fmt.Sprintf("$.skeleton.flat[%d]", i)
		if joint.Joint == "" || len(joint.Rest.V) != 3 || joint.Rest.Fixed <= 0 {
			return nil, schema("legacy", path+".rest", raw.Version, "joint, positive fixed scale and three rest components are required")
		}
		constraint, err := constraintFrom(joint.Constraint.Unit, joint.Constraint.Min, joint.Constraint.Max, raw.Version, path+".constraint", &out.conversions)
		if err != nil {
			return nil, err
		}
		out.joints = append(out.joints, model.Joint{
			ID: joint.Joint, Parent: joint.Parent,
			Rest:       model.FixedVec3{X: joint.Rest.V[0], Y: joint.Rest.V[1], Z: joint.Rest.V[2], Scale: joint.Rest.Fixed},
			Constraint: constraint,
		})
	}
	for i, joint := range raw.Target.Flat {
		path := fmt.Sprintf("$.target.flat[%d]", i)
		if joint.Joint == "" || len(joint.Rest) != 3 {
			return nil, schema("legacy", path, raw.Version, "target joint and three rest components are required")
		}
		proportion, err := parseRatio(joint.Proportion, raw.Version, path+".proportion")
		if err != nil {
			return nil, err
		}
		out.target = append(out.target, model.TargetJoint{
			ID: joint.Joint, Parent: joint.Parent, Proportion: proportion,
			Rest: model.FixedVec3{X: joint.Rest[0], Y: joint.Rest[1], Z: joint.Rest[2], Scale: raw.Target.Fixed},
		})
	}
	for i, frame := range raw.Motion.Frames {
		path := fmt.Sprintf("$.motion.frames[%d]", i)
		if len(frame.Root) != 3 {
			return nil, schema("legacy", path+".root", raw.Version, "root translation needs three components")
		}
		ms, trunc := fixed.FrameToMS(int64(frame.Index), raw.Timeline.FrameRate, round)
		addConversion(&out.conversions, path+".index", "frame_to_ms", int64(frame.Index), ms, trunc)
		rotations := make(map[string]model.Quat, len(frame.Q))
		for j, value := range frame.Q {
			if value.Joint == "" {
				return nil, schema("legacy", fmt.Sprintf("%s.rotations[%d].joint", path, j), raw.Version, "joint is required")
			}
			if _, exists := rotations[value.Joint]; exists {
				return nil, schema("legacy", fmt.Sprintf("%s.rotations[%d]", path, j), raw.Version, "duplicate joint rotation")
			}
			q, err := decodeOrderedQuat(value.Value.Fixed, value.Value.Order, value.Value.V, raw.Version, value.Joint, fmt.Sprintf("%s.rotations[%d].value", path, j), frame.Index)
			if err != nil {
				return nil, err
			}
			rotations[value.Joint] = q
		}
		out.frames = append(out.frames, rawFrame{
			Index: frame.Index, TimeMS: ms, Rotation: rotations,
			Root: model.FixedVec3{X: frame.Root[0], Y: frame.Root[1], Z: frame.Root[2], Scale: raw.Motion.Fixed},
		})
	}
	if err := checkIncreasing(out.frames, raw.Version); err != nil {
		return nil, err
	}
	mappings, err := wireMappings(raw.Retarget.Mappings, raw.Version)
	if err != nil {
		return nil, err
	}
	out.mappings = mappings
	truth, err := decodeTruthList(raw.Truth, raw.Version)
	if err != nil {
		return nil, err
	}
	out.truth = truth
	tolerance, err := decodeTolerance(raw.Tolerance, raw.Version)
	if err != nil {
		return nil, err
	}
	out.tolerance = tolerance
	return out, nil
}
