package samplegen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"

	"mediaforge/internal/codec"
	"mediaforge/internal/domain"
	"mediaforge/internal/errs"
	"mediaforge/internal/project"
)

type Options struct {
	OutDir  string
	Seed    int64
	Profile string
}

type projectSpec struct {
	ID        string
	Name      string
	Width     int
	Height    int
	TargetW   int
	TargetH   int
	AudioRate int
	AudioSecs float64
	AltScale  int
}

type Index struct {
	Seed     int64          `json:"seed"`
	Profile  string         `json:"profile"`
	Projects []IndexProject `json:"projects"`
}

type IndexProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

func Profiles() map[string][]projectSpec {
	return map[string][]projectSpec{
		"small": {
			{ID: "arcade-hud", Name: "Arcade HUD", Width: 512, Height: 320, TargetW: 384, TargetH: 240, AudioRate: 44100, AudioSecs: 0.75, AltScale: 2},
		},
		"demo": {
			{ID: "arcade-hud", Name: "Arcade HUD", Width: 512, Height: 320, TargetW: 384, TargetH: 240, AudioRate: 44100, AudioSecs: 0.75, AltScale: 2},
			{ID: "boss-arena", Name: "Boss Arena", Width: 1280, Height: 720, TargetW: 960, TargetH: 540, AudioRate: 48000, AudioSecs: 1.5, AltScale: 2},
			{ID: "open-world", Name: "Open World Preview", Width: 2048, Height: 1280, TargetW: 1024, TargetH: 640, AudioRate: 48000, AudioSecs: 2.5, AltScale: 2},
		},
		"large": {
			{ID: "open-world-large", Name: "Open World Large", Width: 8192, Height: 5120, TargetW: 2048, TargetH: 1280, AudioRate: 48000, AudioSecs: 4, AltScale: 2},
		},
	}
}

func Generate(opts Options) (*Index, error) {
	if opts.OutDir == "" {
		return nil, errs.Field(errs.EPara, "out", "output directory is required")
	}
	if opts.Profile == "" {
		opts.Profile = "demo"
	}
	specs, ok := Profiles()[opts.Profile]
	if !ok {
		return nil, errs.Field(errs.EPara, "profile", "must be small, demo or large")
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, errs.Wrap(errs.EIO, "create sample directory", err)
	}
	index := &Index{Seed: opts.Seed, Profile: opts.Profile}
	for i, spec := range specs {
		dir := filepath.Join(opts.OutDir, spec.ID)
		if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
			return nil, errs.Wrap(errs.EIO, "create project directory", err)
		}
		p, err := generateProject(dir, spec, opts.Seed+int64(i*100003))
		if err != nil {
			return nil, err
		}
		data, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return nil, errs.Wrap(errs.EInternal, "encode generated project", err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(filepath.Join(dir, "project.json"), data, 0o644); err != nil {
			return nil, errs.Wrap(errs.EIO, "write generated project", err)
		}
		loaded, err := project.Load(filepath.Join(dir, "project.json"))
		if err != nil {
			return nil, err
		}
		if _, err := project.Validate(loaded, dir); err != nil {
			return nil, errs.Wrap(errs.EInternal, "generated project failed validation", err)
		}
		index.Projects = append(index.Projects, IndexProject{
			ID: spec.ID, Name: spec.Name, Path: spec.ID + "/project.json",
		})
	}
	data, _ := json.MarshalIndent(index, "", "  ")
	if err := os.WriteFile(filepath.Join(opts.OutDir, "index.json"), append(data, '\n'), 0o644); err != nil {
		return nil, errs.Wrap(errs.EIO, "write sample index", err)
	}
	return index, nil
}

