package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const AlgorithmVersion = "recverify-algo-v1"
const SchemaVersion = "recverify-truth-v1"
const FrameMagic = "RCVFRM01"

type Rect struct {
	X      int     `json:"x"`
	Y      int     `json:"y"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
	XFloat float64 `json:"x_float,omitempty"`
	YFloat float64 `json:"y_float,omitempty"`
	WFloat float64 `json:"w_float,omitempty"`
	HFloat float64 `json:"h_float,omitempty"`
}

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Primitive struct {
	Kind   string  `json:"kind"`
	Label  string  `json:"label"`
	Color  [3]int  `json:"color"`
	BBox   Rect    `json:"bbox"`
	Points []Point `json:"points,omitempty"`
}

type FrameTruth struct {
	Frame           int         `json:"frame"`
	TimestampNS     int64       `json:"timestamp_ns"`
	File            string      `json:"file"`
	ContentSHA256   string      `json:"content_sha256"`
	BaselineFile    string      `json:"baseline_file"`
	BaselineSHA256  string      `json:"baseline_sha256"`
	WatermarkRegion Rect        `json:"watermark_region"`
	Primitives      []Primitive `json:"primitives"`
}

type Replacement struct {
	Scenario   string `json:"scenario"`
	StartFrame int    `json:"start_frame"`
	EndFrame   int    `json:"end_frame"`
	Region     Rect   `json:"region"`
	Mask       []bool `json:"mask"`
}

type Truth struct {
	Schema           string       `json:"schema"`
	AlgorithmVersion string       `json:"algorithm_version"`
	Seed             int64        `json:"seed"`
	Clip             string       `json:"clip"`
	Width            int          `json:"width"`
	Height           int          `json:"height"`
	Channels         int          `json:"channels"`
	FrameCount       int          `json:"frame_count"`
	FrameTolerance   int          `json:"frame_tolerance"`
	DefaultBlockSize int          `json:"default_block_size"`
	Frames           []FrameTruth `json:"frames"`
	Replacement      Replacement  `json:"replacement"`
	TruthFingerprint string       `json:"truth_fingerprint_sha256,omitempty"`
}

type DatasetManifest struct {
	Schema string     `json:"schema"`
	Seed   int64      `json:"seed"`
	Clips  []ClipInfo `json:"clips"`
}

type ClipInfo struct {
	Name  string `json:"name"`
	Dir   string `json:"dir"`
	Truth string `json:"truth"`
}

type Frame struct {
	No          int
	TimestampNS int64
	Width       int
	Height      int
	Channels    int
	Pixels      []byte
	ContentHash string
}

type FrameSet struct {
	Root      string
	TruthPath string
	Truth     *Truth
	Frames    []*Frame
	Baselines [][]byte
}

func HashBytes(prefix string, b []byte) string {
	sum := sha256.Sum256(append([]byte(prefix), b...))
	return hex.EncodeToString(sum[:])
}

func HashJSON(v any) (string, []byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), b, nil
}

func FingerprintTruth(t *Truth) (string, error) {
	copy := *t
	copy.TruthFingerprint = ""
	key, _, err := HashJSON(&copy)
	return key, err
}

func (r Rect) Contains(x, y int) bool {
	return x >= r.X && y >= r.Y && x < r.X+r.Width && y < r.Y+r.Height
}

func (r Rect) ValidFor(w, h int) bool {
	return r.X >= 0 && r.Y >= 0 && r.Width > 0 && r.Height > 0 &&
		r.X+r.Width <= w && r.Y+r.Height <= h
}
