package synth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"qkernel/internal/errs"
	"qkernel/internal/imagex"
)

const TruthSchema = "qkernel.truth/v1"

type FrameMeta struct {
	ID       string `json:"id"`
	Index    int    `json:"index"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Channels int    `json:"channels"`
}

type ObjectTruth struct {
	ID    string      `json:"id"`
	Kind  string      `json:"kind"`
	BBox  imagex.Rect `json:"bbox"`
	Color [3]int      `json:"color"`
}

type SampleTruth struct {
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Label    string `json:"label"`
	Expected [3]int `json:"expected"`
}

type ImplantTruth struct {
	BlockX        int         `json:"block_x"`
	BlockY        int         `json:"block_y"`
	StepIndex     int         `json:"step_index"`
	StepType      string      `json:"step_type"`
	Region        imagex.Rect `json:"region"`
	DeclaredShare float64     `json:"declared_share"`
}

type GroundTruth struct {
	Schema      string         `json:"schema"`
	Seed        int64          `json:"seed"`
	Frame       FrameMeta      `json:"frame"`
	Objects     []ObjectTruth  `json:"objects"`
	Watermark   imagex.Rect    `json:"watermark"`
	Samples     []SampleTruth  `json:"samples"`
	Implants    []ImplantTruth `json:"implants"`
	Fingerprint string         `json:"fingerprint"`
}

func (t *GroundTruth) ComputeFingerprint() string {
	cp := *t
	cp.Fingerprint = ""
	b, err := json.Marshal(&cp)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (t *GroundTruth) Seal() {
	t.Schema = TruthSchema
	t.Fingerprint = t.ComputeFingerprint()
}

func (t *GroundTruth) VerifyFingerprint() error {
	if t.Fingerprint == "" {
		return errs.New(errs.ESchema, "truth fingerprint is missing")
	}
	got := t.ComputeFingerprint()
	if got != t.Fingerprint {
		return errs.New(errs.ESchema, "truth fingerprint mismatch: want %s got %s", t.Fingerprint, got)
	}
	return nil
}

func SaveTruth(path string, truth *GroundTruth) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errs.Wrap(errs.EIO, err, "create truth directory")
	}
	b, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return errs.Wrap(errs.EBuild, err, "marshal truth")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return errs.Wrap(errs.EIO, err, "write truth")
	}
	if err := os.Rename(tmp, path); err != nil {
		return errs.Wrap(errs.EIO, err, "rename truth")
	}
	return nil
}

func LoadTruth(path string) (*GroundTruth, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, err, "read truth")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, errs.New(errs.ESchema, "truth json is not an object: %v", err)
	}
	for _, key := range []string{"schema", "seed", "frame", "objects", "watermark", "samples", "fingerprint"} {
		if _, ok := raw[key]; !ok {
			return nil, errs.New(errs.ESchema, "missing required field %q", key)
		}
	}
	var frameRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw["frame"], &frameRaw); err != nil {
		return nil, errs.New(errs.ESchema, "frame must be an object")
	}
	for _, key := range []string{"id", "index", "width", "height", "channels"} {
		if _, ok := frameRaw[key]; !ok {
			return nil, errs.New(errs.ESchema, "missing required field frame.%s", key)
		}
	}
	var wmRaw map[string]json.RawMessage
	if err := json.Unmarshal(raw["watermark"], &wmRaw); err != nil {
		return nil, errs.New(errs.ESchema, "watermark must be an object")
	}
	for _, key := range []string{"min_x", "min_y", "max_x", "max_y"} {
		if _, ok := wmRaw[key]; !ok {
			return nil, errs.New(errs.ESchema, "missing required field watermark.%s", key)
		}
	}
	if err := requireObjectFields(raw["objects"], "objects", "id", "kind", "bbox"); err != nil {
		return nil, err
	}
	if err := requireObjectFields(raw["samples"], "samples", "x", "y", "label", "expected"); err != nil {
		return nil, err
	}

	var truth GroundTruth
	if err := json.Unmarshal(b, &truth); err != nil {
		return nil, errs.New(errs.ESchema, "decode truth: %v", err)
	}
	if truth.Schema != TruthSchema {
		return nil, errs.New(errs.ESchema, "unsupported truth schema %q", truth.Schema)
	}
	if err := validateGeometry(&truth); err != nil {
		return nil, err
	}
	if err := truth.VerifyFingerprint(); err != nil {
		return nil, err
	}
	return &truth, nil
}

func requireObjectFields(raw json.RawMessage, arrayName string, fields ...string) error {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return errs.New(errs.ESchema, "%s must be an array of objects", arrayName)
	}
	for i, item := range items {
		for _, field := range fields {
			if _, ok := item[field]; !ok {
				return errs.New(errs.ESchema, "missing required field %s[%d].%s", arrayName, i, field)
			}
		}
	}
	return nil
}

func validateGeometry(t *GroundTruth) error {
	if t.Frame.Width <= 0 || t.Frame.Height <= 0 || t.Frame.Channels <= 0 {
		return errs.New(errs.ESchema, "frame dimensions and channels must be positive")
	}
	bounds := imagex.R(0, 0, t.Frame.Width, t.Frame.Height)
	for i, obj := range t.Objects {
		r := obj.BBox.Canonical()
		if r.Empty() {
			return errs.New(errs.ESchema, "objects[%d].bbox is empty", i)
		}
		if !bounds.ContainsRect(r) {
			return errs.New(errs.ESchema, "objects[%d].bbox %v outside frame %v", i, r, bounds)
		}
	}
	wm := t.Watermark.Canonical()
	if wm.Empty() {
		return errs.New(errs.ESchema, "watermark region is empty")
	}
	if !bounds.ContainsRect(wm) {
		return errs.New(errs.ESchema, "watermark %v outside frame %v", wm, bounds)
	}
	for i, s := range t.Samples {
		if !bounds.Contains(s.X, s.Y) {
			return errs.New(errs.ESchema, "samples[%d]=(%d,%d) outside frame", i, s.X, s.Y)
		}
	}
	for i, im := range t.Implants {
		if im.StepIndex <= 0 {
			return errs.New(errs.ESchema, "implants[%d].step_index must be positive", i)
		}
		r := im.Region.Canonical()
		if r.Empty() || !bounds.ContainsRect(r) {
			return errs.New(errs.ESchema, "implants[%d].region invalid or outside frame", i)
		}
	}
	return nil
}

func ValidateLoadedFrame(f *imagex.Frame, truth *GroundTruth) error {
	if f == nil || truth == nil {
		return errs.New(errs.ESchema, "frame and truth are required")
	}
	if f.Width != truth.Frame.Width || f.Height != truth.Frame.Height {
		return errs.New(errs.EMeta, "dimension conflict: image=%dx%d truth=%dx%d",
			f.Width, f.Height, truth.Frame.Width, truth.Frame.Height)
	}
	if f.Channels != truth.Frame.Channels {
		return errs.New(errs.EMeta, "channel conflict: image=%d truth=%d", f.Channels, truth.Frame.Channels)
	}
	return nil
}

func FramePaths(root, id string) (string, string) {
	return filepath.Join(root, "base", id+".png"), filepath.Join(root, "truth", id+".json")
}

func MustFrameID(index int) string {
	return fmt.Sprintf("frame_%03d", index)
}
