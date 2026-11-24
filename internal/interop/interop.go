package interop

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"storyboardstudio/internal/model"
)

const (
	Version1 = "storyboard/1.0"
	Version2 = "storyboard/2.0"
	Version3 = "storyboard/3.0"
)

type Document struct {
	NodeID string       `json:"node_id"`
	Shots  []model.Shot `json:"shots"`
}

type Trace string

type Report struct {
	Code         model.ErrorCode `json:"code,omitempty"`
	Version      string          `json:"version"`
	UpgradeTrace []string        `json:"upgrade_trace,omitempty"`
	LossyFields  []string        `json:"lossy_fields,omitempty"`
	Message      string          `json:"message,omitempty"`
}

type ExportResult struct {
	Version string
	Data    []byte
	Lossy   *Report
}

type versionEnvelope struct {
	Schema string `json:"schema"`
	Format string `json:"format"`
}

type v1Document struct {
	Schema string   `json:"schema"`
	NodeID string   `json:"node_id"`
	Clips  []v1Clip `json:"clips"`
}

type v1Clip struct {
	ClipID        string `json:"clip_id"`
	Ord           int    `json:"ord"`
	LengthDeciSec int64  `json:"length_deciseconds"`
	RatioPath     string `json:"ratio_path"`
	CameraNotes   string `json:"camera_notes,omitempty"`
}

type v2Document struct {
	Schema string   `json:"schema"`
	NodeID string   `json:"nodeId"`
	Shots  []v2Shot `json:"shots"`
}

type v2Shot struct {
	ID             string `json:"id"`
	Index          int    `json:"index"`
	DurationMillis int64  `json:"durationMillis"`
	AspectURN      string `json:"aspectUrn"`
	Camera         string `json:"camera,omitempty"`
	Transition     string `json:"transition,omitempty"`
}

type v3Document struct {
	Schema string   `json:"schema"`
	NodeID string   `json:"node_id"`
	Shots  []v3Shot `json:"shots"`
}

type v3Shot struct {
	ID         string `json:"id"`
	Index      int    `json:"index"`
	DurationUS int64  `json:"duration_us"`
	AspectURI  string `json:"aspect_uri"`
	Camera     string `json:"camera,omitempty"`
	Motion     string `json:"motion,omitempty"`
	Transition string `json:"transition,omitempty"`
}

var (
	v1AspectRE  = regexp.MustCompile(`^aspect/([1-9][0-9]*)x([1-9][0-9]*)\.json$`)
	v2AspectRE  = regexp.MustCompile(`^urn:storyboard:aspect:([1-9][0-9]*)x([1-9][0-9]*)$`)
	canonicalRE = regexp.MustCompile(`^aspect://([1-9][0-9]*)x([1-9][0-9]*)$`)
)

func Decode(data []byte) (*Document, *Report, error) {
	var envelope versionEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, nil, &model.Error{Code: model.CodeSchema, Message: "storyboard JSON is invalid: " + err.Error()}
	}
	version := envelope.Schema
	if version == "" {
		version = envelope.Format
	}
	switch version {
	case Version1:
		return decodeV1(data)
	case Version2:
		return decodeV2(data)
	case Version3:
		return decodeV3(data)
	case "":
		return nil, nil, &model.Error{Code: model.CodeSchema, Path: "$.schema", Message: "storyboard schema/version is missing"}
	default:
		return nil, &Report{Version: version}, &model.Error{Code: model.CodeVersion, Path: "$.schema",
			Expected: strings.Join([]string{Version1, Version2, Version3}, ", "), Actual: version,
			Message: "unsupported storyboard structure version"}
	}
}

func Encode(document *Document, version string) (*ExportResult, error) {
	if document == nil {
		return nil, &model.Error{Code: model.CodeSchema, Message: "storyboard document is nil"}
	}
	switch version {
	case Version1:
		return encodeV1(document)
	case Version2:
		return encodeV2(document)
	case Version3:
		return encodeV3(document)
	default:
		return nil, &model.Error{Code: model.CodeVersion, Expected: Version1 + ", " + Version2 + ", " + Version3,
			Actual: version, Message: "cannot export unknown storyboard version"}
	}
}

