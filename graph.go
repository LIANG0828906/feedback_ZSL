package ocrflow

import (
	"encoding/json"
	"fmt"
)

type GraphNode struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	ShotID       string `json:"shot_id,omitempty"`
	LineNo       int    `json:"line_no,omitempty"`
	Field        string `json:"field,omitempty"`
	ContentHash  string `json:"content_hash,omitempty"`
	ParamVersion string `json:"param_version,omitempty"`
	CacheKey     string `json:"cache_key,omitempty"`
}

type GraphEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type DependencyGraph struct {
	Nodes map[string]GraphNode `json:"nodes"`
	Edges []GraphEdge          `json:"edges"`
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{Nodes: map[string]GraphNode{}, Edges: []GraphEdge{}}
}

func (g *DependencyGraph) AddNode(n GraphNode) {
	if _, exists := g.Nodes[n.ID]; !exists {
		g.Nodes[n.ID] = n
	}
}

func (g *DependencyGraph) Connect(from, to, reason string) {
	g.Edges = append(g.Edges, GraphEdge{From: from, To: to, Reason: reason})
}

func CacheKey(regionHash, lineID, paramVersion, upstreamKey, stage string) string {
	return "ck_" + HashString(stage + "|" + regionHash + "|" + lineID + "|" + paramVersion + "|" + upstreamKey)[:32]
}

func (r *Recipe) StageParamVersion(stage string, lib *TemplateLibrary) string {
	var payload any
	switch stage {
	case "preprocess":
		payload = struct {
			Algo string
			P    PreprocessConfig
		}{"preprocess-v1", r.Preprocess}
	case "detection", "line_candidate":
		payload = struct {
			Algo string
			P    PreprocessConfig
			D    DetectConfig
		}{"detect-v1", r.Preprocess, r.Detection}
	case "segmentation":
		payload = struct {
			Algo           string
			SequenceSplit  bool
			StrokeWidthMin int
		}{"segment-v1", r.Recognition.SequenceSplit, r.Detection.MinStrokeWidthPx}
	case "recognition":
		libHash := ""
		libVersion := ""
		if lib != nil {
			libHash = lib.Hash
			libVersion = lib.Version
		}
		payload = struct {
			Algo string
			R    RecogConfig
			LV   string
			LH   string
		}{"recognize-v1", r.Recognition, libVersion, libHash}
	default:
		payload = r
	}
	data, _ := json.Marshal(payload)
	return "rp-" + HashBytes(data)[:16]
}

type LineKeys struct {
	RegionHash       string `json:"region_hash"`
	ShotKey          string `json:"shot_key"`
	PreprocessKey    string `json:"preprocess_key"`
	DetectionKey     string `json:"detection_key"`
	CandidateKey     string `json:"candidate_key"`
	SegmentationKey  string `json:"segmentation_key"`
	RecognitionKey   string `json:"recognition_key"`
	PreprocessParam  string `json:"preprocess_param"`
	DetectionParam   string `json:"detection_param"`
	SegmentParam     string `json:"segment_param"`
	RecognitionParam string `json:"recognition_param"`
}

