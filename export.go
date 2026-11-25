package dialogue

import (
	"encoding/json"
	"fmt"
	"strings"
)

func stringPtr(v string) *string { return &v }
func intPtr(v int) *int          { return &v }
func int64Ptr(v int64) *int64    { return &v }

// ExportProject renders the unified model in the requested source schema.
// Downgrades return a non-fatal LossReport containing one E_LOSSY item per
// unsupported construct.
func ExportProject(p *Project, version int) ([]byte, *LossReport, error) {
	loss := &LossReport{FromVersion: p.SchemaVersion, ToVersion: version, Losses: []LossItem{}}
	var doc any
	switch version {
	case 1:
		doc = exportV1(p, loss)
	case 2:
		doc = exportV2(p, loss)
	case 3:
		doc = exportV3(p)
	default:
		return nil, nil, schemaError(p.SourceFile, "$.schema_version", "unsupported export version %d", version)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, loss, err
	}
	return data, loss, nil
}

func appendMarkerLoss(p *Project, loss *LossReport) {
	for i, marker := range p.Markers {
		loss.Losses = append(loss.Losses, LossItem{
			Code:        CodeLossy,
			Path:        fmt.Sprintf("$.calibration_markers[%d]", i),
			Original:    marker,
			Disposition: "drop_optional_calibration_marker",
			Message:     fmt.Sprintf("schema version does not express marker %s", marker.ID),
		})
	}
}

func appendSceneLoss(groups []SceneGroup, path string, loss *LossReport) {
	for i, group := range groups {
		groupPath := fmt.Sprintf("%s[%d]", path, i)
		loss.Losses = append(loss.Losses, LossItem{
			Code: CodeLossy,
			Path: groupPath,
			Original: map[string]any{
				"name":         group.Name,
				"clip_ids":     group.ClipIDs,
				"child_groups": len(group.Children),
			},
			Disposition: "flatten_nested_scene_tree_to_scene_path",
			Message:     "nested scene grouping is not representable in flat schema",
		})
		children := make([]SceneGroup, 0, len(group.Children))
		for _, child := range group.Children {
			if child != nil {
				children = append(children, *child)
			}
		}
		appendSceneLoss(children, groupPath+".children", loss)
	}
}

func exportV1(p *Project, loss *LossReport) rawV1 {
	appendMarkerLoss(p, loss)
	appendSceneLoss(p.SceneTree, "$.scenes", loss)
	clips := make([]rawClipV1, 0, len(p.Clips))
	for i, clip := range p.Clips {
		startMS, _, _ := SamplesToMillis(clip.StartSample, int64(p.SampleRate), RoundNearest, "start_ms")
		durationMS, _, _ := SamplesToMillis(int64(len(clip.Samples)), int64(p.SampleRate), RoundNearest, "duration_ms")
		gainPM, _, _ := BasisToPermille(clip.GainBP, RoundNearest, "gain_pm")
		scene := strings.Join(clip.ScenePath, "/")
		clips = append(clips, rawClipV1{
			ID:                stringPtr(clip.ID),
			Scene:             scene,
			StartMS:           int64Ptr(startMS),
			DurationMS:        int64Ptr(durationMS),
			GainPM:            int64Ptr(gainPM),
			ToneHz:            clip.ToneHz,
			PCM:               append([]int16(nil), clip.Samples...),
			DeclaredSamples:   clip.DeclaredSampleCount,
			ForceStageFailure: clip.ForceStageFailure,
		})
		_ = i
	}
	sessionPM, _, _ := BasisToPermille(p.SessionGainBP, RoundNearest, "session_gain_pm")
	maxPM, _, _ := BasisToPermille(p.MaxAdjustBP, RoundNearest, "max_adjust_pm")
	budgetPM, _, _ := BasisToPermille(p.TotalBudgetBP, RoundNearest, "total_budget_pm")
	targetPM, _ := resolveRounding(p.TargetAmpMicro, LinearScale/PermilleScale, RoundNearest,
		"target_rms_pm", "target_amp_micro/1000", 0)
	return rawV1{
		Schema:        SchemaV1ID,
		DialogueID:    stringPtr(p.DialogueID),
		Seed:          p.Seed,
		SampleRate:    intPtr(p.SampleRate),
		FrameRate:     intPtr(p.FrameRate),
		SessionGainPM: int64Ptr(sessionPM),
		TargetRMSPM:   int64Ptr(targetPM),
		MaxAdjustPM:   int64Ptr(maxPM),
		TotalBudgetPM: int64Ptr(budgetPM),
		Clips:         clips,
	}
}

