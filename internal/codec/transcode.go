package codec

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"

	"mediaforge/internal/domain"
	"mediaforge/internal/errs"
	"mediaforge/internal/mfxfmt"
)

const (
	PathFine     = "fine"
	PathAdaptive = "adaptive"
)

type Input struct {
	ImagePath     string
	AudioPath     string
	ImageChannels int
	AudioChannels int
	ImageMetadata map[string]string
	AudioMetadata map[string]string
}

type Options struct {
	Path                string
	BlockRows           int
	DownsampleFactor    int
	MaxPixelsPerWorker  int64
	MaxSamplesPerWorker int64
}

type Result struct {
	Bytes            []byte
	Header           mfxfmt.Header
	SHA256           string
	MetadataFields   []string
	Path             string
	BlockRows        int
	DownsampleFactor int
	Preview          *domain.PreviewInfo
}

func ResolveImagePath(projectDir, sourcePath string) string {
	if filepath.IsAbs(sourcePath) {
		return sourcePath
	}
	return filepath.Join(projectDir, filepath.FromSlash(sourcePath))
}

func CheckInputs(in Input) error {
	if in.ImagePath == "" {
		return errs.Field(errs.EDecode, "imageSource", "missing image source")
	}
	if _, err := os.Stat(in.ImagePath); err != nil {
		return errs.Wrap(errs.EDecode, "image source is not readable", err)
	}
	if in.AudioPath == "" {
		return errs.Field(errs.EDecode, "audioSource", "missing audio source")
	}
	if _, err := os.Stat(in.AudioPath); err != nil {
		return errs.Wrap(errs.EDecode, "audio source is not readable", err)
	}
	return nil
}