func generateProject(dir string, spec projectSpec, seed int64) (*domain.Project, error) {
	imgBase := filepath.Join(dir, "assets", "image_base.png")
	imgAlt := filepath.Join(dir, "assets", "image_alt.png")
	altW := max(128, spec.Width/spec.AltScale)
	altH := max(128, spec.Height/spec.AltScale)
	if err := generateImage(imgBase, spec.Width, spec.Height, seed^0x1111); err != nil {
		return nil, err
	}
	if err := generateImage(imgAlt, altW, altH, seed^0x2222); err != nil {
		return nil, err
	}
	audioBase := filepath.Join(dir, "assets", "audio_base.wav")
	audioAlt := filepath.Join(dir, "assets", "audio_alt.wav")
	baseSamples, err := generateAudio(audioBase, spec.AudioRate, 2, spec.AudioSecs, 220, seed^0x3333)
	if err != nil {
		return nil, err
	}
	altSamples, err := generateAudio(audioAlt, spec.AudioRate, 2, spec.AudioSecs*0.75+0.05, 330, seed^0x4444)
	if err != nil {
		return nil, err
	}
	hashBase, err := hashFile(imgBase)
	if err != nil {
		return nil, err
	}
	hashAlt, err := hashFile(imgAlt)
	if err != nil {
		return nil, err
	}
	hashAudioBase, err := hashFile(audioBase)
	if err != nil {
		return nil, err
	}
	hashAudioAlt, err := hashFile(audioAlt)
	if err != nil {
		return nil, err
	}
	imgMetaBase := map[string]string{
		"title": "Arcade hero plate", "author": "Deterministic Sample Foundry",
		"copyright": "CC0 synthetic fixture", "image.role": "base",
	}
	imgMetaAlt := map[string]string{
		"title": "Arcade alternate plate", "author": "Deterministic Sample Foundry",
		"image.role": "alternate",
	}
	audioMetaBase := map[string]string{
		"audio.title": "UI pulse", "audio.artist": "Fixed Phase", "license": "CC0",
	}
	audioMetaAlt := map[string]string{
		"audio.title": "Impact tail", "audio.artist": "Fixed Phase", "license": "CC0",
	}
	sources := []domain.Source{
		{ID: "image-base", Kind: "image", Path: "assets/image_base.png", SHA256: hashBase,
			Width: spec.Width, Height: spec.Height, Channels: 4, Metadata: imgMetaBase},
		{ID: "image-alt", Kind: "image", Path: "assets/image_alt.png", SHA256: hashAlt,
			Width: altW, Height: altH, Channels: 4, Metadata: imgMetaAlt},
		{ID: "audio-base", Kind: "audio", Path: "assets/audio_base.wav", SHA256: hashAudioBase,
			SampleRate: spec.AudioRate, AudioChannels: 2, AudioBitDepth: 16,
			SamplesPerChannel: baseSamples, Metadata: audioMetaBase},
		{ID: "audio-alt", Kind: "audio", Path: "assets/audio_alt.wav", SHA256: hashAudioAlt,
			SampleRate: spec.AudioRate, AudioChannels: 2, AudioBitDepth: 16,
			SamplesPerChannel: altSamples, Metadata: audioMetaAlt},
	}
	baseRecipe := domain.Recipe{
		TargetWidth: spec.TargetW, TargetHeight: spec.TargetH, ImageChannels: 4,
		ImageBitDepth: 8, ChromaSampling: "4:2:0", AudioSampleRate: 48000,
		AudioBitDepth: 16, MetadataPolicy: "preserve",
	}
	p := &domain.Project{
		SchemaVersion: domain.SchemaVersion, ID: spec.ID, Name: spec.Name,
		Sources: sources, BaseRecipe: baseRecipe,
		Variants: []domain.VariantSeed{
			{ID: "v-main", ImageSource: "image-base", AudioSource: "audio-base"},
			{ID: "v-alt", ImageSource: "image-base", AudioSource: "audio-alt"},
			{ID: "v-draft", ImageSource: "image-alt", AudioSource: "audio-base"},
		},
		Budget: domain.Budget{
			MaxWorkers: 2, MemoryBytes: 256 << 20, PerVariantMillis: 8000,
			MaxPixelsPerWorker: 4_000_000, MaxSamplesPerWorker: 4_000_000,
			FineMaxPixels: 4_000_000, FineMaxSamples: 8_000_000, FineMaxVariants: 4,
			AdaptiveBlockPixels: 2_000_000, MaxDownsample: 8,
		},
		Consistency: domain.Consistency{
			MaxPixelValueDelta: 0, MaxSampleValueDelta: 0, RequireByteIdentical: true,
		},
	}
	t1, t2, t3, t4, t5, t6, t7 := int64(100), int64(200), int64(300), int64(400), int64(500), int64(600), int64(700)
	p.Operations = []domain.Operation{
		{Seq: 1, Time: t1, Type: "replace_source", Variant: "v-alt", Media: "image", Source: "image-alt"},
		{Seq: 2, Time: t2, Type: "set_recipe", Field: "imageBitDepth", Value: raw(10)},
		{Seq: 3, Time: t3, Type: "add_variant", NewVariant: &domain.VariantSeed{
			ID: "v-preview", ImageSource: "image-base", AudioSource: "audio-base",
		}},
		{Seq: 4, Time: t4, Type: "set_recipe", Variant: "v-preview", Field: "targetWidth", Value: raw(max(16, spec.TargetW/2))},
		{Seq: 5, Time: t5, Type: "set_recipe", Variant: "v-preview", Field: "targetHeight", Value: raw(max(16, spec.TargetH/2))},
		{Seq: 6, Time: t6, Type: "set_recipe", Variant: "v-preview", Field: "chromaSampling", Value: raw("4:2:2")},
		{Seq: 7, Time: t7, Type: "remove_variant", Variant: "v-draft"},
	}
	rp, err := project.ResolveDraft(p, dir)
	if err != nil {
		return nil, errs.Wrap(errs.EInternal, "resolve generated project", err)
	}
	truth := domain.TruthManifest{}
	for _, v := range rp.Variants {
		img := rp.Sources[v.ImageSourceID]
		aud := rp.Sources[v.AudioSourceID]
		result, err := codec.Transcode(codec.Input{
			ImagePath:     filepath.Join(dir, filepath.FromSlash(img.Path)),
			AudioPath:     filepath.Join(dir, filepath.FromSlash(aud.Path)),
			ImageChannels: img.Channels, AudioChannels: aud.AudioChannels,
			ImageMetadata: img.Metadata, AudioMetadata: aud.Metadata,
		}, v.Recipe, codec.Options{Path: codec.PathFine})
		if err != nil {
			return nil, errs.Wrap(errs.EInternal, "generate truth artifact", err)
		}
		truth.Variants = append(truth.Variants, domain.TruthVariant{
			ID: v.ID, Width: result.Header.Width, Height: result.Header.Height,
			Channels: result.Header.ImageChannels, PixelCount: result.Header.PixelCount,
			AudioSamples:   result.Header.AudioSamples,
			MetadataFields: append([]string(nil), result.MetadataFields...),
			Fingerprint:    result.SHA256, SizeBytes: int64(len(result.Bytes)),
		})
	}
	p.Truth = truth
	return p, nil
}

