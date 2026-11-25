package sample

import (
	"encoding/json"
	"os"
	"path/filepath"

	"channelpipe"
)

type ImageHeader struct {
	Format        string                `json:"format"`
	ID            string                `json:"id"`
	Width         int                   `json:"width"`
	Height        int                   `json:"height"`
	Depth         int                   `json:"depth"`
	Channels      []channelpipe.Channel `json:"channels"`
	PixelEncoding string                `json:"pixel_encoding"`
	DataFile      string                `json:"data_file"`
	DataSHA256    string                `json:"data_sha256"`
	PreviewFile   string                `json:"preview_file,omitempty"`
}

type Manifest struct {
	Seed             int64          `json:"seed"`
	PlanFingerprint  string         `json:"plan_fingerprint"`
	TruthFingerprint string         `json:"truth_fingerprint"`
	ParamVersion     string         `json:"param_version"`
	AlgorithmVersion string         `json:"algorithm_version"`
	Images           []ImageHeader  `json:"images"`
	PlanFiles        map[int]string `json:"plan_files"`
	LossReports      map[int]string `json:"loss_reports,omitempty"`
	TruthFile        string         `json:"truth_file"`
	ChunkWidth       int            `json:"chunk_width"`
	ChunkHeight      int            `json:"chunk_height"`
}

type WrittenSample struct {
	Directory string
	Manifest  Manifest
	V1Data    []byte
	V2Data    []byte
	V3Data    []byte
}

func WriteDirectory(directory string, set *SampleSet, chunkWidth, chunkHeight int) (*WrittenSample, error) {
	dirs := []string{directory, filepath.Join(directory, "images"), filepath.Join(directory, "plans"), filepath.Join(directory, "truth")}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, channelpipe.WrapError(channelpipe.CodeIO, err, dir, "cannot create sample directory")
		}
	}
	manifest := Manifest{
		Seed:             set.Seed,
		TruthFingerprint: set.Truth.Fingerprint,
		ParamVersion:     channelpipe.ParamVersion,
		AlgorithmVersion: channelpipe.AlgorithmVersion,
		PlanFiles:        map[int]string{},
		LossReports:      map[int]string{},
		TruthFile:        filepath.ToSlash(filepath.Join("truth", "truth.json")),
		ChunkWidth:       chunkWidth,
		ChunkHeight:      chunkHeight,
	}
	planFingerprint, err := channelpipe.Fingerprint(set.Set)
	if err != nil {
		return nil, err
	}
	manifest.PlanFingerprint = planFingerprint

	for _, image := range set.Images {
		blob, err := channelpipe.EncodeBuffer(&image.Buffer)
		if err != nil {
			return nil, err
		}
		dataName := image.ID + ".pixels.bin"
		headerName := image.ID + ".chimg.json"
		if err := os.WriteFile(filepath.Join(directory, "images", dataName), blob, 0o644); err != nil {
			return nil, channelpipe.WrapError(channelpipe.CodeIO, err, dataName, "cannot write pixels")
		}
		header := ImageHeader{
			Format:        "channelpipe.raw-image/v1",
			ID:            image.ID,
			Width:         image.Width,
			Height:        image.Height,
			Depth:         image.Depth,
			Channels:      image.Channels,
			PixelEncoding: pixelEncoding(image.Depth),
			DataFile:      dataName,
			DataSHA256:    channelpipe.SHA256Hex(blob),
			PreviewFile:   image.ID + ".preview.png",
		}
		if err := channelpipe.WritePNGPreview(filepath.Join(directory, "images", header.PreviewFile), &image.Buffer); err != nil {
			return nil, err
		}
		if err := writeJSON(filepath.Join(directory, "images", headerName), header); err != nil {
			return nil, err
		}
		manifest.Images = append(manifest.Images, header)
	}

	exports := map[int][]byte{}
	for _, version := range []int{1, 2, 3} {
		data, loss, err := channelpipe.ExportSetAs(set.Set, version)
		if err != nil {
			return nil, err
		}
		exports[version] = data
		name := filepath.Join("plans", planFileName(version))
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o644); err != nil {
			return nil, channelpipe.WrapError(channelpipe.CodeIO, err, name, "cannot write plan")
		}
		manifest.PlanFiles[version] = filepath.ToSlash(name)
		if loss != nil && loss.HasLoss() {
			lossName := filepath.Join("plans", planFileName(version)+".lossy.json")
			if err := writeJSON(filepath.Join(directory, lossName), loss); err != nil {
				return nil, err
			}
			manifest.LossReports[version] = filepath.ToSlash(lossName)
		}
	}

	truthData, err := json.MarshalIndent(set.Truth, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(directory, manifest.TruthFile), truthData, 0o644); err != nil {
		return nil, channelpipe.WrapError(channelpipe.CodeIO, err, manifest.TruthFile, "cannot write truth")
	}
	truthSum := channelpipe.SHA256Hex(truthData)
	if err := os.WriteFile(filepath.Join(directory, manifest.TruthFile+".sha256"), []byte(truthSum+"  truth.json\n"), 0o644); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(directory, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	return &WrittenSample{Directory: directory, Manifest: manifest, V1Data: exports[1], V2Data: exports[2], V3Data: exports[3]}, nil
}

func ReadImage(directory string, header ImageHeader) (*SourceImage, error) {
	headerPath := filepath.Join(directory, "images", header.ID+".chimg.json")
	data, err := os.ReadFile(headerPath)
	if err != nil {
		return nil, channelpipe.WrapError(channelpipe.CodeIO, err, headerPath, "cannot read image header")
	}
	var readHeader ImageHeader
	if err := json.Unmarshal(data, &readHeader); err != nil {
		return nil, channelpipe.WrapError(channelpipe.CodeSchema, err, "$", "invalid image header")
	}
	blob, err := os.ReadFile(filepath.Join(directory, "images", readHeader.DataFile))
	if err != nil {
		return nil, channelpipe.WrapError(channelpipe.CodeIO, err, readHeader.DataFile, "cannot read pixel data")
	}
	if channelpipe.SHA256Hex(blob) != readHeader.DataSHA256 {
		return nil, channelpipe.FieldError(channelpipe.CodeDiff, "data_sha256", "pixel data hash mismatch")
	}
	buffer, err := channelpipe.DecodeBuffer(blob, readHeader.Width, readHeader.Height, len(readHeader.Channels), readHeader.Depth)
	if err != nil {
		return nil, err
	}
	buffer.Channels = readHeader.Channels
	return &SourceImage{ID: readHeader.ID, Buffer: *buffer}, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return channelpipe.WrapError(channelpipe.CodeIO, err, path, "cannot write JSON")
	}
	return nil
}

func planFileName(version int) string {
	switch version {
	case 1:
		return "plan-v1.json"
	case 2:
		return "plan-v2.json"
	default:
		return "plan-v3.json"
	}
}

func pixelEncoding(depth int) string {
	if depth == 8 {
		return "raw-u8-le"
	}
	return "raw-u16le"
}