func Transcode(in Input, recipe domain.Recipe, opts Options) (*Result, error) {
	if opts.Path == "" {
		opts.Path = PathFine
	}
	if opts.Path != PathFine && opts.Path != PathAdaptive {
		return nil, errs.Field(errs.EPara, "scale.path", "path must be fine or adaptive")
	}
	img, err := decodeNormalizedImage(in.ImagePath, recipe.ImageChannels)
	if err != nil {
		return nil, errs.Wrap(errs.EDecode, "decode image", err)
	}
	if in.ImageChannels > 0 && img.Channels > in.ImageChannels {
		return nil, errs.Field(errs.EDecode, "imageChannels", "decoded image has more channels than declared")
	}
	wavData, err := os.ReadFile(in.AudioPath)
	if err != nil {
		return nil, errs.Wrap(errs.EDecode, "read audio", err)
	}
	audio, err := DecodeWAV(wavData)
	if err != nil {
		return nil, errs.Wrap(errs.EDecode, "decode WAV", err)
	}
	if in.AudioChannels > 0 && audio.Channels != in.AudioChannels {
		return nil, errs.Field(errs.EDecode, "audioChannels", "decoded WAV channel count differs from manifest")
	}
	metadata := mergeMetadata(in, recipe)
	outSamples := outputSampleCount(audio.Samples, audio.SampleRate, recipe.AudioSampleRate)
	imageBytes := int64(recipe.TargetWidth) * int64(recipe.TargetHeight) * int64(recipe.ImageChannels) * 2
	audioBytes := outSamples * int64(audio.Channels) * 4
	header := mfxfmt.Header{
		Format:           "MFX1",
		Width:            recipe.TargetWidth,
		Height:           recipe.TargetHeight,
		ImageChannels:    recipe.ImageChannels,
		ImageBitDepth:    recipe.ImageBitDepth,
		ChromaSampling:   recipe.ChromaSampling,
		PixelCount:       int64(recipe.TargetWidth) * int64(recipe.TargetHeight),
		ImageSampleBytes: imageBytes,
		AudioSampleRate:  recipe.AudioSampleRate,
		AudioBitDepth:    recipe.AudioBitDepth,
		AudioChannels:    audio.Channels,
		AudioSamples:     outSamples,
		AudioSampleBytes: audioBytes,
		Metadata:         metadata,
		MetadataFields:   mfxfmt.SortedFields(metadata),
		AlgorithmVersion: domain.AlgorithmVersion,
	}
	headerBytes, err := mfxfmt.EncodeHeader(header)
	if err != nil {
		return nil, errs.Wrap(errs.EInternal, "encode package header", err)
	}
	expected := 12 + int64(len(headerBytes)) + imageBytes + audioBytes
	var buf bytes.Buffer
	if expected < 1<<31 {
		buf.Grow(int(expected))
	}
	if err := mfxfmt.WritePrefix(&buf, header); err != nil {
		return nil, errs.Wrap(errs.EInternal, "write package header", err)
	}
	blockRows := recipe.TargetHeight
	if opts.Path == PathAdaptive && opts.BlockRows > 0 && opts.BlockRows < blockRows {
		blockRows = opts.BlockRows
	}
	if opts.MaxPixelsPerWorker > 0 {
		maxRows := int(opts.MaxPixelsPerWorker / int64(max(1, recipe.TargetWidth*recipe.ImageChannels)))
		if maxRows < 1 {
			maxRows = 1
		}
		if maxRows < blockRows {
			blockRows = maxRows
		}
	}
	if recipe.ChromaSampling == "4:2:0" {
		blockRows = (blockRows / 2) * 2
		if blockRows < 2 {
			blockRows = 2
		}
	}
	previewFactor := max(1, opts.DownsampleFactor)
	var previewBuf bytes.Buffer
	previewW := (recipe.TargetWidth + previewFactor - 1) / previewFactor
	previewH := (recipe.TargetHeight + previewFactor - 1) / previewFactor
	for y := 0; y < recipe.TargetHeight; y += blockRows {
		y1 := y + blockRows
		if y1 > recipe.TargetHeight {
			y1 = recipe.TargetHeight
		}
		pix := resizeBlock(img, recipe.TargetWidth, recipe.TargetHeight, y, y1, recipe.ImageChannels)
		applyChromaSampling(pix, recipe.TargetWidth, y1-y, recipe.ImageChannels, recipe.ChromaSampling)
		var tmp [2]byte
		for i, v := range pix {
			px := i / recipe.ImageChannels
			globalX := px % recipe.TargetWidth
			globalY := y + px/recipe.TargetWidth
			q := quantize(v, recipe.ImageBitDepth)
			binary.LittleEndian.PutUint16(tmp[:], q)
			if _, err := buf.Write(tmp[:]); err != nil {
				return nil, errs.Wrap(errs.EInternal, "write image samples", err)
			}
			if opts.Path == PathAdaptive && globalX%previewFactor == 0 && globalY%previewFactor == 0 {
				if _, err := previewBuf.Write(tmp[:]); err != nil {
					return nil, errs.Wrap(errs.EInternal, "write preview samples", err)
				}
			}
		}
	}
	if _, err := writeResampledAudio(&buf, audio, recipe.AudioSampleRate, recipe.AudioBitDepth, 65536); err != nil {
		return nil, errs.Wrap(errs.EInternal, "write audio samples", err)
	}
	if int64(buf.Len()) != expected {
		return nil, errs.Errorf(errs.EInternal, "package size mismatch: got %d want %d", buf.Len(), expected)
	}
	result := buf.Bytes()
	parsed, err := mfxfmt.Parse(result)
	if err != nil {
		return nil, errs.Wrap(errs.EInternal, "self-check encoded package", err)
	}
	resultInfo := &Result{
		Bytes:            result,
		Header:           parsed.Header,
		SHA256:           mfxfmt.SHA256(result),
		MetadataFields:   append([]string(nil), parsed.Header.MetadataFields...),
		Path:             opts.Path,
		BlockRows:        blockRows,
		DownsampleFactor: previewFactor,
	}
	if opts.Path == PathAdaptive {
		resultInfo.Preview = &domain.PreviewInfo{
			Width: previewW, Height: previewH, Downsample: previewFactor,
			SampledPixels: int64(previewW * previewH),
			SHA256:        mfxfmt.SHA256(previewBuf.Bytes()),
		}
	}
	return resultInfo, nil
}