func decodeV1(data []byte) (*Document, *Report, error) {
	var dto v1Document
	if err := json.Unmarshal(data, &dto); err != nil {
		return nil, nil, schemaWrap(err)
	}
	if strings.TrimSpace(dto.NodeID) == "" {
		return nil, nil, &model.Error{Code: model.CodeSchema, Path: "$.node_id", Message: "node_id is required"}
	}
	report := &Report{Version: Version1, UpgradeTrace: []string{
		"schema upgraded storyboard/1.0 -> storyboard/3.0",
		"duration unit converted deciseconds(100ms) -> milliseconds",
		"ratio_path converted to canonical aspect:// URI",
		"motion defaulted to static",
		"transition defaulted to cut",
	}}
	doc := &Document{NodeID: dto.NodeID, Shots: make([]model.Shot, 0, len(dto.Clips))}
	for i, clip := range dto.Clips {
		if clip.ClipID == "" {
			return nil, report, &model.Error{Code: model.CodeSchema, Path: fmt.Sprintf("$.clips[%d].clip_id", i), Message: "clip_id is required"}
		}
		if clip.LengthDeciSec < 0 {
			return nil, report, &model.Error{Code: model.CodeSchema, Path: fmt.Sprintf("$.clips[%d].length_deciseconds", i), Message: "duration must be non-negative"}
		}
		aspect, err := fromV1Aspect(clip.RatioPath)
		if err != nil {
			return nil, report, err
		}
		if clip.Ord != i {
			report.UpgradeTrace = append(report.UpgradeTrace, fmt.Sprintf("clips[%d].ord normalized from %d to %d", i, clip.Ord, i))
		}
		doc.Shots = append(doc.Shots, model.Shot{
			ID: clip.ClipID, DurationMS: clip.LengthDeciSec * 100, DurationPresent: true,
			AspectPath: aspect, Camera: clip.CameraNotes, Motion: "static", Transition: "cut",
		})
	}
	return doc, report, nil
}

func decodeV2(data []byte) (*Document, *Report, error) {
	var dto v2Document
	if err := json.Unmarshal(data, &dto); err != nil {
		return nil, nil, schemaWrap(err)
	}
	report := &Report{Version: Version2, UpgradeTrace: []string{
		"schema upgraded storyboard/2.0 -> storyboard/3.0",
		"duration field durationMillis normalized to duration_ms",
		"aspectUrn converted to canonical aspect:// URI",
		"motion defaulted to static",
	}}
	doc := &Document{NodeID: dto.NodeID, Shots: make([]model.Shot, 0, len(dto.Shots))}
	for i, shot := range dto.Shots {
		if shot.ID == "" {
			return nil, report, &model.Error{Code: model.CodeSchema, Path: fmt.Sprintf("$.shots[%d].id", i), Message: "id is required"}
		}
		if shot.DurationMillis < 0 {
			return nil, report, &model.Error{Code: model.CodeSchema, Path: fmt.Sprintf("$.shots[%d].durationMillis", i), Message: "duration must be non-negative"}
		}
		aspect, err := fromV2Aspect(shot.AspectURN)
		if err != nil {
			return nil, report, err
		}
		if shot.Index != i {
			report.UpgradeTrace = append(report.UpgradeTrace, fmt.Sprintf("shots[%d].index normalized from %d to %d", i, shot.Index, i))
		}
		transition := shot.Transition
		if transition == "" {
			transition = "cut"
			report.UpgradeTrace = append(report.UpgradeTrace, fmt.Sprintf("shots[%d].transition defaulted to cut", i))
		}
		doc.Shots = append(doc.Shots, model.Shot{
			ID: shot.ID, DurationMS: shot.DurationMillis, DurationPresent: true, AspectPath: aspect,
			Camera: shot.Camera, Transition: transition, Motion: "static",
		})
	}
	return doc, report, nil
}

func decodeV3(data []byte) (*Document, *Report, error) {
	var dto v3Document
	if err := json.Unmarshal(data, &dto); err != nil {
		return nil, nil, schemaWrap(err)
	}
	report := &Report{Version: Version3, UpgradeTrace: []string{
		"duration unit converted microseconds -> milliseconds",
	}}
	doc := &Document{NodeID: dto.NodeID, Shots: make([]model.Shot, 0, len(dto.Shots))}
	for i, shot := range dto.Shots {
		if shot.ID == "" {
			return nil, report, &model.Error{Code: model.CodeSchema, Path: fmt.Sprintf("$.shots[%d].id", i), Message: "id is required"}
		}
		if shot.DurationUS < 0 {
			return nil, report, &model.Error{Code: model.CodeSchema, Path: fmt.Sprintf("$.shots[%d].duration_us", i), Message: "duration must be non-negative"}
		}
		if !canonicalRE.MatchString(shot.AspectURI) {
			return nil, report, &model.Error{Code: model.CodeSchema, Path: fmt.Sprintf("$.shots[%d].aspect_uri", i), Message: "aspect_uri is not canonical"}
		}
		if shot.Index != i {
			report.UpgradeTrace = append(report.UpgradeTrace, fmt.Sprintf("shots[%d].index normalized from %d to %d", i, shot.Index, i))
		}
		motion := shot.Motion
		if motion == "" {
			motion = "static"
			report.UpgradeTrace = append(report.UpgradeTrace, fmt.Sprintf("shots[%d].motion defaulted to static", i))
		}
		transition := shot.Transition
		if transition == "" {
			transition = "cut"
		}
		doc.Shots = append(doc.Shots, model.Shot{
			ID: shot.ID, DurationMS: shot.DurationUS / 1000, DurationPresent: true, AspectPath: shot.AspectURI,
			Camera: shot.Camera, Motion: motion, Transition: transition,
		})
	}
	return doc, report, nil
}

