package sample

import (
	"encoding/json"
	"os"
	"path/filepath"

	"edgerefine/internal/errs"
	"edgerefine/internal/imagecodec"
	"edgerefine/internal/model"
)

type Bundle struct {
	Dir       string
	Manifest  model.Manifest
	Image     *model.GrayImage
	Reference []byte
}

func LoadManifest(path string) (model.Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return model.Manifest{}, errs.Decode(path, err)
	}
	var m model.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return model.Manifest{}, errs.Decode(path, err)
	}
	return m, nil
}

func LoadBundle(dir string) (*Bundle, error) {
	mp := filepath.Join(dir, "truth.json")
	m, err := LoadManifest(mp)
	if err != nil {
		return nil, err
	}
	if err := model.ValidateParams(m.Params, m.Width, m.Height); err != nil {
		return nil, err
	}
	img, err := imagecodec.LoadGrayPNG(filepath.Join(dir, m.ImageFile), m.Width, m.Height, m.Channels, m.BitDepth)
	if err != nil {
		return nil, err
	}
	refIm, err := imagecodec.LoadGrayPNG(filepath.Join(dir, m.ReferenceMaskFile), m.Width, m.Height, 1, 8)
	if err != nil {
		return nil, err
	}
	ref := refIm.Pix
	if got := model.HashBytes("reference-mask-v1:", ref); got != m.ReferenceMaskHash {
		return nil, errs.Decode(m.ReferenceMaskFile, nil)
	}
	if got := Fingerprint(m); got != m.TruthFingerprint {
		return nil, errs.Decode("truth_fingerprint", nil)
	}
	return &Bundle{Dir: dir, Manifest: m, Image: img, Reference: ref}, nil
}

func FindSampleDir(root, sampleID string) string {
	return filepath.Join(root, sampleID)
}