func BuildLineKeys(shot *Screenshot, line DetectedLine, recipe *Recipe, lib *TemplateLibrary) LineKeys {
	regionHash := RegionHash(shot, line.Box)
	prepParam := recipe.StageParamVersion("preprocess", lib)
	detParam := recipe.StageParamVersion("detection", lib)
	segParam := recipe.StageParamVersion("segmentation", lib)
	recParam := recipe.StageParamVersion("recognition", lib)

	shotKey := CacheKey(shot.ContentHash, shot.ShotID, recipe.RecipeVersion+":"+recipe.ParamVersion, RecipeAlgorithmVersion, "shot")
	prepKey := CacheKey(shot.ContentHash, shot.ShotID, prepParam, shotKey, "preprocess")
	detKey := CacheKey(shot.ContentHash, shot.ShotID, detParam, prepKey, "detection")
	detSpecKey := CacheKey("detection-parameter-boundary", shot.ShotID, detParam, RecipeAlgorithmVersion, "detection_params")
	lineID := line.LineID
	candidateKey := CacheKey(regionHash, shot.ShotID+":"+lineID, detParam, detSpecKey, "line_candidate")
	segmentKey := CacheKey(regionHash, lineID, segParam, candidateKey, "segmentation")
	recognitionKey := CacheKey(regionHash, lineID, recParam, segmentKey, "recognition")
	return LineKeys{
		RegionHash: regionHash, ShotKey: shotKey, PreprocessKey: prepKey, DetectionKey: detKey,
		CandidateKey: candidateKey, SegmentationKey: segmentKey, RecognitionKey: recognitionKey,
		PreprocessParam: prepParam, DetectionParam: detParam, SegmentParam: segParam, RecognitionParam: recParam,
	}
}

func BuildShotGraph(shot *Screenshot, det *DetectionResult, recipe *Recipe, lib *TemplateLibrary) (*DependencyGraph, []LineKeys) {
	g := NewDependencyGraph()
	shotNode := "shot:" + shot.ShotID
	prepNode := "preprocess:" + shot.ShotID
	detParamNode := "detect_params:" + recipe.ParamVersion
	detNode := "detection:" + shot.ShotID
	g.AddNode(GraphNode{ID: shotNode, Kind: "screenshot_content", ShotID: shot.ShotID, ContentHash: shot.ContentHash})
	g.AddNode(GraphNode{ID: prepNode, Kind: "preprocess", ShotID: shot.ShotID, ParamVersion: recipe.StageParamVersion("preprocess", lib)})
	g.AddNode(GraphNode{ID: detParamNode, Kind: "detection_thresholds", ParamVersion: recipe.StageParamVersion("detection", lib)})
	g.AddNode(GraphNode{ID: detNode, Kind: "detection_result", ShotID: shot.ShotID, ParamVersion: recipe.StageParamVersion("detection", lib), CacheKey: firstKeyOrEmpty(det)})
	g.Connect(shotNode, prepNode, "content_hash->grayscale/normalize")
	g.Connect(prepNode, detNode, "normalized_pixels")
	g.Connect(detParamNode, detNode, "thresholds/stroke/gradient")

	keys := make([]LineKeys, 0, len(det.Lines))
	for _, line := range det.Lines {
		lk := BuildLineKeys(shot, line, recipe, lib)
		keys = append(keys, lk)
		candidateNode := fmt.Sprintf("candidate:%s:%s", shot.ShotID, line.LineID)
		segNode := fmt.Sprintf("segment:%s:%s", shot.ShotID, line.LineID)
		recNode := fmt.Sprintf("recognize:%s:%s", shot.ShotID, line.LineID)
		g.AddNode(GraphNode{ID: candidateNode, Kind: "line_candidate", ShotID: shot.ShotID, LineNo: line.LineNo, ContentHash: lk.RegionHash, ParamVersion: lk.DetectionParam, CacheKey: lk.CandidateKey})
		g.AddNode(GraphNode{ID: segNode, Kind: "character_split", ShotID: shot.ShotID, LineNo: line.LineNo, ContentHash: lk.RegionHash, ParamVersion: lk.SegmentParam, CacheKey: lk.SegmentationKey})
		g.AddNode(GraphNode{ID: recNode, Kind: "recognition", ShotID: shot.ShotID, LineNo: line.LineNo, ContentHash: lk.RegionHash, ParamVersion: lk.RecognitionParam, CacheKey: lk.RecognitionKey})
		g.Connect(detNode, candidateNode, "merged_components")
		g.Connect(candidateNode, segNode, "stroke_width_and_gaps")
		g.Connect(segNode, recNode, "character_boxes")
	}
	return g, keys
}

func firstKeyOrEmpty(_ *DetectionResult) string { return "" }
