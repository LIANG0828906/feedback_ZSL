// Package project defines the canonical internal program model and the
// versioned interchange loaders/writers.
package project

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
	"loudnessworkbench/dsp"
)

const (
	SourceV1 = "v1"
	SourceV2 = "v2"
	SourceV3 = "v3"
)

var CanonicalStageOrder = []string{"compressor", "limiter"}

type Segment struct {
	Index         int          `json:"index"`
	ID            string       `json:"id"`
	Channels      int          `json:"channels"`
	SourcePath    string       `json:"source_path"`
	ContentSHA256 string       `json:"content_sha256"`
	Source        audio.Signal `json:"-"`
}

func (s Segment) Frames() int {
	return s.Source.Frames()
}

type Program struct {
	ProjectID     string
	SourceVersion string
	SampleRateHz  int
	TargetLUFS    float64
	FrameMs       float64
	HopMs         float64
	Gate          dsp.GateConfig
	Compressor    dsp.CompressorConfig
	Limiter       dsp.LimiterConfig
	StageOrder    []string
	Segments      []Segment
}

func (p *Program) ChainConfig() dsp.ChainConfig {
	return dsp.ChainConfig{
		TargetLUFS: p.TargetLUFS,
		FrameMs:    p.FrameMs,
		HopMs:      p.HopMs,
		Gate:       p.Gate,
		Compressor: p.Compressor,
		Limiter:    p.Limiter,
	}
}

func (p *Program) MeasureConfig() dsp.MeasureConfig {
	return dsp.MeasureConfig{
		SampleRateHz: p.SampleRateHz,
		FrameMs:      p.FrameMs,
		HopMs:        p.HopMs,
		Gate:         p.Gate,
	}
}

func (p *Program) CloneWithChain(chain dsp.ChainConfig) *Program {
	clone := *p
	clone.TargetLUFS = chain.TargetLUFS
	clone.FrameMs = chain.FrameMs
	clone.HopMs = chain.HopMs
	clone.Gate = chain.Gate
	clone.Compressor = chain.Compressor
	clone.Limiter = chain.Limiter
	clone.StageOrder = append([]string(nil), p.StageOrder...)
	clone.Segments = make([]Segment, len(p.Segments))
	for i, segment := range p.Segments {
		clone.Segments[i] = segment
		clone.Segments[i].Source = segment.Source.Clone()
	}
	return &clone
}