func generateImage(path string, w, h int, seed int64) error {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*img.Stride + x*4
			h1 := mix(uint64(seed), uint64(x), uint64(y), 0x9e3779b97f4a7c15)
			h2 := mix(uint64(seed)^0xa5a5a5a5, uint64(x/64), uint64(y/64), 0xbf58476d1ce4e5b9)
			checker := byte(0)
			if ((x/64)+(y/64))%2 == 0 {
				checker = 48
			}
			img.Pix[i] = byte(h1) ^ checker
			img.Pix[i+1] = byte(h1>>8) ^ byte(h2)>>1
			img.Pix[i+2] = byte(h1>>16) ^ byte(h2>>8)
			img.Pix[i+3] = 224 + byte(h1>>32)%32
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return errs.Wrap(errs.EIO, "create image source", err)
	}
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	err = enc.Encode(f, img)
	closeErr := f.Close()
	if err != nil {
		return errs.Wrap(errs.EIO, "encode image source", err)
	}
	if closeErr != nil {
		return errs.Wrap(errs.EIO, "close image source", closeErr)
	}
	return nil
}

func generateAudio(path string, rate, channels int, seconds float64, freq float64, seed int64) (int64, error) {
	n := int(seconds * float64(rate))
	if n < 1 {
		n = 1
	}
	samples := make([]int16, n*channels)
	for i := 0; i < n; i++ {
		t := float64(i) / float64(rate)
		w := math.Sin(2*math.Pi*freq*t) * 0.28
		w += math.Sin(2*math.Pi*(freq*1.5)*t) * 0.07
		env := math.Min(1, math.Min(
			float64(i)/(float64(rate)*0.02+1),
			float64(n-i)/(float64(rate)*0.05+1)))
		for ch := 0; ch < channels; ch++ {
			phase := 0.0
			if ch == 1 {
				phase = 0.11
			}
			v := w*env + phase*math.Sin(2*math.Pi*freq*0.5*t)
			if v > 0.98 {
				v = 0.98
			}
			if v < -0.98 {
				v = -0.98
			}
			samples[i*channels+ch] = int16(v * 32767)
		}
	}
	data, err := codec.EncodeWAVPCM16(rate, channels, samples)
	if err != nil {
		return 0, errs.Wrap(errs.EInternal, "encode WAV source", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return 0, errs.Wrap(errs.EIO, "write WAV source", err)
	}
	return int64(n), nil
}

func raw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errs.Wrap(errs.EIO, "read generated asset", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func mix(seed, x, y, mul uint64) uint64 {
	z := seed + x*0x9e3779b97f4a7c15 + y*0xc2b2ae3d27d4eb4f + mul
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func FindProjects(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, errs.Wrap(errs.EIO, "inspect sample path", err)
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	var out []string
	err = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && strings.EqualFold(d.Name(), "project.json") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, errs.Wrap(errs.EIO, "scan samples", err)
	}
	return out, nil
}

func EnsureDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	return nil
}
