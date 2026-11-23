package morph

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type ErrorEvidence struct {
	Code     Code   `json:"code"`
	Field    string `json:"field,omitempty"`
	PathID   string `json:"path_id,omitempty"`
	Sample   int    `json:"sample,omitempty"`
	Message  string `json:"message"`
	Observed bool   `json:"observed"`
}

// ExerciseErrorChain creates local mutations and reports the observed stable
// error for every required code. It never touches files outside workDir.
func ExerciseErrorChain(scene *Scene, truth *Truth, results []*SampleResult, workDir string) (map[Code]ErrorEvidence, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	out := map[Code]ErrorEvidence{}
	record := func(code Code, err error) {
		if err == nil {
			out[code] = ErrorEvidence{Code: code, Message: "no error produced"}
			return
		}
		e, _ := AsError(err)
		ev := ErrorEvidence{Code: CodeOf(err), Observed: true, Message: err.Error()}
		if e != nil {
			ev.Field, ev.PathID, ev.Sample = e.Field, e.PathID, e.Sample
			ev.Code = e.Code
		}
		out[code] = ev
	}
	bad := filepath.Join(workDir, "corrupt.json")
	_ = os.WriteFile(bad, []byte("{broken"), 0o644)
	_, err := LoadScene(bad)
	record(CodeDecode, err)

	open := cloneSceneJSON(scene)
	last := len(open.Paths[0].Keyframes[0].Segments) - 1
	if open.Paths[0].Keyframes[0].Segments[last].P3 != nil {
		open.Paths[0].Keyframes[0].Segments[last].P3.X += 0.2
	} else {
		open.Paths[0].Keyframes[0].Segments[last].P2.X += 0.2
	}
	record(CodePath, ValidateScene(open))

	badKey := cloneSceneJSON(scene)
	badKey.Paths[0].Keyframes[1].Time = -1
	record(CodeKey, ValidateScene(badKey))

	badStyle := cloneSceneJSON(scene)
	badStyle.Paths[0].Opacity = 1.1
	record(CodeStyle, ValidateScene(badStyle))

	_, err = FlattenPath(&scene.Paths[0], scene.SampleTimes[0], EvalConfig{Interpolation: "linear", Scale: 1, MidTolerance: 0})
	record(CodeParam, err)

	tl := NewTimeline(scene)
	record(CodeState, tl.Transition(0, scene.Paths[0].ID, StageFlatten, StateCommitted))

	if len(results) > 0 && len(results[0].PathStats) > 0 {
		broken := make([]*SampleResult, len(results))
		for i, r := range results {
			broken[i] = cloneResultForDiag(r)
		}
		broken[0].PathStats[0].CoverageArea *= 2
		broken[0].PathStats[0].CoverageBBox = PixelRect{}
		_, err = CompareQuality(scene, truth, ConsistencyTolerance{RelativeArea: 1e-9, BBoxIoU: 1, MeanCoverage: 1}, results, broken)
		record(CodeScale, err)
	}
	if len(results) > 0 && len(results[0].Gray) > 0 {
		broken := make([]*SampleResult, len(results))
		for i, r := range results {
			broken[i] = cloneResultForDiag(r)
		}
		broken[0].Gray[len(broken[0].Gray)/2] ^= 0x7f
		_, err = CompareResultFrames(scene, []string{"baseline", "mutated"}, [][]*SampleResult{results, broken})
		record(CodeDiverge, err)
	}
	return out, nil
}

func cloneSceneJSON(s *Scene) *Scene {
	data, _ := json.Marshal(s)
	var out Scene
	_ = json.Unmarshal(data, &out)
	return &out
}

func cloneResultForDiag(r *SampleResult) *SampleResult {
	if r == nil {
		return nil
	}
	data, _ := json.Marshal(r)
	var out SampleResult
	_ = json.Unmarshal(data, &out)
	out.Gray = append([]uint8(nil), r.Gray...)
	out.PathStats = append([]PathFrameStats(nil), r.PathStats...)
	out.Flattened = nil
	for _, f := range r.Flattened {
		cp := *f
		cp.Points = append([]Vec2(nil), f.Points...)
		out.Flattened = append(out.Flattened, &cp)
	}
	return &out
}

func EvidenceComplete(ev map[Code]ErrorEvidence) (bool, []string) {
	required := []Code{CodeDecode, CodePath, CodeKey, CodeStyle, CodeParam, CodeScale, CodeState, CodeDiverge}
	var missing []string
	for _, c := range required {
		if e, ok := ev[c]; !ok || !e.Observed || e.Code != c {
			missing = append(missing, string(c))
		}
	}
	return len(missing) == 0, missing
}

func FormatEvidence(ev map[Code]ErrorEvidence) string {
	required := []Code{CodeDecode, CodePath, CodeKey, CodeStyle, CodeParam, CodeScale, CodeState, CodeDiverge}
	s := ""
	for _, c := range required {
		e := ev[c]
		s += fmt.Sprintf("%s observed=%v field=%s path=%s: %s\n", c, e.Observed, e.Field, e.PathID, e.Message)
	}
	return s
}
