package sample

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"audioeval/internal/model"
)

type Loaded struct {
	Dir            string
	Manifest       model.Manifest
	Truth          model.Truth
	Original       []int16
	Reconstruction []int16
}

func Load(dir string) (Loaded, error) {
	manifestPath := filepath.Join(dir, "manifest.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return Loaded{}, model.NewError(model.CodeDecode, "manifest", -1, fmt.Sprintf("read manifest: %v", err))
	}
	var rawManifest map[string]json.RawMessage
	if err := json.Unmarshal(manifestData, &rawManifest); err != nil {
		return Loaded{}, model.NewError(model.CodeDecode, "manifest", -1, fmt.Sprintf("decode manifest: %v", err))
	}
	requiredManifest := []string{
		"schema_version", "sample_id", "seed", "sample_rate", "channels",
		"frame_length", "frame_hop", "frame_count", "total_samples_per_channel",
		"quality", "quant_step", "original_file", "reconstruction_file",
		"truth_file", "original_hash", "reconstruction_hash", "truth_fingerprint",
		"original_frame_hashes", "reconstruction_frame_hashes", "input_hash",
	}
	for _, key := range requiredManifest {
		if _, ok := rawManifest[key]; !ok {
			return Loaded{}, model.NewError(model.CodePara, key, -1, "required manifest field is missing")
		}
	}
	var manifest model.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return Loaded{}, model.NewError(model.CodeDecode, "manifest", -1, fmt.Sprintf("decode manifest: %v", err))
	}
	if manifest.SchemaVersion != model.SchemaVersion {
		return Loaded{}, model.NewError(model.CodePara, "schema_version", -1, "unsupported manifest schema version")
	}
	if manifest.SampleID == "" {
		return Loaded{}, model.NewError(model.CodePara, "sample_id", -1, "sample id must not be empty")
	}
	if manifest.SampleRate <= 0 {
		return Loaded{}, model.NewError(model.CodePara, "sample_rate", -1, "sample rate must be a positive integer")
	}
	if manifest.Channels <= 0 {
		return Loaded{}, model.NewError(model.CodePara, "channels", -1, "channel count must be a positive integer")
	}
	if manifest.FrameLength <= 1 {
		return Loaded{}, model.NewError(model.CodePara, "frame_length", -1, "frame length must be greater than one")
	}
	if manifest.FrameHop <= 0 || manifest.FrameHop > manifest.FrameLength {
		return Loaded{}, model.NewError(model.CodePara, "frame_hop", -1, "frame hop must be positive and not greater than frame length")
	}
	if manifest.FrameCount <= 0 {
		return Loaded{}, model.NewError(model.CodePara, "frame_count", -1, "frame count must be a positive integer")
	}
	spec, ok := model.LookupQuality(manifest.Quality)
	if !ok {
		return Loaded{}, model.NewError(model.CodePara, "quality", -1, "unsupported quality level")
	}
	if manifest.QuantStep != spec.Step {
		return Loaded{}, model.NewError(model.CodePara, "quant_step", -1, "quantization step does not match the quality level")
	}
	expectedSamples := (manifest.FrameCount-1)*manifest.FrameHop + manifest.FrameLength
	if manifest.TotalSamplesPerChannel != expectedSamples {
		return Loaded{}, model.NewError(model.CodePara, "total_samples_per_channel", -1, "total sample count does not match frame geometry")
	}
	if len(manifest.OriginalFrameHashes) != manifest.FrameCount {
		return Loaded{}, model.NewError(model.CodePara, "original_frame_hashes", -1, "frame hash count does not match frame count")
	}
	if len(manifest.ReconstructionFrameHashes) != manifest.FrameCount {
		return Loaded{}, model.NewError(model.CodePara, "reconstruction_frame_hashes", -1, "frame hash count does not match frame count")
	}
	if !completeFingerprint(manifest.TruthFingerprint) {
		return Loaded{}, model.NewError(model.CodePara, "truth_fingerprint", -1, "truth fingerprint is missing or incomplete")
	}
	for _, name := range []string{manifest.OriginalFile, manifest.ReconstructionFile, manifest.TruthFile} {
		if !localRelativeName(name) {
			return Loaded{}, model.NewError(model.CodePara, "file", -1, "sequence and truth paths must be local relative names")
		}
	}

	truthPath := filepath.Join(dir, manifest.TruthFile)
	truthData, err := os.ReadFile(truthPath)
	if err != nil {
		return Loaded{}, model.NewError(model.CodeDecode, "truth_file", -1, fmt.Sprintf("read truth: %v", err))
	}
	var rawTruth map[string]json.RawMessage
	if err := json.Unmarshal(truthData, &rawTruth); err != nil {
		return Loaded{}, model.NewError(model.CodeDecode, "truth_file", -1, fmt.Sprintf("decode truth: %v", err))
	}
	for _, key := range []string{"schema_version", "sample_id", "truth_fingerprint", "frames"} {
		if _, ok := rawTruth[key]; !ok {
			return Loaded{}, model.NewError(model.CodePara, key, -1, "required truth field is missing")
		}
	}
	var rawFrames []json.RawMessage
	if err := json.Unmarshal(rawTruth["frames"], &rawFrames); err != nil {
		return Loaded{}, model.NewError(model.CodeDecode, "frames", -1, "truth frames are malformed")
	}
	for i, rawFrame := range rawFrames {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawFrame, &fields); err != nil {
			return Loaded{}, model.NewError(model.CodeDecode, fmt.Sprintf("frames[%d]", i), i, "truth frame is malformed")
		}
		for _, key := range []string{"frame", "pulse_positions", "expected_snr_udb", "noise_floor_std_q16"} {
			if _, ok := fields[key]; !ok {
				return Loaded{}, model.NewError(model.CodePara, fmt.Sprintf("frames[%d].%s", i, key), i, "required truth frame field is missing")
			}
		}
	}
	var truth model.Truth
	if err := json.Unmarshal(truthData, &truth); err != nil {
		return Loaded{}, model.NewError(model.CodeDecode, "truth_file", -1, fmt.Sprintf("decode truth: %v", err))
	}
	if truth.SchemaVersion != model.SchemaVersion {
		return Loaded{}, model.NewError(model.CodePara, "truth.schema_version", -1, "unsupported truth schema version")
	}
	if truth.SampleID != manifest.SampleID {
		return Loaded{}, model.NewError(model.CodePara, "truth.sample_id", -1, "truth sample id does not match manifest")
	}
	if len(truth.Frames) != manifest.FrameCount {
		return Loaded{}, model.NewError(model.CodePara, "truth.frames", -1, "truth frame count does not match manifest")
	}
	if !completeFingerprint(truth.TruthFingerprint) {
		return Loaded{}, model.NewError(model.CodePara, "truth.truth_fingerprint", -1, "truth fingerprint is missing or incomplete")
	}
	recomputedFingerprint, err := model.TruthFingerprint(&truth)
	if err != nil {
		return Loaded{}, model.NewError(model.CodeDecode, "truth.truth_fingerprint", -1, "truth fingerprint cannot be computed")
	}
	if recomputedFingerprint != truth.TruthFingerprint {
		return Loaded{}, model.NewError(model.CodePara, "truth.truth_fingerprint", -1, "truth fingerprint does not match truth contents")
	}
	if manifest.TruthFingerprint != truth.TruthFingerprint {
		return Loaded{}, model.NewError(model.CodePara, "truth_fingerprint", -1, "manifest truth fingerprint does not match truth file")
	}
	for i, frame := range truth.Frames {
		if frame.Frame != i {
			return Loaded{}, model.NewError(model.CodePara, fmt.Sprintf("truth.frames[%d].frame", i), i, "truth frames must be sequential")
		}
		if len(frame.PulsePositions) == 0 {
			return Loaded{}, model.NewError(model.CodePara, fmt.Sprintf("truth.frames[%d].pulse_positions", i), i, "pulse positions must not be empty")
		}
		for _, position := range frame.PulsePositions {
			if position < 0 || position >= manifest.FrameLength {
				return Loaded{}, model.NewError(model.CodePara, fmt.Sprintf("truth.frames[%d].pulse_positions", i), i, "pulse position is outside the frame")
			}
		}
		if frame.NoiseFloorStdQ16 < 0 {
			return Loaded{}, model.NewError(model.CodePara, fmt.Sprintf("truth.frames[%d].noise_floor_std_q16", i), i, "noise-floor standard deviation must not be negative")
		}
	}

	expectedBytes := manifest.TotalSamplesPerChannel * manifest.Channels * 2
	original, err := readPCM(filepath.Join(dir, manifest.OriginalFile), expectedBytes, manifest.Channels, manifest.FrameLength, manifest.FrameCount, "original_file")
	if err != nil {
		return Loaded{}, err
	}
	reconstruction, err := readPCM(filepath.Join(dir, manifest.ReconstructionFile), expectedBytes, manifest.Channels, manifest.FrameLength, manifest.FrameCount, "reconstruction_file")
	if err != nil {
		return Loaded{}, err
	}
	for frame := 0; frame < manifest.FrameCount; frame++ {
		start := frame * manifest.FrameHop * manifest.Channels
		end := start + manifest.FrameLength*manifest.Channels
		if hashBytes(pcmBytes(original[start:end])) != manifest.OriginalFrameHashes[frame] {
			return Loaded{}, model.NewError(model.CodeDecode, "original_frame_hash", frame, "original frame data is damaged").WithFrame(frame)
		}
		if hashBytes(pcmBytes(reconstruction[start:end])) != manifest.ReconstructionFrameHashes[frame] {
			return Loaded{}, model.NewError(model.CodeDecode, "reconstruction_frame_hash", frame, "reconstruction frame data is damaged").WithFrame(frame)
		}
	}
	if hashBytes(pcmBytes(original)) != manifest.OriginalHash {
		return Loaded{}, model.NewError(model.CodeDecode, "original_hash", -1, "original sequence hash mismatch").WithFrame(0)
	}
	if hashBytes(pcmBytes(reconstruction)) != manifest.ReconstructionHash {
		return Loaded{}, model.NewError(model.CodeDecode, "reconstruction_hash", -1, "reconstruction sequence hash mismatch").WithFrame(0)
	}
	inputHash, err := model.ComputeInputHash(&manifest, truth.TruthFingerprint)
	if err != nil {
		return Loaded{}, model.NewError(model.CodeDecode, "input_hash", -1, "input hash cannot be computed")
	}
	if inputHash != manifest.InputHash {
		return Loaded{}, model.NewError(model.CodeDecode, "input_hash", -1, "input hash does not match manifest")
	}

	return Loaded{
		Dir:            dir,
		Manifest:       manifest,
		Truth:          truth,
		Original:       original,
		Reconstruction: reconstruction,
	}, nil
}

func readPCM(path string, expectedBytes, channels, frameLength, frameCount int, field string) ([]int16, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.NewError(model.CodeDecode, field, -1, fmt.Sprintf("read sequence: %v", err))
	}
	if len(data) != expectedBytes {
		frameStride := channels * frameLength * 2
		badFrame := len(data) / frameStride
		if badFrame >= frameCount {
			badFrame = frameCount - 1
		}
		return nil, model.NewError(model.CodeDecode, field, badFrame, "sequence byte length does not match manifest").WithFrame(badFrame)
	}
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return samples, nil
}

func completeFingerprint(fingerprint string) bool {
	if len(fingerprint) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(fingerprint)
	return err == nil
}

func localRelativeName(name string) bool {
	if name == "" || filepath.IsAbs(name) {
		return false
	}
	clean := filepath.Clean(name)
	return clean == name && clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
