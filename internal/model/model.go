// Package model declares the on-disk manifest schema and the in-memory
// result structures shared by every pipeline stage. All time quantities are
// integer sample positions unless explicitly documented otherwise.
package model

// HighlightMode selects how the character highlight advances.
type HighlightMode string

const (
	HighlightLinear   HighlightMode = "linear"   // uniform per-character dwell
	HighlightWeighted HighlightMode = "weighted" // dwell proportional to weights
)

// SegmentManifest declares one synthetic speech-envelope segment.
type SegmentManifest struct {
	ID    int   `json:"id"`
	Start int64 `json:"start"` // inclusive, samples
	End   int64 `json:"end"`   // exclusive, samples
}

// CaptionManifest describes a single line of dialogue.
type CaptionManifest struct {
	ID        int    `json:"id"`
	Text      string `json:"text"`
	SegmentID int    `json:"segmentId"`
	// ExplicitWindow, when present, pins the display window inside the
	// referenced segment. Nil means "derive from the envelope".
	ExplicitWindow *Window `json:"explicitWindow,omitempty"`
}

// Window is a half-open sample interval [Start, End).
type Window struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Len returns the window length in samples.
func (w Window) Len() int64 { return w.End - w.Start }

// ReadingParams carries the narration-assist parameters.
type ReadingParams struct {
	// SpeechRate is characters per second, as a positive rational
	// Num/Den to keep every downstream conversion exact.
	SpeechRateNum int64 `json:"speechRateNum"`
	SpeechRateDen int64 `json:"speechRateDen"`
	// HighlightMode selects linear or weight-proportional advance.
	HighlightMode HighlightMode `json:"highlightMode"`
	// Weights holds one positive per-character duration weight per
	// caption, parallel to Captions.
	Weights [][]int64 `json:"weights"`
	// MaxReadRateNum/Den is the readability ceiling in characters per
	// second used by the minimum-readable-duration constraint.
	MaxReadRateNum int64 `json:"maxReadRateNum"`
	MaxReadRateDen int64 `json:"maxReadRateDen"`
}

// FrameParams declares the analysis frame geometry.
type FrameParams struct {
	SampleRate  int64 `json:"sampleRate"`
	FrameLength int64 `json:"frameLength"`
	FrameHop    int64 `json:"frameHop"`
}

// GroundTruth is produced analytically by the deterministic generator.
type GroundTruth struct {
	Captions []CaptionTruth `json:"captions"`
	// TotalReadSamples is the expected summed display duration.
	TotalReadSamples int64 `json:"totalReadSamples"`
}

// CaptionTruth is the expected timing for one caption.
type CaptionTruth struct {
	CaptionID int   `json:"captionId"`
	Start     int64 `json:"start"`
	End       int64 `json:"end"`
	// CharStarts[i] is the expected highlight onset of character i.
	CharStarts []int64 `json:"charStarts"`
}

// Manifest is the root document written by the sample generator.
type Manifest struct {
	ProjectID string            `json:"projectId"`
	Seed      int64             `json:"seed"`
	Frame     FrameParams       `json:"frame"`
	Segments  []SegmentManifest `json:"segments"`
	Captions  []CaptionManifest `json:"captions"`
	Reading   ReadingParams     `json:"reading"`
	// ToleranceSamples is the declared window-derivation tolerance.
	ToleranceSamples int64 `json:"toleranceSamples"`
	// SegmentChunkSamples is the fixed processing-chunk duration used
	// by the segment state machine.
	SegmentChunkSamples int64       `json:"segmentChunkSamples"`
	TotalSamples        int64       `json:"totalSamples"`
	Truth               GroundTruth `json:"truth"`
}

// ---- computed results ----

// BoundaryObservation records one detected envelope boundary.
type BoundaryObservation struct {
	SegmentID int    `json:"segmentId"`
	Kind      string `json:"kind"` // "start" or "end"
	// Raw is the pre-quantisation estimate, Quantized the hop-grid value.
	Raw       int64 `json:"raw"`
	Quantized int64 `json:"quantized"`
	// QuantError is |Quantized-Raw|, always < FrameHop.
	QuantError int64 `json:"quantError"`
}

// DecisionKind enumerates the minimum-duration trade-off means.
type DecisionKind string

const (
	DecisionNone          DecisionKind = "none"
	DecisionExtend        DecisionKind = "extend_gap"
	DecisionCompressDwell DecisionKind = "compress_dwell"
	DecisionWholeSegment  DecisionKind = "whole_segment"
)

// Decision records one deterministic trade-off for the audit log.
type Decision struct {
	CaptionID int          `json:"captionId"`
	Kind      DecisionKind `json:"kind"`
	Reason    string       `json:"reason"`
}

// CaptionResult is the fully derived timing for one caption.
type CaptionResult struct {
	CaptionID int    `json:"captionId"`
	Window    Window `json:"window"`
	// CharStarts[i] / CharEnds[i] bound the highlight of character i.
	CharStarts []int64 `json:"charStarts"`
	CharEnds   []int64 `json:"charEnds"`
	// SegmentIdx is the processing chunk that owns this caption.
	SegmentIdx int `json:"segmentIdx"`
}

// CaptionErrorTerms holds the per-caption measured error terms.
type CaptionErrorTerms struct {
	CaptionID int `json:"captionId"`
	// BoundaryQuant is the summed boundary quantisation error feeding
	// this caption's window.
	BoundaryQuant int64 `json:"boundaryQuant"`
	// WindowDeviation is |derived-truth| accumulated for the window.
	WindowDeviation int64 `json:"windowDeviation"`
	// SchedResidual is the summed per-character rounding residual.
	SchedResidual int64 `json:"schedResidual"`
	// DurationDeviation is |derived window length - truth length|.
	DurationDeviation int64 `json:"durationDeviation"`
}

// ErrorReport is the full error-propagation ledger for a project.
type ErrorReport struct {
	Captions []CaptionErrorTerms `json:"captions"`
	// Cumulative upper bounds along caption order, derived only from
	// observed parameters (frame geometry, sample-rate quantisation
	// step, weight denominators) - never from ground truth.
	BoundaryQuantBound   []int64 `json:"boundaryQuantBound"`
	WindowDeviationBound []int64 `json:"windowDeviationBound"`
	SchedResidualBound   []int64 `json:"schedResidualBound"`
	DurationBound        []int64 `json:"durationBound"`
	// TotalDurationDeviation is |sum(derived) - truth total|.
	TotalDurationDeviation int64 `json:"totalDurationDeviation"`
	// TotalDurationBound is the project-level duration bound.
	TotalDurationBound int64 `json:"totalDurationBound"`
}

// Result is the frozen output of a full pipeline run.
type Result struct {
	ProjectID  string                `json:"projectId"`
	Frame      FrameParams           `json:"frame"`
	Boundaries []BoundaryObservation `json:"boundaries"`
	Captions   []CaptionResult       `json:"captions"`
	Decisions  []Decision            `json:"decisions"`
	Report     ErrorReport           `json:"report"`
}