func exportV2(p *Project, loss *LossReport) rawV2 {
	appendMarkerLoss(p, loss)
	appendSceneLoss(p.SceneTree, "$.scenes", loss)
	clips := make([]rawClipV2, 0, len(p.Clips))
	for _, clip := range p.Clips {
		declared := clip.DeclaredSampleCount
		if declared == 0 {
			declared = len(clip.Samples)
		}
		clips = append(clips, rawClipV2{
			ClipID:              stringPtr(clip.ID),
			ScenePath:           strings.Join(clip.ScenePath, "/"),
			StartSample:         int64Ptr(clip.StartSample),
			SampleCount:         intPtr(len(clip.Samples)),
			GainBP:              int64Ptr(clip.GainBP),
			ToneHz:              clip.ToneHz,
			PCM:                 append([]int16(nil), clip.Samples...),
			DeclaredSampleCount: intPtr(declared),
			ForceStageFailure:   clip.ForceStageFailure,
		})
	}
	targetCDB, _ := resolveRounding(p.TargetLoudMDB, 10, RoundNearest,
		"targetLoudnessCentibel", "target_loud_mdb/10", 0)
	return rawV2{
		SchemaVersion: intPtr(2),
		DialogueID:    stringPtr(p.DialogueID),
		Seed:          p.Seed,
		SampleRate:    intPtr(p.SampleRate),
		FPS:           intPtr(p.FrameRate),
		SessionGainBP: int64Ptr(p.SessionGainBP),
		TargetLoudCDB: int64Ptr(targetCDB),
		MaxAdjustBP:   int64Ptr(p.MaxAdjustBP),
		TotalBudgetBP: int64Ptr(p.TotalBudgetBP),
		Clips:         clips,
	}
}

func exportV3(p *Project) rawV3 {
	clipByID := make(map[string]*Clip, len(p.Clips))
	for _, clip := range p.Clips {
		clipByID[clip.ID] = clip
	}
	scenes := make([]rawSceneV3, 0, len(p.SceneTree))
	for _, group := range p.SceneTree {
		scenes = append(scenes, exportSceneV3(group, clipByID))
	}
	return rawV3{
		SchemaVersion: intPtr(3),
		DialogueID:    stringPtr(p.DialogueID),
		Seed:          p.Seed,
		Audio: &rawAudioV3{
			SampleRate: intPtr(p.SampleRate),
			FrameRate:  intPtr(p.FrameRate),
		},
		Calibration: &rawCalibrationV3{
			SessionGainBP:  int64Ptr(p.SessionGainBP),
			TargetLoudMDB:  int64Ptr(p.TargetLoudMDB),
			TargetAmpMicro: int64Ptr(p.TargetAmpMicro),
			MaxAdjustBP:    int64Ptr(p.MaxAdjustBP),
			TotalBudgetBP:  int64Ptr(p.TotalBudgetBP),
		},
		Scenes:  scenes,
		Markers: append([]Marker(nil), p.Markers...),
	}
}

func exportSceneV3(group SceneGroup, clipByID map[string]*Clip) rawSceneV3 {
	out := rawSceneV3{Name: group.Name}
	for _, id := range group.ClipIDs {
		clip := clipByID[id]
		if clip == nil {
			continue
		}
		linear, _ := BasisToLinearMicro(clip.GainBP, "gain_linear_micro")
		declared := clip.DeclaredSampleCount
		if declared == 0 {
			declared = len(clip.Samples)
		}
		out.Clips = append(out.Clips, rawClipV3{
			ID:                  stringPtr(clip.ID),
			StartSample:         int64Ptr(clip.StartSample),
			SampleCount:         intPtr(len(clip.Samples)),
			GainLinearMicro:     int64Ptr(linear),
			ToneHz:              clip.ToneHz,
			PCM:                 append([]int16(nil), clip.Samples...),
			DeclaredSampleCount: intPtr(declared),
			ForceStageFailure:   clip.ForceStageFailure,
		})
	}
	for _, child := range group.Children {
		if child != nil {
			out.Children = append(out.Children, exportSceneV3(*child, clipByID))
		}
	}
	return out
}

// GenerateDocuments creates canonical JSON documents for all three schemas.
func GenerateDocuments(project *Project) (map[int][]byte, map[int]*LossReport, error) {
	docs := make(map[int][]byte, 3)
	losses := make(map[int]*LossReport, 3)
	for _, version := range []int{1, 2, 3} {
		data, loss, err := ExportProject(project, version)
		if err != nil {
			return nil, nil, err
		}
		docs[version] = data
		losses[version] = loss
	}
	return docs, losses, nil
}
