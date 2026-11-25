package codec

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"skeletonretarget/internal/fixed"
	"skeletonretarget/internal/model"
	"skeletonretarget/internal/topology"
)

type rawFrame struct {
	Index    int
	TimeMS   int64
	Root     model.FixedVec3
	Rotation map[string]model.Quat
}

type decoded struct {
	format        string
	version       string
	sourceVersion string
	timeBase      model.TimeBase
	joints        []model.Joint
	frames        []rawFrame
	mappings      []model.Mapping
	target        []model.TargetJoint
	truth         []model.TruthSample
	tolerance     model.Tolerance
	conversions   []model.ConversionRecord
}

func Load(data []byte) (*model.Document, error) {
	doc, err := LoadDetailed(data)
	if err != nil {
		return nil, err
	}
	return doc.Document, nil
}

type DetailedResult struct {
	Document    *model.Document
	Conversions []model.ConversionRecord
}

func LoadDetailed(data []byte) (*DetailedResult, error) {
	var probe struct {
		Format  string `json:"format"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, &model.Error{Code: model.CodeDecode, Path: "$", Message: "invalid JSON binding data", Cause: err}
	}
	if probe.Format == "" {
		return nil, schema("", "$.format", probe.Version, "required field is missing")
	}
	if probe.Version == "" {
		return nil, schema(probe.Format, "$.version", "", "required field is missing")
	}
	var result *decoded
	var err *model.Error
	switch {
	case probe.Format == "studio" && probe.Version == "3":
		result, err = decodeStudio(data)
	case probe.Format == "engine" && probe.Version == "2":
		result, err = decodeEngine(data)
	case probe.Format == "legacy" && probe.Version == "1":
		result, err = decodeLegacy(data)
	case probe.Format == "studio" || probe.Format == "engine" || probe.Format == "legacy":
		return nil, schema(probe.Format, "$.version", probe.Version, "unknown release version")
	default:
		return nil, schema(probe.Format, "$.format", probe.Version, "unknown release format")
	}
	if err != nil {
		return nil, err
	}
	doc, err := finalize(result)
	if err != nil {
		return nil, err
	}
	return &DetailedResult{Document: doc, Conversions: result.conversions}, nil
}

func finalize(in *decoded) (*model.Document, *model.Error) {
	if in.sourceVersion == "" {
		return nil, schema(in.format, "$.sourceVersion", in.version, "required field is missing")
	}
	if in.timeBase.FPS <= 0 {
		return nil, schema(in.format, "$.timeline.fps", in.version, "fps must be positive")
	}
	if in.timeBase.Round == "" {
		in.timeBase.Round = model.RoundHalfUp
	}
	if len(in.joints) == 0 {
		return nil, schema(in.format, "$.skeleton", in.version, "skeleton is empty")
	}
	if len(in.frames) == 0 {
		return nil, schema(in.format, "$.clip.frames", in.version, "clip has no frames")
	}
	sortedJoints, topoErr := topology.SortSkeleton(in.joints)
	if topoErr != nil {
		topoErr.Version = in.version
		return nil, topoErr
	}
	sortedTarget, topoErr := topology.SortTarget(in.target)
	if topoErr != nil {
		topoErr.Version = in.version
		return nil, topoErr
	}
	jointIndex := make(map[string]int, len(sortedJoints))
	for i, joint := range sortedJoints {
		jointIndex[joint.ID] = i
	}
	frames := make([]model.Frame, 0, len(in.frames))
	for i, raw := range in.frames {
		rotations := make([]model.Quat, len(sortedJoints))
		seen := make([]bool, len(sortedJoints))
		for joint, q := range raw.Rotation {
			idx, ok := jointIndex[joint]
			if !ok {
				return nil, schema(in.format, fmt.Sprintf("$.clip.frames[%d].rotations.%s", i, joint), in.version, "rotation references unknown joint")
			}
			rotations[idx] = q
			seen[idx] = true
		}
		for j, joint := range sortedJoints {
			if !seen[j] {
				return nil, schema(in.format, fmt.Sprintf("$.clip.frames[%d].rotations.%s", i, joint.ID), in.version, "required joint rotation is missing")
			}
		}
		frames = append(frames, model.Frame{
			Index: raw.Index, TimeMS: raw.TimeMS, Root: raw.Root, Rotations: rotations,
		})
	}
	if err := validateMapping(in, sortedJoints, sortedTarget); err != nil {
		return nil, err
	}
	targetIDs := make(map[string]bool, len(sortedTarget))
	for _, joint := range sortedTarget {
		targetIDs[joint.ID] = true
	}
	for i := range in.truth {
		if !targetIDs[in.truth[i].Joint] {
			return nil, schema(in.format, fmt.Sprintf("$.truth[%d].joint", i), in.version, "truth references unknown target joint")
		}
		if len(frames) == 0 || in.truth[i].Frame < frames[0].Index || in.truth[i].Frame > frames[len(frames)-1].Index {
			return nil, schema(in.format, fmt.Sprintf("$.truth[%d].frame", i), in.version, "truth references unknown frame")
		}
	}
	if len(in.truth) == 0 {
		return nil, schema(in.format, "$.truth", in.version, "truth list is empty")
	}
	if in.tolerance.NormPPM <= 0 || in.tolerance.RotationPPM <= 0 {
		return nil, schema(in.format, "$.tolerance", in.version, "tolerance values must be positive")
	}
	doc := &model.Document{
		Format: in.format, Version: in.version, SourceVersion: in.sourceVersion,
		TimeBase: in.timeBase, Skeleton: model.Skeleton{Joints: sortedJoints},
		Clip:     model.Clip{FPS: in.timeBase.FPS, Frames: frames},
		Retarget: model.RetargetSpec{Mappings: in.mappings},
		Target:   model.TargetSkeleton{Joints: sortedTarget},
		Truth:    in.truth, Tolerance: in.tolerance, Conversions: in.conversions,
	}
	return doc, nil
}

func validateMapping(in *decoded, source []model.Joint, target []model.TargetJoint) *model.Error {
	sourceIDs := make(map[string]bool, len(source))
	for _, joint := range source {
		sourceIDs[joint.ID] = true
	}
	targetIDs := make(map[string]bool, len(target))
	for _, joint := range target {
		targetIDs[joint.ID] = true
	}
	seenTarget := make(map[string]bool, len(target))
	for i, mapping := range in.mappings {
		path := fmt.Sprintf("$.retarget.mappings[%d]", i)
		if mapping.ID == "" || mapping.Source == "" || mapping.Target == "" {
			return schema(in.format, path, in.version, "mapping id, source and target are required")
		}
		if !sourceIDs[mapping.Source] {
			return &model.Error{Code: model.CodeMap, Path: path + ".source", Version: in.version, Joint: mapping.Source, Frame: -1, Message: "source joint does not exist"}
		}
		if !targetIDs[mapping.Target] {
			return &model.Error{Code: model.CodeMap, Path: path + ".target", Version: in.version, Joint: mapping.Target, Frame: -1, Message: "target joint does not exist"}
		}
		if seenTarget[mapping.Target] {
			return &model.Error{Code: model.CodeMap, Path: path + ".target", Version: in.version, Joint: mapping.Target, Frame: -1, Message: "target joint has multiple mappings"}
		}
		seenTarget[mapping.Target] = true
		if !mapping.RotationScale.Valid() || !mapping.TranslationScale.Valid() {
			return &model.Error{Code: model.CodeMap, Path: path, Version: in.version, Joint: mapping.Target, Frame: -1, Message: "ratio denominator must be positive"}
		}
	}
	for _, joint := range target {
		if !seenTarget[joint.ID] {
			return &model.Error{Code: model.CodeMap, Version: in.version, Joint: joint.ID, Frame: -1, Message: "target joint is not covered by retarget mapping"}
		}
	}
	return nil
}

func schema(format, path, version, message string) *model.Error {
	if format != "" {
		version = version + " (" + format + ")"
	}
	return &model.Error{Code: model.CodeSchema, Path: path, Version: version, Frame: -1, Message: message}
}

func decodeQuat(scale int64, values []int64, version, joint, path string, frame int) (model.Quat, *model.Error) {
	if scale <= 0 {
		return model.Quat{}, schema("", path+".fixed", version, "fixed scale must be positive")
	}
	if len(values) != 4 {
		return model.Quat{}, schema("", path, version, "quaternion must have four components")
	}
	for _, component := range values {
		if component > scale || component < -scale {
			return model.Quat{}, &model.Error{
				Code: model.CodeComp, Path: path, Version: version, Joint: joint, Frame: frame,
				Message: fmt.Sprintf("rotation component %d exceeds fixed scale %d", component, scale),
			}
		}
	}
	return model.Quat{W: values[0], X: values[1], Y: values[2], Z: values[3], Scale: scale}, nil
}

func decodeOrderedQuat(scale int64, order string, values []int64, version, joint, path string, frame int) (model.Quat, *model.Error) {
	if len(values) != 4 {
		return model.Quat{}, schema("", path+".v", version, "quaternion must have four components")
	}
	ordered := make([]int64, 4)
	switch order {
	case "wxyz":
		copy(ordered, values)
	case "xyzw":
		ordered[0], ordered[1], ordered[2], ordered[3] = values[3], values[0], values[1], values[2]
	default:
		return model.Quat{}, schema("", path+".order", version, "unknown quaternion component order")
	}
	return decodeQuat(scale, ordered, version, joint, path, frame)
}

func parseRatio(values []int64, version, path string) (model.Ratio, *model.Error) {
	if len(values) != 2 || values[1] <= 0 {
		return model.Ratio{}, schema("", path, version, "ratio must be [positive numerator, positive denominator]")
	}
	if values[0] < 0 {
		return model.Ratio{}, schema("", path, version, "ratio numerator must not be negative")
	}
	return model.Ratio{Num: values[0], Den: values[1]}, nil
}

func roundMode(value string, fallback model.RoundMode) (model.RoundMode, bool) {
	if value == "" {
		return fallback, true
	}
	switch model.RoundMode(strings.ToLower(value)) {
	case model.RoundHalfUp, model.RoundFloor, model.RoundCeil, model.RoundToward:
		return model.RoundMode(strings.ToLower(value)), true
	default:
		return "", false
	}
}

func checkIncreasing(frames []rawFrame, version string) *model.Error {
	for i := 1; i < len(frames); i++ {
		if frames[i].Index <= frames[i-1].Index {
			return &model.Error{
				Code: model.CodeOrder, Path: fmt.Sprintf("$.clip.frames[%d]", i), Version: version,
				Frame: frames[i].Index, Message: "frame sequence is not strictly increasing",
			}
		}
		if frames[i].TimeMS <= frames[i-1].TimeMS {
			return &model.Error{
				Code: model.CodeOrder, Path: fmt.Sprintf("$.clip.frames[%d]", i), Version: version,
				Frame: frames[i].Index, Message: "time sequence is not strictly increasing",
			}
		}
	}
	return nil
}

func addConversion(records *[]model.ConversionRecord, path, kind string, from, to int64, trunc fixed.Truncation) {
	*records = append(*records, model.ConversionRecord{
		Path: path, Kind: kind, From: from, To: to, Remainder: trunc.Remainder,
		Divisor: trunc.Divisor, Round: trunc.Mode,
	})
}

func constraintFrom(unit string, min, max int64, version, path string, records *[]model.ConversionRecord) (model.Constraint, *model.Error) {
	if max < min {
		return model.Constraint{}, schema("", path, version, "constraint max is below min")
	}
	switch unit {
	case "rad":
		return model.Constraint{MinRad: min, MaxRad: max, Scale: model.AngleScale}, nil
	case "deg":
		radMin := int64(math.Round(float64(min) * math.Pi / 180))
		radMax := int64(math.Round(float64(max) * math.Pi / 180))
		*records = append(*records,
			model.ConversionRecord{Path: path + ".min", Kind: "deg_to_rad", From: min, To: radMin, Round: model.RoundHalfUp},
			model.ConversionRecord{Path: path + ".max", Kind: "deg_to_rad", From: max, To: radMax, Round: model.RoundHalfUp},
		)
		return model.Constraint{MinRad: radMin, MaxRad: radMax, Scale: model.AngleScale}, nil
	default:
		return model.Constraint{}, schema("", path+".unit", version, "unknown angle unit")
	}
}

func wireMappings(values []wireMapping, version string) ([]model.Mapping, *model.Error) {
	out := make([]model.Mapping, 0, len(values))
	for i, value := range values {
		if value.ID == "" || value.Source == "" || value.Target == "" {
			return nil, schema("", fmt.Sprintf("$.retarget.mappings[%d]", i), version, "mapping id, source and target are required")
		}
		rotation, err := parseRatio(value.RotationScale, version, fmt.Sprintf("$.retarget.mappings[%d].rotationScale", i))
		if err != nil {
			return nil, err
		}
		translation, err := parseRatio(value.TranslationScale, version, fmt.Sprintf("$.retarget.mappings[%d].translationScale", i))
		if err != nil {
			return nil, err
		}
		out = append(out, model.Mapping{
			ID: value.ID, Source: value.Source, Target: value.Target,
			RotationScale: rotation, TranslationScale: translation,
		})
	}
	return out, nil
}

type wireTruth struct {
	Frame    int      `json:"frame"`
	Joint    string   `json:"joint"`
	Rotation wireQuat `json:"rotation"`
	Position wireVec3 `json:"position"`
}

type wireQuat struct {
	Fixed int64   `json:"fixed"`
	Order string  `json:"order"`
	V     []int64 `json:"v"`
}

type wireVec3 struct {
	Fixed int64   `json:"fixed"`
	V     []int64 `json:"v"`
}

type wireTolerance struct {
	NormPPM      int64 `json:"normPpm"`
	RoundTripPPM int64 `json:"roundTripPpm"`
	RotationPPM  int64 `json:"rotationPpm"`
}

type wireRetarget struct {
	Mappings []wireMapping `json:"mappings"`
}

type wireMapping struct {
	ID               string  `json:"id"`
	Source           string  `json:"source"`
	Target           string  `json:"target"`
	RotationScale    []int64 `json:"rotationScale"`
	TranslationScale []int64 `json:"translationScale"`
}

func decodeTruthList(values []wireTruth, version string) ([]model.TruthSample, *model.Error) {
	out := make([]model.TruthSample, 0, len(values))
	for i, value := range values {
		path := fmt.Sprintf("$.truth[%d]", i)
		if value.Joint == "" {
			return nil, schema("", path+".joint", version, "required field is missing")
		}
		rotation, err := decodeOrderedQuat(value.Rotation.Fixed, value.Rotation.Order, value.Rotation.V, version, value.Joint, path+".rotation", value.Frame)
		if err != nil {
			return nil, err
		}
		if value.Position.Fixed <= 0 || len(value.Position.V) != 3 {
			return nil, schema("", path+".position", version, "position needs positive scale and three components")
		}
		out = append(out, model.TruthSample{
			Frame: value.Frame, Joint: value.Joint, Rotation: rotation,
			Position: model.FixedVec3{X: value.Position.V[0], Y: value.Position.V[1], Z: value.Position.V[2], Scale: value.Position.Fixed},
		})
	}
	return out, nil
}

func decodeTolerance(value wireTolerance, version string) (model.Tolerance, *model.Error) {
	if value.NormPPM <= 0 || value.RoundTripPPM <= 0 || value.RotationPPM <= 0 {
		return model.Tolerance{}, schema("", "$.tolerance", version, "required tolerance is missing")
	}
	return model.Tolerance{NormPPM: value.NormPPM, RoundTripPPM: value.RoundTripPPM, RotationPPM: value.RotationPPM}, nil
}