func (p *Program) Validate() error {
	if p.ProjectID == "" {
		return apperr.New(apperr.CodeVersion, "program id is required").WithField("program_id")
	}
	if !dsp.SupportedSampleRate(p.SampleRateHz) {
		return apperr.New(apperr.CodeParameter, "unsupported sample rate").
			WithField("sample_rate_hz").WithActual(p.SampleRateHz)
	}
	if err := p.ChainConfig().Validate(p.SampleRateHz); err != nil {
		return err
	}
	if len(p.StageOrder) != len(CanonicalStageOrder) {
		return apperr.New(apperr.CodeVersion, "compression stage list is missing or incomplete").WithField("processing_stages")
	}
	for i, stage := range CanonicalStageOrder {
		if p.StageOrder[i] != stage {
			return apperr.New(apperr.CodeVersion, "compression stages must be ordered compressor then limiter").
				WithField(fmt.Sprintf("processing_stages[%d]", i)).
				WithExpected(stage).WithActual(p.StageOrder[i])
		}
	}
	if len(p.Segments) == 0 {
		return apperr.New(apperr.CodeVersion, "at least one program segment is required").WithField("segments")
	}
	seen := make(map[string]struct{}, len(p.Segments))
	for i, segment := range p.Segments {
		loudnessPath := fmt.Sprintf("segments[%d]", i)
		if segment.ID == "" {
			return apperr.New(apperr.CodeVersion, "segment id is required").WithField(loudnessPath + ".id")
		}
		if _, exists := seen[segment.ID]; exists {
			return apperr.New(apperr.CodeSchema, "duplicate segment id").WithField(loudnessPath + ".id").WithActual(segment.ID)
		}
		seen[segment.ID] = struct{}{}
		if segment.Channels != 1 && segment.Channels != 2 {
			return apperr.New(apperr.CodeParameter, "segment must be mono or stereo").
				WithField(loudnessPath + ".channels").WithActual(segment.Channels)
		}
		if segment.Source.SampleRateHz != p.SampleRateHz {
			return apperr.New(apperr.CodeSchema, "segment sample rate differs from program").
				WithField(loudnessPath + ".sample_rate_hz").
				WithExpected(p.SampleRateHz).WithActual(segment.Source.SampleRateHz)
		}
		if segment.Source.Channels != segment.Channels {
			return apperr.New(apperr.CodeSchema, "segment channel count differs from audio").
				WithField(loudnessPath + ".channels").
				WithExpected(segment.Channels).WithActual(segment.Source.Channels)
		}
		if segment.Source.Frames() == 0 {
			return apperr.New(apperr.CodeSchema, "segment contains no audio frames").
				WithField(loudnessPath + ".frame_count").WithActual(0)
		}
		expectedHash := audio.ContentHash(segment.Source)
		if segment.ContentSHA256 == "" {
			p.Segments[i].ContentSHA256 = expectedHash
		} else if segment.ContentSHA256 != expectedHash {
			return apperr.New(apperr.CodeSchema, "segment content hash mismatch").
				WithField(loudnessPath + ".content_sha256").
				WithExpected(segment.ContentSHA256).WithActual(expectedHash)
		}
	}
	return nil
}

type DecisionParams struct {
	SampleRateHz int                  `json:"sample_rate_hz"`
	TargetLUFS   float64              `json:"target_lufs"`
	FrameMs      float64              `json:"frame_ms"`
	HopMs        float64              `json:"hop_ms"`
	Gate         dsp.GateConfig       `json:"gate"`
	Compressor   dsp.CompressorConfig `json:"compressor"`
	Limiter      dsp.LimiterConfig    `json:"limiter"`
	StageOrder   []string             `json:"stage_order"`
	Segments     []DecisionSegment    `json:"segments"`
}

type DecisionSegment struct {
	ID            string `json:"id"`
	Channels      int    `json:"channels"`
	Frames        int    `json:"frames"`
	ContentSHA256 string `json:"content_sha256"`
}

// DecisionParamsHash deliberately excludes source-version and path spelling:
// equivalent v1/v2/v3 projects produce the same processing decisions.
func (p *Program) DecisionParamsHash() string {
	params := DecisionParams{
		SampleRateHz: p.SampleRateHz,
		TargetLUFS:   p.TargetLUFS,
		FrameMs:      p.FrameMs,
		HopMs:        p.HopMs,
		Gate:         p.Gate,
		Compressor:   p.Compressor,
		Limiter:      p.Limiter,
		StageOrder:   append([]string(nil), p.StageOrder...),
		Segments:     make([]DecisionSegment, len(p.Segments)),
	}
	for i, segment := range p.Segments {
		params.Segments[i] = DecisionSegment{
			ID:            segment.ID,
			Channels:      segment.Channels,
			Frames:        segment.Frames(),
			ContentSHA256: segment.ContentSHA256,
		}
	}
	data, _ := json.Marshal(params)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

func (p *Program) NormalizedAudioPaths() []string {
	paths := make([]string, len(p.Segments))
	for i, segment := range p.Segments {
		absolute, err := filepath.Abs(segment.SourcePath)
		if err != nil {
			absolute = segment.SourcePath
		}
		paths[i] = filepath.ToSlash(absolute)
	}
	return paths
}

func nearlyEqual(a, b, epsilon float64) bool {
	if math.IsInf(a, -1) && math.IsInf(b, -1) {
		return true
	}
	return math.Abs(a-b) <= epsilon
}
