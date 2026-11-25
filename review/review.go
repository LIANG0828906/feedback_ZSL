// Package review independently recomputes persisted chunk products and bands.
package review

import (
	"encoding/json"
	"fmt"

	ct "colortrace"
	"colortrace/reference"
)

// Violation is the first persisted-product mismatch found by review.
type Violation struct {
	Stage    int       `json:"stage"`
	ChunkID  string    `json:"chunk_id,omitempty"`
	Plane    string    `json:"plane,omitempty"`
	Coord    *ct.Coord `json:"coord,omitempty"`
	Field    string    `json:"field"`
	Expected any       `json:"expected,omitempty"`
	Actual   any       `json:"actual,omitempty"`
}

// Report is the independent review result.
type Report struct {
	OK                         bool           `json:"ok"`
	FirstViolation             *Violation     `json:"first_violation,omitempty"`
	RecomputedTruthFingerprint string         `json:"recomputed_truth_fingerprint"`
	Band                       *ct.BandReport `json:"band,omitempty"`
	CheckedUnits               int            `json:"checked_units"`
}

// RunStore loads only persisted artifacts and recomputes without the scheduler
// or production chunk executor.
func RunStore(store ct.CheckpointStore, expectedTruthFingerprint string) (*Report, error) {
	rawProject, err := store.GetArtifact("project.json")
	if err != nil {
		return nil, err
	}
	if rawProject == nil {
		return nil, fmt.Errorf("persisted project.json artifact is missing")
	}
	var project ct.Project
	if err := json.Unmarshal(rawProject, &project); err != nil {
		return nil, &ct.Error{Code: ct.ESchema, Path: "artifact:project.json", Message: err.Error()}
	}
	meta, err := store.Meta()
	if err != nil {
		return nil, err
	}
	if expectedTruthFingerprint == "" {
		expectedTruthFingerprint = meta.TruthFingerprint
	}
	return Run(store, &project, expectedTruthFingerprint)
}

// Run compares every persisted unit against the independent reference.
func Run(store ct.CheckpointStore, project *ct.Project, expectedTruthFingerprint string) (*Report, error) {
	truth, referenceSamples, err := reference.BuildTruth(project)
	if err != nil {
		return nil, err
	}
	report := &Report{RecomputedTruthFingerprint: truth.Fingerprint}
	if truth.Fingerprint != expectedTruthFingerprint {
		report.FirstViolation = &Violation{
			Field: "truth_fingerprint", Expected: expectedTruthFingerprint, Actual: truth.Fingerprint,
		}
		return report, nil
	}
	units, err := store.Units(project.ProjectID)
	if err != nil {
		return nil, err
	}
	unitByKey := map[string]ct.UnitRecord{}
	for _, record := range units {
		unitByKey[ct.UnitKey(record.Stage, record.ChunkID)] = record
	}
	for stageIdx, sample := range referenceSamples {
		first := project.Planes[0]
		plane := sample.Planes[first.Name]
		for _, chunk := range ct.AllChunks(plane.Width, plane.Height, project.ChunkSize) {
			chunkID := ct.ChunkID(chunk)
			key := ct.UnitKey(stageIdx, chunkID)
			record, ok := unitByKey[key]
			if !ok || record.State != ct.StateCommitted {
				actual := "missing"
				if ok {
					actual = string(record.State)
				}
				report.FirstViolation = &Violation{
					Stage: stageIdx, ChunkID: chunkID, Field: "committed_unit",
					Expected: string(ct.StateCommitted), Actual: actual,
				}
				return report, nil
			}
		}
	}
	for _, record := range units {
		if record.State != ct.StateCommitted {
			continue
		}
		report.CheckedUnits++
		var chunk ct.ChunkResult
		if err := json.Unmarshal(record.OutputJSON, &chunk); err != nil {
			report.FirstViolation = &Violation{
				Stage: record.Stage, ChunkID: record.ChunkID, Field: "output_json",
				Expected: "valid chunk JSON", Actual: err.Error(),
			}
			return report, nil
		}
		if violation := compareChunk(project, referenceSamples, record, &chunk); violation != nil {
			report.FirstViolation = violation
			return report, nil
		}
	}
	band, err := reference.FindFirstBand(project, referenceSamples)
	if err != nil {
		return nil, err
	}
	report.Band = band
	report.OK = true
	return report, nil
}