func encodeV1(doc *Document) (*ExportResult, error) {
	dto := v1Document{Schema: Version1, NodeID: doc.NodeID, Clips: make([]v1Clip, 0, len(doc.Shots))}
	lossy := []string{}
	for i, shot := range doc.Shots {
		aspect, err := toV1Aspect(shot.AspectPath)
		if err != nil {
			return nil, err
		}
		deci := shot.DurationMS / 100
		if shot.DurationMS%100 != 0 {
			lossy = append(lossy, fmt.Sprintf("clips[%d].length_deciseconds", i))
		}
		if shot.Motion != "" && shot.Motion != "static" {
			lossy = append(lossy, fmt.Sprintf("clips[%d].motion", i))
		}
		if shot.Transition != "" && shot.Transition != "cut" {
			lossy = append(lossy, fmt.Sprintf("clips[%d].transition", i))
		}
		dto.Clips = append(dto.Clips, v1Clip{
			ClipID: shot.ID, Ord: i, LengthDeciSec: deci, RatioPath: aspect, CameraNotes: shot.Camera,
		})
	}
	data, err := marshalStable(dto)
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: Version1, Data: data, Lossy: lossyReport(Version1, lossy)}, nil
}

func encodeV2(doc *Document) (*ExportResult, error) {
	dto := v2Document{Schema: Version2, NodeID: doc.NodeID, Shots: make([]v2Shot, 0, len(doc.Shots))}
	lossy := []string{}
	for i, shot := range doc.Shots {
		aspect, err := toV2Aspect(shot.AspectPath)
		if err != nil {
			return nil, err
		}
		if shot.Motion != "" && shot.Motion != "static" {
			lossy = append(lossy, fmt.Sprintf("shots[%d].motion", i))
		}
		dto.Shots = append(dto.Shots, v2Shot{
			ID: shot.ID, Index: i, DurationMillis: shot.DurationMS, AspectURN: aspect,
			Camera: shot.Camera, Transition: defaultString(shot.Transition, "cut"),
		})
	}
	data, err := marshalStable(dto)
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: Version2, Data: data, Lossy: lossyReport(Version2, lossy)}, nil
}

func encodeV3(doc *Document) (*ExportResult, error) {
	dto := v3Document{Schema: Version3, NodeID: doc.NodeID, Shots: make([]v3Shot, 0, len(doc.Shots))}
	for i, shot := range doc.Shots {
		if !canonicalRE.MatchString(shot.AspectPath) {
			return nil, &model.Error{Code: model.CodeSchema, Path: fmt.Sprintf("shots[%d].aspect_uri", i), Message: "internal aspect path is not canonical"}
		}
		dto.Shots = append(dto.Shots, v3Shot{
			ID: shot.ID, Index: i, DurationUS: shot.DurationMS * 1000, AspectURI: shot.AspectPath,
			Camera: shot.Camera, Motion: defaultString(shot.Motion, "static"),
			Transition: defaultString(shot.Transition, "cut"),
		})
	}
	data, err := marshalStable(dto)
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: Version3, Data: data}, nil
}

func marshalStable(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func lossyReport(version string, fields []string) *Report {
	if len(fields) == 0 {
		return nil
	}
	return &Report{Code: model.CodeLossy, Version: version, LossyFields: fields,
		Message: "some internal fields cannot be represented in the target version"}
}

func schemaWrap(err error) error {
	return &model.Error{Code: model.CodeSchema, Message: "invalid storyboard payload: " + err.Error()}
}

func parseAspect(pattern *regexp.Regexp, value, path string) (string, error) {
	matches := pattern.FindStringSubmatch(value)
	if matches == nil {
		return "", &model.Error{Code: model.CodeSchema, Path: path, Actual: value, Message: "unsupported aspect path expression"}
	}
	return "aspect://" + matches[1] + "x" + matches[2], nil
}

func fromV1Aspect(value string) (string, error) {
	return parseAspect(v1AspectRE, value, "ratio_path")
}

func fromV2Aspect(value string) (string, error) {
	return parseAspect(v2AspectRE, value, "aspectUrn")
}

func toV1Aspect(canonical string) (string, error) {
	matches := canonicalRE.FindStringSubmatch(canonical)
	if matches == nil {
		return "", &model.Error{Code: model.CodeSchema, Actual: canonical, Message: "internal aspect path is not canonical"}
	}
	return "aspect/" + matches[1] + "x" + matches[2] + ".json", nil
}

func toV2Aspect(canonical string) (string, error) {
	matches := canonicalRE.FindStringSubmatch(canonical)
	if matches == nil {
		return "", &model.Error{Code: model.CodeSchema, Actual: canonical, Message: "internal aspect path is not canonical"}
	}
	return "urn:storyboard:aspect:" + matches[1] + "x" + matches[2], nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func AtoiOrZero(value string) int {
	result, _ := strconv.Atoi(value)
	return result
}
