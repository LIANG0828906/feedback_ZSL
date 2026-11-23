package project

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"loudnessworkbench/apperr"
)

// WriteVersions emits all supported interchange representations. Relative
// audio references intentionally differ between versions.
func WriteVersions(projectDir string, program *Program) (map[string]string, error) {
	if err := program.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "cannot create project directory", err)
	}
	paths := map[string]string{
		SourceV1: filepath.Join(projectDir, "program.v1.json"),
		SourceV2: filepath.Join(projectDir, "program.v2.json"),
		SourceV3: filepath.Join(projectDir, "program.v3.json"),
	}
	if err := writeV1(paths[SourceV1], projectDir, program); err != nil {
		return nil, err
	}
	if err := writeV2(paths[SourceV2], projectDir, program); err != nil {
		return nil, err
	}
	if err := writeV3(paths[SourceV3], projectDir, program); err != nil {
		return nil, err
	}
	return paths, nil
}

func writeV1(path, projectDir string, p *Program) error {
	segments := make([]map[string]any, len(p.Segments))
	for i, segment := range p.Segments {
		segments[i] = map[string]any{
			"segment_id": segment.ID,
			"channels":   segment.Channels,
			"source": map[string]any{
				"kind":           "wav-slice",
				"path":           relativeAudioPath(projectDir, segment.SourcePath),
				"offset_samples": 0,
				"sample_count":   segment.Frames(),
				"sha256":         segment.ContentSHA256,
			},
		}
	}
	document := map[string]any{
		"version":        1,
		"program_id":     p.ProjectID,
		"sample_rate_hz": p.SampleRateHz,
		"target_lufs":    p.TargetLUFS,
		"frame_ms":       p.FrameMs,
		"hop_ms":         p.HopMs,
		"gate": map[string]any{
			"absolute_lufs": p.Gate.AbsoluteLUFS,
			"relative_lu":   p.Gate.RelativeLU,
		},
		"compression_chain": []string{"compress", "limit"},
		"compressor": map[string]any{
			"threshold_dbfs": p.Compressor.ThresholdDBFS,
			"ratio":          p.Compressor.Ratio,
			"attack_ms":      p.Compressor.AttackMs,
			"release_ms":     p.Compressor.ReleaseMs,
		},
		"limiter": map[string]any{
			"ceiling_dbtp": p.Limiter.CeilingDBTP,
			"lookahead_ms": p.Limiter.LookaheadMs,
		},
		"segments": segments,
	}
	return writeJSON(path, document)
}

func writeV2(path, projectDir string, p *Program) error {
	assets := make([]map[string]any, len(p.Segments))
	segments := make([]map[string]any, len(p.Segments))
	for i, segment := range p.Segments {
		assetID := fmt.Sprintf("asset-%02d", i+1)
		assets[i] = map[string]any{
			"assetId":       assetID,
			"wavPath":       relativeAudioPath(projectDir, segment.SourcePath),
			"contentSha256": segment.ContentSHA256,
			"sampleRateHz":  p.SampleRateHz,
		}
		segments[i] = map[string]any{
			"id":            segment.ID,
			"assetRef":      assetID,
			"channels":      segment.Channels,
			"offsetSamples": 0,
			"frameCount":    segment.Frames(),
		}
	}
	document := map[string]any{
		"schemaVersion": "2.0",
		"programId":     p.ProjectID,
		"sampleRateHz":  p.SampleRateHz,
		"targetLufs":    p.TargetLUFS,
		"frameMs":       p.FrameMs,
		"hopMs":         p.HopMs,
		"gating": map[string]any{
			"absoluteLufs":   p.Gate.AbsoluteLUFS,
			"relativeDropLu": p.Gate.RelativeLU,
		},
		"stageOrder": []string{"compressor", "limiter"},
		"compressor": map[string]any{
			"thresholdDbfs": p.Compressor.ThresholdDBFS,
			"ratio":         p.Compressor.Ratio,
			"attackMs":      p.Compressor.AttackMs,
			"releaseMs":     p.Compressor.ReleaseMs,
		},
		"limiter": map[string]any{
			"ceilingDbtp": p.Limiter.CeilingDBTP,
			"lookaheadMs": p.Limiter.LookaheadMs,
		},
		"assets":   assets,
		"segments": segments,
	}
	return writeJSON(path, document)
}

func writeV3(path, projectDir string, p *Program) error {
	clips := map[string]any{}
	segments := make([]map[string]any, len(p.Segments))
	for i, segment := range p.Segments {
		clipKey := fmt.Sprintf("clip-%02d", i+1)
		clips[clipKey] = map[string]any{
			"file":          relativeAudioPath(projectDir, segment.SourcePath),
			"contentSha256": segment.ContentSHA256,
			"sampleRateHz":  p.SampleRateHz,
		}
		segments[i] = map[string]any{
			"key":           segment.ID,
			"clip":          clipKey,
			"channels":      segment.Channels,
			"offsetSamples": 0,
			"lengthSamples": segment.Frames(),
		}
	}
	document := map[string]any{
		"format": "loudness-workbench/3",
		"program": map[string]any{
			"id":           p.ProjectID,
			"sampleRateHz": p.SampleRateHz,
			"targetLufs":   p.TargetLUFS,
		},
		"analysis": map[string]any{
			"frameMs": p.FrameMs,
			"hopMs":   p.HopMs,
			"gates": []map[string]any{
				{"kind": "absolute", "value": p.Gate.AbsoluteLUFS, "unit": "LUFS"},
				{"kind": "relative", "value": math.Pow(10, -p.Gate.RelativeLU/10), "unit": "energy_ratio"},
			},
		},
		"processingStages": []map[string]any{
			{
				"type":          "compressor",
				"thresholdDbfs": p.Compressor.ThresholdDBFS,
				"ratio":         p.Compressor.Ratio,
				"attackMs":      p.Compressor.AttackMs,
				"releaseMs":     p.Compressor.ReleaseMs,
			},
			{
				"type":        "limiter",
				"ceilingDbtp": p.Limiter.CeilingDBTP,
				"lookaheadMs": p.Limiter.LookaheadMs,
			},
		},
		"media": map[string]any{
			"baseDir": "",
			"clips":   clips,
		},
		"segments": segments,
	}
	return writeJSON(path, document)
}

func relativeAudioPath(projectDir, source string) string {
	relative, err := filepath.Rel(projectDir, source)
	if err != nil {
		return filepath.ToSlash(source)
	}
	return filepath.ToSlash(relative)
}

func writeJSON(path string, document any) error {
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cannot encode project", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "cannot write project", err).WithPath(path)
	}
	return nil
}
