package dialoguecal

import (
	"context"
	"encoding/json"
	"path/filepath"
)

type InjectionResult struct {
	Name               string         `json:"name"`
	ExpectedCode       Code           `json:"expected_code"`
	Code               Code           `json:"code,omitempty"`
	OK                 bool           `json:"ok"`
	Message            string         `json:"message,omitempty"`
	Path               string         `json:"path,omitempty"`
	Context            map[string]any `json:"context,omitempty"`
	SourceHashBefore   string         `json:"source_hash_before"`
	SourceHashAfter    string         `json:"source_hash_after"`
	SourceRemainedSame bool           `json:"source_remained_same"`
}

func businessResult(name string, expected Code, sourceHash string, err error) InjectionResult {
	result := InjectionResult{Name: name, ExpectedCode: expected, SourceHashBefore: sourceHash, SourceHashAfter: sourceHash, SourceRemainedSame: true}
	be, ok := AsBusinessError(err)
	if !ok {
		result.OK = false
		result.Message = errString(err)
		return result
	}
	result.Code = be.Code
	result.OK = be.Code == expected
	result.Message = be.Detail
	result.Path = be.Path
	result.Context = be.Context
	return result
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func mutateV1JSON(raw []byte, mutate func(map[string]any)) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	mutate(doc)
	return json.Marshal(doc)
}

// RunInjectionSuite executes every required failure injection in an isolated
// checkpoint directory. It never mutates fixture.Project.
func RunInjectionSuite(fixture *Fixture, checkpointDir string) []InjectionResult {
	sourceHash := ContentHash(fixture.Project)
	results := []InjectionResult{}

	badGainJSON, _ := mutateV1JSON(fixture.V1JSON, func(doc map[string]any) {
		segments := doc["segments"].([]any)
		first := segments[0].(map[string]any)
		first["gain_permille"] = -125
	})
	_, err := LoadProjectJSON(badGainJSON)
	results = append(results, businessResult("negative_gain", CodeGainNegative, sourceHash, err))

	_, err = ApplyEdits(fixture.Project, Edit{Kind: EditTarget, SegmentID: "seg-0", TargetLevelPermille: 5000})
	results = append(results, businessResult("target_loudness_out_of_range", CodeTargetLoudnessRange, sourceHash, err))

	_, err = ApplyEdits(fixture.Project, Edit{Kind: EditFrameRate, FramesPerSecond: 0})
	results = append(results, businessResult("frame_rate_zero", CodeFrameRateZero, sourceHash, err))

	reversed := fixture.Project.Phonemes[0]
	reversed.StartSample = reversed.EndSample + 10
	_, err = ApplyEdits(fixture.Project, Edit{Kind: EditPhoneme, PhonemeID: reversed.ID, Phoneme: &reversed})
	results = append(results, businessResult("phoneme_start_after_end", CodePhonemeOrder, sourceHash, err))

	conflict := []byte(`{
		"segment_id":"seg-conflict",
		"target_loudness_db_x100":-2000,
		"target_amplitude_permille":251
	}`)
	_, _, err = LoadMixedFragmentJSON(conflict)
	results = append(results, businessResult("schema_semantic_conflict_without_watermark", CodeSchemaConflict, sourceHash, err))

	_, err = NewFrameGate(Budget{Workers: 0, FramesPerWorker: 8, MaxWorkers: 8})
	results = append(results, businessResult("worker_budget_out_of_range", CodeBudgetWorkers, sourceHash, err))

	_, err = NewFrameGate(Budget{Workers: 2, FramesPerWorker: -1, MaxWorkers: 8})
	results = append(results, businessResult("buffer_budget_out_of_range", CodeBudgetBuffer, sourceHash, err))

	_, err = FillGateForTest(Budget{Workers: 1, FramesPerWorker: 1, MaxWorkers: 8})
	results = append(results, businessResult("buffer_exhausted_backpressure", CodeBackpressure, sourceHash, err))

	unknown := append([]byte(nil), fixture.V1JSON...)
	var header map[string]any
	_ = json.Unmarshal(unknown, &header)
	header["schema"] = "dialogue.unknown.v9"
	unknown, _ = json.Marshal(header)
	_, err = LoadProjectJSON(unknown)
	results = append(results, businessResult("unknown_schema_version", CodeSchemaUnknown, sourceHash, err))

	truncDir := filepath.Join(checkpointDir, "truncated")
	_ = WriteTruncatedMeta(truncDir, sourceHash)
	truncStore, errStore := NewFileCheckpointStore(truncDir)
	if errStore != nil {
		results = append(results, businessResult("truncated_checkpoint", CodeCheckpointTruncated, sourceHash, errStore))
	} else {
		_, err = truncStore.LoadCheckpoint(context.Background(), sourceHash)
		_ = truncStore.Close()
		results = append(results, businessResult("truncated_checkpoint", CodeCheckpointTruncated, sourceHash, err))
	}

	versionDir := filepath.Join(checkpointDir, "version-mismatch")
	versionStore, errStore := NewFileCheckpointStore(versionDir)
	if errStore != nil {
		results = append(results, businessResult("checkpoint_version_mismatch", CodeCheckpointVersion, sourceHash, errStore))
	} else {
		fake := FrameResult{Frame: Frame{GlobalIndex: 0, SegmentID: "seg-0", IndexInSegment: 0}, Calibration: Calibration{State: StateCalibrated}}
		_ = versionStore.CommitFrame(context.Background(), "snapshot-version-old", fake, 0)
		_, err = versionStore.LoadCheckpoint(context.Background(), sourceHash)
		_ = versionStore.Close()
		results = append(results, businessResult("checkpoint_version_mismatch", CodeCheckpointVersion, sourceHash, err))
	}

	afterHash := ContentHash(fixture.Project)
	for i := range results {
		results[i].SourceHashAfter = afterHash
		results[i].SourceRemainedSame = beforeAfterEqual(sourceHash, afterHash)
	}
	return results
}

func beforeAfterEqual(a, b string) bool { return a == b }

// ResolveConflictWithWatermark demonstrates the disambiguation success path.
func ResolveConflictWithWatermark() (*Segment, string, error) {
	doc := []byte(`{
		"segment_id":"seg-conflict",
		"target_loudness_db_x100":-2000,
		"target_amplitude_permille":251,
		"calibration_watermark_db_x100":-2000
	}`)
	return LoadMixedFragmentJSON(doc)
}