func mergeMetadata(in Input, recipe domain.Recipe) map[string]string {
	switch recipe.MetadataPolicy {
	case "strip":
		return map[string]string{}
	case "normalize":
		want := map[string]bool{}
		for _, k := range recipe.MetadataFields {
			want[k] = true
		}
		out := map[string]string{}
		for k, v := range mergeMetadataRaw(in) {
			if want[k] {
				out[k] = normalizeMetadata(v)
			}
		}
		return out
	default:
		return mergeMetadataRaw(in)
	}
}

func mergeMetadataRaw(in Input) map[string]string {
	out := map[string]string{}
	for k, v := range in.AudioMetadata {
		out[k] = v
	}
	for k, v := range in.ImageMetadata {
		out[k] = v
	}
	return out
}

func normalizeMetadata(s string) string {
	b := []byte(s)
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r' || b[start] == '\n') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r' || b[end-1] == '\n') {
		end--
	}
	return string(b[start:end])
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func CompareProducts(a, b []byte, c domain.Consistency) (*mfxfmt.Package, error) {
	pa, err := mfxfmt.Parse(a)
	if err != nil {
		return nil, errs.Wrap(errs.EVerify, "parse left artifact", err)
	}
	pb, err := mfxfmt.Parse(b)
	if err != nil {
		return nil, errs.Wrap(errs.EVerify, "parse right artifact", err)
	}
	if pa.Header.Width != pb.Header.Width {
		return nil, scaleDiff("width", pa.Header.Width, pb.Header.Width, "")
	}
	if pa.Header.Height != pb.Header.Height {
		return nil, scaleDiff("height", pa.Header.Height, pb.Header.Height, "")
	}
	if pa.Header.ImageChannels != pb.Header.ImageChannels {
		return nil, scaleDiff("channels", pa.Header.ImageChannels, pb.Header.ImageChannels, "")
	}
	if pa.Header.AudioSamples != pb.Header.AudioSamples {
		return nil, scaleDiff("audioSamples", pa.Header.AudioSamples, pb.Header.AudioSamples, "")
	}
	ia, ib := mfxfmt.ImageSamples(a, pa), mfxfmt.ImageSamples(b, pb)
	maxPixelDelta := 0
	for i := 0; i < len(ia) && i < len(ib); i++ {
		d := int(ia[i]) - int(ib[i])
		if d < 0 {
			d = -d
		}
		if d > maxPixelDelta {
			maxPixelDelta = d
		}
	}
	if maxPixelDelta > c.MaxPixelValueDelta {
		return nil, scaleDiff("pixelValueDelta", maxPixelDelta, c.MaxPixelValueDelta, "")
	}
	aa, ab := mfxfmt.AudioSamples(a, pa), mfxfmt.AudioSamples(b, pb)
	maxAudioDelta := 0
	for i := 0; i < len(aa) && i < len(ab); i++ {
		d := int64(aa[i]) - int64(ab[i])
		if d < 0 {
			d = -d
		}
		if int(d) > maxAudioDelta {
			maxAudioDelta = int(d)
		}
	}
	if maxAudioDelta > c.MaxSampleValueDelta {
		return nil, scaleDiff("sampleValueDelta", maxAudioDelta, c.MaxSampleValueDelta, "")
	}
	if c.RequireByteIdentical && !bytes.Equal(a, b) {
		return nil, scaleDiff("fingerprint", mfxfmt.SHA256(a), mfxfmt.SHA256(b), "")
	}
	return &pa, nil
}

func scaleDiff(field string, actual, expected any, variant string) error {
	m := errs.Mismatch{
		Variant: variant, Artifact: "mfx", Field: field, Actual: actual, Expected: expected,
	}
	return &errs.Error{
		Code: errs.EScale, Variant: variant, Mismatch: &m,
		Message: "scale-path consistency exceeds declared tolerance",
	}
}

func MetadataKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