func compareChunk(project *ct.Project, samples []*ct.Sample, record ct.UnitRecord, chunk *ct.ChunkResult) *Violation {
	if record.Stage < 0 || record.Stage >= len(samples) {
		return &Violation{Stage: record.Stage, ChunkID: record.ChunkID, Field: "stage", Expected: "valid stage", Actual: record.Stage}
	}
	if chunk.CacheKey != record.CacheKey {
		return &Violation{Stage: record.Stage, ChunkID: record.ChunkID, Field: "cache_key", Expected: record.CacheKey, Actual: chunk.CacheKey}
	}
	expectedKey := ct.ComputeCacheKey(chunk.InputHash, project.Stages[record.Stage], chunk.UpstreamKeys)
	if expectedKey != chunk.CacheKey {
		return &Violation{Stage: record.Stage, ChunkID: record.ChunkID, Field: "cache_key_parameter_mix", Expected: expectedKey, Actual: chunk.CacheKey}
	}
	refStage := samples[record.Stage]
	for _, planeName := range chunk.PlaneNames {
		pc := chunk.Planes[planeName]
		refPlane := refStage.Planes[planeName]
		if pc == nil || refPlane == nil {
			return &Violation{Stage: record.Stage, ChunkID: record.ChunkID, Plane: planeName, Field: "plane", Expected: true, Actual: pc != nil}
		}
		for ly := 0; ly < pc.Height; ly++ {
			for lx := 0; lx < pc.Width; lx++ {
				x := pc.Region.MinX + lx
				y := pc.Region.MinY + ly
				local := ly*pc.Width + lx
				global := y*refPlane.Width + x
				coord := ct.Coord{X: x, Y: y}
				chunkID := ct.ChunkID(ct.ChunkCoordAt(x, y, project.ChunkSize))
				if pc.Codes[local] != refPlane.Codes[global] {
					return violation(record.Stage, record.ChunkID, chunkID, planeName, coord, "code", refPlane.Codes[global], pc.Codes[local])
				}
				if pc.Values[local] != refPlane.Values[global] {
					return violation(record.Stage, record.ChunkID, chunkID, planeName, coord, "output_fixed", refPlane.Values[global], pc.Values[local])
				}
				persistedLedger := pc.Ledgers[local]
				refLedger := refPlane.Ledgers[global]
				if persistedLedger.Remainder != refLedger.Remainder {
					return violation(record.Stage, record.ChunkID, chunkID, planeName, coord, "remainder", refLedger.Remainder, persistedLedger.Remainder)
				}
				if persistedLedger.Breakdown != refLedger.Breakdown {
					return violation(record.Stage, record.ChunkID, chunkID, planeName, coord, "breakdown", refLedger.Breakdown, persistedLedger.Breakdown)
				}
				if persistedLedger.CumulativeDeviation != refLedger.CumulativeDeviation {
					return violation(record.Stage, record.ChunkID, chunkID, planeName, coord, "cumulative_deviation", refLedger.CumulativeDeviation, persistedLedger.CumulativeDeviation)
				}
			}
		}
	}
	return nil
}

func violation(stage int, recordChunkID, derivedChunkID, plane string, coord ct.Coord, field string, expected, actual any) *Violation {
	chunkID := recordChunkID
	if chunkID == "" {
		chunkID = derivedChunkID
	}
	return &Violation{
		Stage: stage, ChunkID: chunkID, Plane: plane, Coord: &coord,
		Field: field, Expected: expected, Actual: actual,
	}
}
