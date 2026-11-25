package codec

import (
	"encoding/json"
	"fmt"

	"skeletonretarget/internal/fixed"
	"skeletonretarget/internal/model"
)

type studioDoc struct {
	Format        string         `json:"format"`
	Version       string         `json:"version"`
	SourceVersion string         `json:"sourceVersion"`
	Timeline      studioTimeline `json:"timeline"`
	Skeleton      studioSkeleton `json:"skeleton"`
	Clip          studioClip     `json:"clip"`
	Retarget      wireRetarget   `json:"retarget"`
	Target        studioTarget   `json:"target"`
	Truth         []wireTruth    `json:"truth"`
	Tolerance     wireTolerance  `json:"tolerance"`
}

type studioTimeline struct {
	Unit  string `json:"unit"`
	FPS   int64  `json:"fps"`
	Round string `json:"round"`
}

type studioSkeleton struct {
	Scale int64         `json:"scale"`
	Tree  []studioJoint `json:"tree"`
}

type studioJoint struct {
	Name       string        `json:"name"`
	Parent     *string       `json:"parent"`
	Rest       []int64       `json:"rest"`
	Constraint studioLimit   `json:"constraint"`
	Children   []studioJoint `json:"children"`
}

type studioLimit struct {
	Unit string `json:"unit"`
	Min  int64  `json:"min"`
	Max  int64  `json:"max"`
}

type studioClip struct {
	RotationScale    int64         `json:"rotationScale"`
	TranslationScale int64         `json:"translationScale"`
	Frames           []studioFrame `json:"frames"`
}

type studioFrame struct {
	Frame int          `json:"frame"`
	Root  []int64      `json:"root"`
	Poses []studioPose `json:"poses"`
}

type studioPose struct {
	Joint string  `json:"joint"`
	Q     []int64 `json:"q"`
}

type studioTarget struct {
	Scale int64               `json:"scale"`
	Tree  []studioTargetJoint `json:"tree"`
}

type studioTargetJoint struct {
	Name       string              `json:"name"`
	Parent     *string             `json:"parent"`
	Rest       []int64             `json:"rest"`
	Proportion []int64             `json:"proportion"`
	Children   []studioTargetJoint `json:"children"`
}

