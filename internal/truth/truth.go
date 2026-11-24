package truth

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"texlab/internal/apperr"
	"texlab/internal/model"
)

type PatchTruth struct {
	Name   string    `json:"name"`
	X      int       `json:"x"`
	Y      int       `json:"y"`
	Width  int       `json:"width"`
	Height int       `json:"height"`
	RGBA   [4]uint16 `json:"rgba"`
}

type AlphaStep struct {
	X    int       `json:"x"`
	Y    int       `json:"y"`
	RGBA [4]uint16 `json:"rgba"`
}

type EntryTruth struct {
	EntryID     string       `json:"entry_id"`
	EntryIndex  int          `json:"entry_index"`
	ContentHash string       `json:"content_hash"`
	Depth       int          `json:"depth"`
	Channels    int          `json:"channels"`
	Layout      string       `json:"layout"`
	Alpha       string       `json:"alpha"`
	Width       int          `json:"width"`
	Height      int          `json:"height"`
	PixelsZ     string       `json:"pixels_zlib_b64"`
	Patches     []PatchTruth `json:"patches"`
	AlphaSteps  []AlphaStep  `json:"alpha_steps"`
}

type Truth struct {
	Schema            string       `json:"schema"`
	SourceFingerprint string       `json:"source_fingerprint"`
	PlanFingerprint   string       `json:"plan_fingerprint"`
	AlgorithmVersion  string       `json:"algorithm_version"`
	Width             int          `json:"width"`
	Height            int          `json:"height"`
	SourceDepth       int          `json:"source_depth"`
	SourceLayout      string       `json:"source_layout"`
	Entries           []EntryTruth `json:"entries"`
	FinalHash         string       `json:"final_hash"`
	Fingerprint       string       `json:"fingerprint"`
}

func (t Truth) ComputeFingerprint() string {
	parts := []string{
		"texlab-truth-fingerprint-v1", t.Schema, t.SourceFingerprint,
		t.PlanFingerprint, t.AlgorithmVersion, fmt.Sprintf("%d", t.Width),
		fmt.Sprintf("%d", t.Height), fmt.Sprintf("%d", t.SourceDepth), t.SourceLayout,
		t.FinalHash,
	}
	for _, e := range t.Entries {
		parts = append(parts, fmt.Sprintf("%s|%d|%s|%d|%d|%s|%s",
			e.EntryID, e.EntryIndex, e.ContentHash, e.Depth, e.Channels, e.Layout, e.Alpha))
	}
	return model.HashParts(parts...)
}

func Load(path string) (Truth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Truth{}, &apperr.Error{Code: apperr.EIO, Message: "read truth manifest", Path: path, Details: err.Error()}
	}
	var t Truth
	if err := json.Unmarshal(raw, &t); err != nil {
		return Truth{}, &apperr.Error{Code: apperr.EPlan, Message: "decode truth manifest", Field: "truth", Details: err.Error()}
	}
	if t.Schema != model.TruthSchema {
		return Truth{}, apperr.At(apperr.EPlan, 0, "truth.schema", "unsupported truth schema %q", t.Schema)
	}
	if t.Fingerprint == "" || t.ComputeFingerprint() != t.Fingerprint {
		return Truth{}, apperr.At(apperr.EPlan, 0, "truth.fingerprint", "truth fingerprint mismatch")
	}
	return t, nil
}

func ImageFromEntry(e EntryTruth) (model.Image, error) {
	return DecodePixels(e.PixelsZ, e.Width, e.Height, e.Depth, e.Channels, e.Layout, e.Alpha)
}

func EncodePixels(im model.Image) (string, error) {
	var raw bytes.Buffer
	var tmp [2]byte
	for _, v := range im.Pixels {
		binary.LittleEndian.PutUint16(tmp[:], v)
		raw.Write(tmp[:])
	}
	var z bytes.Buffer
	w, err := zlib.NewWriterLevel(&z, zlib.BestSpeed)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(raw.Bytes()); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(z.Bytes()), nil
}

func DecodePixels(encoded string, width, height, depth, channels int, layout, alpha string) (model.Image, error) {
	zraw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return model.Image{}, err
	}
	zr, err := zlib.NewReader(bytes.NewReader(zraw))
	if err != nil {
		return model.Image{}, err
	}
	raw, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		return model.Image{}, err
	}
	if len(raw) != width*height*4*2 {
		return model.Image{}, fmt.Errorf("truth pixel buffer length mismatch")
	}
	im := model.NewImage(width, height, depth, channels, layout, alpha)
	for i := range im.Pixels {
		im.Pixels[i] = binary.LittleEndian.Uint16(raw[i*2 : i*2+2])
	}
	return im, nil
}

func Write(path string, t Truth) error {
	t.Fingerprint = t.ComputeFingerprint()
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

func SortedPatches(v []PatchTruth) []PatchTruth {
	out := append([]PatchTruth(nil), v...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
