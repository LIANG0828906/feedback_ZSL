// Package loader reads a generated media project from disk and validates
// its structure (E_SCHEMA) and parameters (E_PARA) before any processing.
package loader

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"

	"captionx/internal/errs"
	"captionx/internal/model"
	"captionx/internal/samplegen"
)

// Project is a fully validated media project ready for the pipeline.
type Project struct {
	Dir      string
	Manifest model.Manifest
	Audio    samplegen.AudioFile
	// ContentHash is the deterministic hash of manifest+audio bytes.
	ContentHash string
	// CharCounts[i] is the rune count of caption i (parallel to
	// Manifest.Captions).
	CharCounts []int
}

// Legal parameter ranges reported with E_PARA failures.
const (
	MinSampleRate = 1000
	MaxSampleRate = 384000
	MinFrameLen   = 16
)

// Load reads and validates the project rooted at dir.
func Load(dir string) (*Project, error) {
	mPath := filepath.Join(dir, "manifest.json")
	mBytes, err := os.ReadFile(mPath)
	if err != nil {
		return nil, errs.IO(fmt.Sprintf("read manifest: %v", err))
	}
	var m model.Manifest
	if err := json.Unmarshal(mBytes, &m); err != nil {
		return nil, errs.Schema(-1, "manifest", fmt.Sprintf("invalid JSON: %v", err))
	}
	aPath := filepath.Join(dir, "audio.json")
	aBytes, err := os.ReadFile(aPath)
	if err != nil {
		return nil, errs.IO(fmt.Sprintf("read audio: %v", err))
	}
	var audio samplegen.AudioFile
	if err := json.Unmarshal(aBytes, &audio); err != nil {
		return nil, errs.Schema(-1, "audio", fmt.Sprintf("invalid JSON: %v", err))
	}

	p := &Project{Dir: dir, Manifest: m, Audio: audio}
	if err := p.validate(mBytes, aBytes); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Project) validate(mBytes, aBytes []byte) error {
	m := &p.Manifest

	// Frame geometry and declared ranges.
	if m.Frame.SampleRate < MinSampleRate || m.Frame.SampleRate > MaxSampleRate {
		return errs.Para("frame.sampleRate",
			fmt.Sprintf("must be in [%d,%d], got %d", MinSampleRate, MaxSampleRate, m.Frame.SampleRate))
	}
	if m.Frame.FrameLength < MinFrameLen || m.Frame.FrameLength > m.Frame.SampleRate {
		return errs.Para("frame.frameLength",
			fmt.Sprintf("must be in [%d,%d], got %d", MinFrameLen, m.Frame.SampleRate, m.Frame.FrameLength))
	}
	if m.Frame.FrameHop < 1 || m.Frame.FrameHop > m.Frame.FrameLength {
		return errs.Para("frame.frameHop",
			fmt.Sprintf("must be in [1,%d], got %d", m.Frame.FrameLength, m.Frame.FrameHop))
	}
	if m.ToleranceSamples < 0 {
		return errs.Para("toleranceSamples", "must be >= 0")
	}
	if m.SegmentChunkSamples <= 0 {
		return errs.Para("segmentChunkSamples", "must be > 0")
	}

	// Audio timeline consistency with the manifest declaration.
	if p.Audio.SampleRate != m.Frame.SampleRate {
		return errs.Schema(-1, "frame.sampleRate",
			fmt.Sprintf("audio declares %d, manifest declares %d", p.Audio.SampleRate, m.Frame.SampleRate))
	}
	if int64(len(p.Audio.Samples)) < m.TotalSamples {
		return errs.Schema(-1, "audio.samples",
			fmt.Sprintf("audio has %d samples, manifest declares %d", len(p.Audio.Samples), m.TotalSamples))
	}

	// Segment structure: ordered, non-overlapping, inside the timeline.
	segByID := map[int]model.SegmentManifest{}
	var prevEnd int64
	for i, s := range m.Segments {
		if s.End <= s.Start {
			return errs.Schema(-1, fmt.Sprintf("segments[%d]", i), "segment end must exceed start")
		}
		if i > 0 && s.Start < prevEnd {
			return errs.Schema(-1, fmt.Sprintf("segments[%d].start", i), "segments overlap")
		}
		if s.End > m.TotalSamples {
			return errs.Schema(-1, fmt.Sprintf("segments[%d].end", i), "segment exceeds timeline")
		}
		if _, dup := segByID[s.ID]; dup {
			return errs.Schema(-1, fmt.Sprintf("segments[%d].id", i), "duplicate segment id")
		}
		segByID[s.ID] = s
		prevEnd = s.End
	}

	// Reading-assist parameters.
	r := &m.Reading
	if r.SpeechRateNum <= 0 || r.SpeechRateDen <= 0 {
		return errs.Para("reading.speechRate", "speech rate must be a positive finite rational")
	}
	if r.MaxReadRateNum <= 0 || r.MaxReadRateDen <= 0 {
		return errs.Para("reading.maxReadRate", "max read rate must be a positive finite rational")
	}
	if r.HighlightMode != model.HighlightLinear && r.HighlightMode != model.HighlightWeighted {
		return errs.Para("reading.highlightMode", "must be \"linear\" or \"weighted\"")
	}
	if len(r.Weights) != len(m.Captions) {
		return errs.Schema(-1, "reading.weights",
			fmt.Sprintf("got %d weight rows for %d captions", len(r.Weights), len(m.Captions)))
	}

	// Captions and per-caption weights.
	p.CharCounts = make([]int, len(m.Captions))
	truthByID := map[int]model.CaptionTruth{}
	for _, t := range m.Truth.Captions {
		truthByID[t.CaptionID] = t
	}
	for i, c := range m.Captions {
		nCh := utf8.RuneCountInString(c.Text)
		p.CharCounts[i] = nCh
		if nCh == 0 {
			return errs.Schema(c.ID, "text", "caption text is empty")
		}
		seg, ok := segByID[c.SegmentID]
		if !ok {
			return errs.Schema(c.ID, "segmentId", fmt.Sprintf("no segment with id %d", c.SegmentID))
		}
		if c.ExplicitWindow != nil {
			w := c.ExplicitWindow
			if w.End <= w.Start {
				return errs.Schema(c.ID, "explicitWindow", "window end must exceed start")
			}
			if w.Start < seg.Start || w.End > seg.End {
				return errs.Schema(c.ID, "explicitWindow",
					fmt.Sprintf("window [%d,%d) escapes segment [%d,%d)", w.Start, w.End, seg.Start, seg.End))
			}
		}
		w := r.Weights[i]
		if len(w) != nCh {
			return errs.Schema(c.ID, "weights",
				fmt.Sprintf("got %d weights for %d characters", len(w), nCh))
		}
		for j, x := range w {
			if x <= 0 {
				return errs.Para("weights",
					fmt.Sprintf("caption %d char %d: weight must be positive finite, got %d", c.ID, j, x))
			}
		}
		t, ok := truthByID[c.ID]
		if !ok {
			return errs.Schema(c.ID, "truth", "missing ground-truth entry")
		}
		if len(t.CharStarts) != nCh {
			return errs.Schema(c.ID, "truth.charStarts",
				fmt.Sprintf("got %d truth char starts for %d characters", len(t.CharStarts), nCh))
		}
	}

	p.ContentHash = contentHash(mBytes, aBytes)
	return nil
}