func decodeStudio(data []byte) (*decoded, *model.Error) {
	var raw studioDoc
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, &model.Error{Code: model.CodeDecode, Path: "$", Version: "studio-3", Message: "invalid studio binding", Cause: err}
	}
	if raw.SourceVersion == "" {
		return nil, schema("studio", "$.sourceVersion", raw.Version, "required field is missing")
	}
	if raw.Timeline.Unit != "frame" {
		return nil, schema("studio", "$.timeline.unit", raw.Version, "studio timeline must use frames")
	}
	round, ok := roundMode(raw.Timeline.Round, model.RoundHalfUp)
	if !ok {
		return nil, schema("studio", "$.timeline.round", raw.Version, "unknown rounding mode")
	}
	if raw.Timeline.FPS <= 0 {
		return nil, schema("studio", "$.timeline.fps", raw.Version, "fps must be positive")
	}
	if raw.Skeleton.Scale <= 0 || len(raw.Skeleton.Tree) == 0 {
		return nil, schema("studio", "$.skeleton", raw.Version, "positive scale and skeleton tree are required")
	}
	if raw.Clip.RotationScale <= 0 || raw.Clip.TranslationScale <= 0 {
		return nil, schema("studio", "$.clip", raw.Version, "positive rotationScale and translationScale are required")
	}
	out := &decoded{
		format: "studio", version: "3", sourceVersion: raw.SourceVersion,
		timeBase: model.TimeBase{Kind: "frame", FPS: raw.Timeline.FPS, Round: round},
	}
	var flatten func(nodes []studioJoint, inheritedParent string, path string) *model.Error
	flatten = func(nodes []studioJoint, inheritedParent, path string) *model.Error {
		for i, node := range nodes {
			if node.Name == "" {
				return schema("studio", fmt.Sprintf("%s[%d].name", path, i), raw.Version, "joint name is required")
			}
			parent := inheritedParent
			if node.Parent != nil {
				if inheritedParent != "" && *node.Parent != inheritedParent {
					return schema("studio", fmt.Sprintf("%s[%d].parent", path, i), raw.Version, "nested parent contradicts explicit parent")
				}
				parent = *node.Parent
			}
			if len(node.Rest) != 3 {
				return schema("studio", fmt.Sprintf("%s[%d].rest", path, i), raw.Version, "rest offset needs three components")
			}
			constraint, err := constraintFrom(node.Constraint.Unit, node.Constraint.Min, node.Constraint.Max, raw.Version, fmt.Sprintf("%s[%d].constraint", path, i), &out.conversions)
			if err != nil {
				return err
			}
			out.joints = append(out.joints, model.Joint{
				ID: node.Name, Parent: parent, Constraint: constraint,
				Rest: model.FixedVec3{X: node.Rest[0], Y: node.Rest[1], Z: node.Rest[2], Scale: raw.Skeleton.Scale},
			})
			if err := flatten(node.Children, node.Name, fmt.Sprintf("%s[%d].children", path, i)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := flatten(raw.Skeleton.Tree, "", "$.skeleton.tree"); err != nil {
		return nil, err
	}

	var flattenTarget func(nodes []studioTargetJoint, inheritedParent, path string) *model.Error
	flattenTarget = func(nodes []studioTargetJoint, inheritedParent, path string) *model.Error {
		for i, node := range nodes {
			if node.Name == "" {
				return schema("studio", fmt.Sprintf("%s[%d].name", path, i), raw.Version, "target joint name is required")
			}
			parent := inheritedParent
			if node.Parent != nil {
				if inheritedParent != "" && *node.Parent != inheritedParent {
					return schema("studio", fmt.Sprintf("%s[%d].parent", path, i), raw.Version, "nested parent contradicts explicit parent")
				}
				parent = *node.Parent
			}
			if len(node.Rest) != 3 {
				return schema("studio", fmt.Sprintf("%s[%d].rest", path, i), raw.Version, "target rest needs three components")
			}
			proportion, err := parseRatio(node.Proportion, raw.Version, fmt.Sprintf("%s[%d].proportion", path, i))
			if err != nil {
				return err
			}
			out.target = append(out.target, model.TargetJoint{
				ID: node.Name, Parent: parent, Proportion: proportion,
				Rest: model.FixedVec3{X: node.Rest[0], Y: node.Rest[1], Z: node.Rest[2], Scale: raw.Target.Scale},
			})
			if err := flattenTarget(node.Children, node.Name, fmt.Sprintf("%s[%d].children", path, i)); err != nil {
				return err
			}
		}
		return nil
	}
	if raw.Target.Scale <= 0 || len(raw.Target.Tree) == 0 {
		return nil, schema("studio", "$.target", raw.Version, "positive target scale and tree are required")
	}
	if err := flattenTarget(raw.Target.Tree, "", "$.target.tree"); err != nil {
		return nil, err
	}

	for i, frame := range raw.Clip.Frames {
		path := fmt.Sprintf("$.clip.frames[%d]", i)
		if len(frame.Root) != 3 {
			return nil, schema("studio", path+".root", raw.Version, "root offset needs three components")
		}
		ms, trunc := fixed.FrameToMS(int64(frame.Frame), raw.Timeline.FPS, round)
		addConversion(&out.conversions, path+".frame", "frame_to_ms", int64(frame.Frame), ms, trunc)
		rotations := make(map[string]model.Quat, len(frame.Poses))
		for j, pose := range frame.Poses {
			if pose.Joint == "" {
				return nil, schema("studio", fmt.Sprintf("%s.poses[%d].joint", path, j), raw.Version, "joint is required")
			}
			if _, exists := rotations[pose.Joint]; exists {
				return nil, schema("studio", fmt.Sprintf("%s.poses[%d]", path, j), raw.Version, "duplicate joint rotation")
			}
			q, err := decodeOrderedQuat(raw.Clip.RotationScale, "xyzw", pose.Q, raw.Version, pose.Joint, fmt.Sprintf("%s.poses[%d].q", path, j), frame.Frame)
			if err != nil {
				return nil, err
			}
			rotations[pose.Joint] = q
		}
		out.frames = append(out.frames, rawFrame{
			Index: frame.Frame, TimeMS: ms, Rotation: rotations,
			Root: model.FixedVec3{X: frame.Root[0], Y: frame.Root[1], Z: frame.Root[2], Scale: raw.Clip.TranslationScale},
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
