package codec

import (
	"encoding/json"
	"fmt"

	"skeletonretarget/internal/fixed"
	"skeletonretarget/internal/model"
)

type engineDoc struct {
	Format        string         `json:"format"`
	Version       string         `json:"version"`
	SourceVersion string         `json:"sourceVersion"`
	Timeline      engineTimeline `json:"timeline"`
	Skeleton      engineSkeleton `json:"skeleton"`
	Clip          engineClip     `json:"clip"`
	Retarget      wireRetarget   `json:"retarget"`
	Target        engineTarget   `json:"target"`
	Truth         []wireTruth    `json:"truth"`
	Tolerance     wireTolerance  `json:"tolerance"`
}

type engineTimeline struct {
	Unit     string `json:"unit"`
	FPS      int64  `json:"fps"`
	Rounding string `json:"rounding"`
}

type engineSkeleton struct {
	Fixed int64        `json:"fixed"`
	Bones []engineBone `json:"bones"`
}

type engineBone struct {
	ID     string      `json:"id"`
	Parent string      `json:"parent"`
	Rest   []int64     `json:"rest"`
	Limit  engineLimit `json:"limit"`
}

type engineLimit struct {
	Unit string `json:"unit"`
	Min  int64  `json:"min"`
	Max  int64  `json:"max"`
}

type engineClip struct {
	Fixed  int64         `json:"fixed"`
	Frames []engineFrame `json:"frames"`
}

type engineFrame struct {
	AtMS  int64        `json:"atMs"`
	Root  []int64      `json:"root"`
	Poses []enginePose `json:"poses"`
}

type enginePose struct {
	ID string  `json:"id"`
	Q  []int64 `json:"q"`
}

type engineTarget struct {
	Fixed int64              `json:"fixed"`
	Bones []engineTargetBone `json:"bones"`
}

type engineTargetBone struct {
	ID         string  `json:"id"`
	Parent     string  `json:"parent"`
	Rest       []int64 `json:"rest"`
	Proportion []int64 `json:"proportion"`
}

func decodeEngine(data []byte) (*decoded, *model.Error) {
	var raw engineDoc
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, &model.Error{Code: model.CodeDecode, Path: "$", Version: "engine-2", Message: "invalid engine binding", Cause: err}
	}
	if raw.SourceVersion == "" {
		return nil, schema("engine", "$.sourceVersion", raw.Version, "required field is missing")
	}
	if raw.Timeline.Unit != "ms" {
		return nil, schema("engine", "$.timeline.unit", raw.Version, "engine timeline must use milliseconds")
	}
	round, ok := roundMode(raw.Timeline.Rounding, model.RoundHalfUp)
	if !ok {
		return nil, schema("engine", "$.timeline.rounding", raw.Version, "unknown rounding mode")
	}
	if raw.Timeline.FPS <= 0 {
		return nil, schema("engine", "$.timeline.fps", raw.Version, "fps must be positive")
	}
	if raw.Skeleton.Fixed <= 0 || len(raw.Skeleton.Bones) == 0 {
		return nil, schema("engine", "$.skeleton", raw.Version, "positive fixed scale and bones are required")
	}
	if raw.Clip.Fixed <= 0 || raw.Target.Fixed <= 0 {
		return nil, schema("engine", "$.clip.fixed", raw.Version, "positive clip and target scales are required")
	}
	out := &decoded{
		format: "engine", version: "2", sourceVersion: raw.SourceVersion,
		timeBase: model.TimeBase{Kind: "ms", FPS: raw.Timeline.FPS, Round: round},
	}
	for i, bone := range raw.Skeleton.Bones {
		path := fmt.Sprintf("$.skeleton.bones[%d]", i)
		if bone.ID == "" || len(bone.Rest) != 3 {
			return nil, schema("engine", path, raw.Version, "bone id and three rest components are required")
		}
		constraint, err := constraintFrom(bone.Limit.Unit, bone.Limit.Min, bone.Limit.Max, raw.Version, path+".limit", &out.conversions)
		if err != nil {
			return nil, err
		}
		out.joints = append(out.joints, model.Joint{
			ID: bone.ID, Parent: bone.Parent,
			Rest:       model.FixedVec3{X: bone.Rest[0], Y: bone.Rest[1], Z: bone.Rest[2], Scale: raw.Skeleton.Fixed},
			Constraint: constraint,
		})
	}
	for i, bone := range raw.Target.Bones {
		path := fmt.Sprintf("$.target.bones[%d]", i)
		if bone.ID == "" || len(bone.Rest) != 3 {
			return nil, schema("engine", path, raw.Version, "target bone id and three rest components are required")
		}
		proportion, err := parseRatio(bone.Proportion, raw.Version, path+".proportion")
		if err != nil {
			return nil, err
		}
		out.target = append(out.target, model.TargetJoint{
			ID: bone.ID, Parent: bone.Parent, Proportion: proportion,
			Rest: model.FixedVec3{X: bone.Rest[0], Y: bone.Rest[1], Z: bone.Rest[2], Scale: raw.Target.Fixed},
		})
	}
	for i, frame := range raw.Clip.Frames {
		path := fmt.Sprintf("$.clip.frames[%d]", i)
		if len(frame.Root) != 3 {
			return nil, schema("engine", path+".root", raw.Version, "root translation needs three components")
		}
		frameIndex, trunc := fixed.MSToFrame(frame.AtMS, raw.Timeline.FPS, round)
		addConversion(&out.conversions, path+".atMs", "ms_to_frame", frame.AtMS, frameIndex, trunc)
		rotations := make(map[string]model.Quat, len(frame.Poses))
		for j, pose := range frame.Poses {
			if pose.ID == "" {
				return nil, schema("engine", fmt.Sprintf("%s.poses[%d].id", path, j), raw.Version, "joint id is required")
			}
			if _, exists := rotations[pose.ID]; exists {
				return nil, schema("engine", fmt.Sprintf("%s.poses[%d]", path, j), raw.Version, "duplicate joint rotation")
			}
			q, err := decodeOrderedQuat(raw.Clip.Fixed, "wxyz", pose.Q, raw.Version, pose.ID, fmt.Sprintf("%s.poses[%d].q", path, j), int(frameIndex))
			if err != nil {
				return nil, err
			}
			rotations[pose.ID] = q
		}
		out.frames = append(out.frames, rawFrame{
			Index: int(frameIndex), TimeMS: frame.AtMS, Rotation: rotations,
			Root: model.FixedVec3{X: frame.Root[0], Y: frame.Root[1], Z: frame.Root[2], Scale: raw.Clip.Fixed},
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
