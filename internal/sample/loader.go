package sample

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"

	"vqa/internal/errs"
)

// ManifestName is the fixed truth manifest file name.
const ManifestName = "manifest.json"

// WriteManifest atomically writes the manifest (temp file + rename).
func WriteManifest(dataDir string, m *Manifest) error {
	payload, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(dataDir, ManifestName)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// LoadManifest reads and validates the truth manifest. Missing fields and
// unsupported quality levels return E_SCHEMA naming the offending field.
func LoadManifest(dataDir string) (*Manifest, error) {
	payload, err := os.ReadFile(filepath.Join(dataDir, ManifestName))
	if err != nil {
		return nil, errs.New(errs.ESchema, "manifest unreadable").With("path", filepath.Join(dataDir, ManifestName))
	}
	var m Manifest
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, errs.New(errs.ESchema, "manifest is not valid JSON").With("detail", err.Error())
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate enforces structural completeness of the manifest. Expected values
// must be present; the pipeline never infers missing truth.
func (m *Manifest) Validate() error {
	check := func(ok bool, field, detail string) error {
		if ok {
			return nil
		}
		return errs.New(errs.ESchema, "missing or invalid manifest field").With("field", field).With("detail", detail)
	}
	if err := check(m.SchemaVersion != "", "schema_version", "must be set"); err != nil {
		return err
	}
	if err := check(m.Width > 0, "width", "must be positive"); err != nil {
		return err
	}
	if err := check(m.Height > 0, "height", "must be positive"); err != nil {
		return err
	}
	if err := check(m.Channels > 0, "channels", "must be positive"); err != nil {
		return err
	}
	if err := check(m.Frames > 0, "frames", "must be positive"); err != nil {
		return err
	}
	if err := check(m.BlockSize > 0, "block_size", "must be positive"); err != nil {
		return err
	}
	if m.Width%m.BlockSize != 0 || m.Height%m.BlockSize != 0 {
		return errs.New(errs.ESchema, "frame dimensions must be divisible by block_size").
			With("field", "block_size")
	}
	if len(m.QualityLevels) == 0 {
		return errs.New(errs.ESchema, "missing quality levels").With("field", "quality_levels")
	}
	for _, q := range m.QualityLevels {
		if !QualitySupported(q) {
			return errs.New(errs.ESchema, "unsupported quality level").
				With("field", "quality_levels").WithInt("value", int64(q))
		}
	}
	if len(m.SampleFrames) == 0 {
		return errs.New(errs.ESchema, "missing sample frame list").With("field", "sample_frames")
	}
	for _, sf := range m.SampleFrames {
		if sf < 0 || sf >= m.Frames {
			return errs.New(errs.ESchema, "sample frame index out of range").
				With("field", "sample_frames").WithInt("value", int64(sf))
		}
	}
	if len(m.Fingerprints) == 0 {
		return errs.New(errs.ESchema, "missing truth fingerprints").With("field", "fingerprints")
	}
	for _, q := range m.QualityLevels {
		for _, sf := range m.SampleFrames {
			key := fingerprintKey(q, sf)
			fp, ok := m.Fingerprints[key]
			if !ok {
				return errs.New(errs.ESchema, "missing fingerprint entry").With("field", "fingerprints."+key)
			}
			if fp.Frame != sf {
				return errs.New(errs.ESchema, "fingerprint frame mismatch").With("field", "fingerprints."+key+".frame")
			}
			if !(fp.PSNRMin > 0) || !(fp.PSNRMax >= fp.PSNRMin) {
				return errs.New(errs.ESchema, "invalid psnr interval").With("field", "fingerprints."+key+".psnr")
			}
			if len(fp.EdgePixels) == 0 {
				return errs.New(errs.ESchema, "missing edge pixels").With("field", "fingerprints."+key+".edge_pixels")
			}
			want := (m.Width / m.BlockSize) * (m.Height / m.BlockSize)
			if len(fp.BlockQuantiles) != want {
				return errs.New(errs.ESchema, "block quantile count mismatch").
					With("field", "fingerprints."+key+".block_quantiles").
					WithInt("expected", int64(want)).WithInt("actual", int64(len(fp.BlockQuantiles)))
			}
		}
	}
	return nil
}

// DecodeFrame parses one binary frame file, verifying magic and CRC. Corrupt
// payloads return E_DECODE rather than being silently repaired.
func DecodeFrame(payload []byte) (*Frame, error) {
	if len(payload) < len(FrameMagic)+11+4 {
		return nil, errs.New(errs.EDecode, "frame payload truncated")
	}
	if string(payload[:len(FrameMagic)]) != FrameMagic {
		return nil, errs.New(errs.EDecode, "bad frame magic")
	}
	off := len(FrameMagic)
	if payload[off] != 1 {
		return nil, errs.New(errs.EDecode, "unsupported frame format version").WithInt("version", int64(payload[off]))
	}
	w := int(binary.BigEndian.Uint16(payload[off+1 : off+3]))
	h := int(binary.BigEndian.Uint16(payload[off+3 : off+5]))
	ch := int(payload[off+5])
	n := int(binary.BigEndian.Uint32(payload[off+6 : off+10]))
	bodyStart := off + 11
	bodyEnd := bodyStart + n
	if bodyEnd+4 != len(payload) {
		return nil, errs.New(errs.EDecode, "frame length does not match header")
	}
	wantCRC := binary.BigEndian.Uint32(payload[bodyEnd:])
	gotCRC := crc32.ChecksumIEEE(payload[:bodyEnd])
	if wantCRC != gotCRC {
		return nil, errs.New(errs.EDecode, "frame crc mismatch")
	}
	if w == 0 || h == 0 || ch == 0 || n != w*h*ch {
		return nil, errs.New(errs.EDecode, "frame header fields inconsistent")
	}
	pix := make([]uint8, n)
	copy(pix, payload[bodyStart:bodyEnd])
	return &Frame{Width: w, Height: h, Channels: ch, Pix: pix}, nil
}

// Sequence is a loaded pair of original and reconstructed frame sequences.
type Sequence struct {
	Manifest *Manifest
	Orig     []*Frame
	Recon    []*Frame
	Quality  int
}

// LoadSequence validates the manifest, then loads both sequences for one
// quality level, checking frame count and per-frame dimensions.
func LoadSequence(dataDir string, quality int) (*Sequence, error) {
	m, err := LoadManifest(dataDir)
	if err != nil {
		return nil, err
	}
	if !QualitySupported(quality) {
		return nil, errs.New(errs.ESchema, "unsupported quality level").
			With("field", "quality").WithInt("value", int64(quality))
	}
	seq := &Sequence{Manifest: m, Quality: quality}
	seq.Orig, err = loadFrames(filepath.Join(dataDir, "orig"), m)
	if err != nil {
		return nil, err
	}
	seq.Recon, err = loadFrames(filepath.Join(dataDir, "recon", fmt.Sprintf("q%d", quality)), m)
	if err != nil {
		return nil, err
	}
	for i := range seq.Orig {
		if err := checkDims(seq.Orig[i], m); err != nil {
			return nil, err
		}
		if err := checkDims(seq.Recon[i], m); err != nil {
			return nil, err
		}
	}
	return seq, nil
}

func loadFrames(dir string, m *Manifest) ([]*Frame, error) {
	frames := make([]*Frame, 0, m.Frames)
	for i := 0; i < m.Frames; i++ {
		payload, err := os.ReadFile(filepath.Join(dir, frameName(i)))
		if err != nil {
			return nil, errs.New(errs.EDecode, "frame file missing").With("path", filepath.Join(dir, frameName(i)))
		}
		fr, err := DecodeFrame(payload)
		if err != nil {
			return nil, err
		}
		frames = append(frames, fr)
	}
	if len(frames) != m.Frames {
		return nil, errs.New(errs.ESchema, "frame count mismatch").
			With("field", "frames").WithInt("expected", int64(m.Frames)).WithInt("actual", int64(len(frames)))
	}
	return frames, nil
}

func checkDims(f *Frame, m *Manifest) error {
	if f.Width != m.Width || f.Height != m.Height || f.Channels != m.Channels {
		return errs.New(errs.EDecode, "frame dimensions differ from manifest").
			With("field", "width/height/channels")
	}
	if len(f.Pix) != m.Width*m.Height*m.Channels {
		return errs.New(errs.EDecode, "frame pixel count inconsistent")
	}
	return nil
}
