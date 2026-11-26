package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"dialogueforge/dialogue"
)

type sampleManifest struct {
	Seed         int64                `json:"seed"`
	DialogueID   string               `json:"dialogue_id"`
	SampleRate   int64                `json:"sample_rate"`
	TotalSamples int64                `json:"total_samples"`
	Scenes       int                  `json:"scenes"`
	Segments     int                  `json:"segments"`
	Frames       int                  `json:"frames"`
	TargetLoud   int64                `json:"target_loudness"`
	Files        []sampleFileManifest `json:"files"`
	Calibration  []calibrationSummary `json:"calibration"`
}

type sampleFileManifest struct {
	Version       string   `json:"version"`
	Path          string   `json:"path"`
	SHA256        string   `json:"sha256"`
	Bytes         int      `json:"bytes"`
	Lossy         int      `json:"lossy"`
	LossyCodes    []string `json:"lossy_codes,omitempty"`
	Truncations   int      `json:"truncations"`
	LoadsAfter    bool     `json:"loads_after_export"`
	SourceVersion string   `json:"source_version_after_load"`
}

type calibrationSummary struct {
	SegmentID          string `json:"segment_id"`
	GainAdjustment     int64  `json:"gain_adjustment"`
	BaseLoudness       int64  `json:"base_loudness"`
	CalibratedLoudness int64  `json:"calibrated_loudness"`
	Reached            bool   `json:"reached"`
}

func main() {
	outDir := flag.String("out", "samples", "output directory")
	seed := flag.Int64("seed", 42, "deterministic generator seed")
	flag.Parse()
	if err := run(*outDir, *seed); err != nil {
		fmt.Fprintln(os.Stderr, "samplegen:", err)
		os.Exit(1)
	}
}

func run(outDir string, seed int64) error {
	project, err := dialogue.GenerateProject(seed)
	if err != nil {
		return err
	}
	envelopes, err := dialogue.MeasureProjectEnvelopes(project)
	if err != nil {
		return err
	}
	calibration, err := dialogue.CalibrateEnvelopes(project, envelopes)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	manifest := sampleManifest{
		Seed: seed, DialogueID: project.ID, SampleRate: project.SampleRate,
		TotalSamples: project.DeclaredTotalSamples, Scenes: len(project.Scenes),
		Segments: len(project.Segments()), Frames: len(project.Truth.Frames), TargetLoud: project.TargetLoudness,
	}
	for _, segment := range calibration.Segments {
		manifest.Calibration = append(manifest.Calibration, calibrationSummary{
			SegmentID: segment.SegmentID, GainAdjustment: segment.GainAdjustment,
			BaseLoudness: segment.BaseLoudness, CalibratedLoudness: segment.CalibratedLoudness, Reached: segment.Reached,
		})
	}
	versions := []struct {
		version string
		name    string
	}{
		{version: dialogue.Version1, name: "dialogue-v1.json"},
		{version: dialogue.Version2, name: "dialogue-v2.json"},
		{version: dialogue.Version3, name: "dialogue-v3.json"},
	}
	for _, item := range versions {
		exported, err := dialogue.ExportProject(project, item.version)
		if err != nil {
			return err
		}
		path := filepath.Join(outDir, item.name)
		if err := os.WriteFile(path, exported.Data, 0o644); err != nil {
			return err
		}
		loaded, err := dialogue.LoadProject(exported.Data)
		if err != nil {
			return fmt.Errorf("reload %s: %w", item.version, err)
		}
		digest := sha256.Sum256(exported.Data)
		entry := sampleFileManifest{
			Version: item.version, Path: path, SHA256: hex.EncodeToString(digest[:]), Bytes: len(exported.Data),
			Lossy: len(exported.Lossy), Truncations: len(exported.Truncations), LoadsAfter: true,
			SourceVersion: loaded.SourceVersion,
		}
		for _, issue := range exported.Lossy {
			entry.LossyCodes = append(entry.LossyCodes, string(issue.Code)+":"+issue.Path+":"+issue.Treatment)
		}
		manifest.Files = append(manifest.Files, entry)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "manifest.json"), data, 0o644)
}
