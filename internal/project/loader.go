package project

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediaforge/internal/codec"
	"mediaforge/internal/domain"
	"mediaforge/internal/errs"
	"mediaforge/internal/mfxfmt"
)

func Load(path string) (*domain.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errs.Wrap(errs.EDecode, "read project manifest", err)
	}
	var p domain.Project
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, errs.Wrap(errs.EDecode, "decode project manifest", err)
	}
	return &p, nil
}

func Validate(p *domain.Project, projectDir string) (*domain.ResolvedProject, error) {
	return validateProject(p, projectDir, true)
}

func ResolveDraft(p *domain.Project, projectDir string) (*domain.ResolvedProject, error) {
	return validateProject(p, projectDir, false)
}

func validateProject(p *domain.Project, projectDir string, requireTruth bool) (*domain.ResolvedProject, error) {
	if p == nil {
		return nil, errs.New(errs.EDecode, "project is nil")
	}
	if p.SchemaVersion != domain.SchemaVersion {
		return nil, errs.Field(errs.EPara, "schemaVersion",
			fmt.Sprintf("unsupported schema version %d", p.SchemaVersion))
	}
	if strings.TrimSpace(p.ID) == "" {
		return nil, errs.Field(errs.EDecode, "id", "project ID is required")
	}
	if len(p.Sources) == 0 {
		return nil, errs.Field(errs.EDecode, "sources", "at least one source is required")
	}
	if len(p.Variants) == 0 {
		return nil, errs.Field(errs.EDecode, "variants", "at least one variant is required")
	}
	if err := ValidateRecipe(p.BaseRecipe, "baseRecipe"); err != nil {
		return nil, err
	}
	if err := validateBudget(p.Budget); err != nil {
		return nil, err
	}
	sources := map[string]domain.Source{}
	for i := range p.Sources {
		s := p.Sources[i]
		prefix := fmt.Sprintf("sources[%d]", i)
		if s.ID == "" {
			return nil, errs.Field(errs.EDecode, prefix+".id", "source ID is required")
		}
		if _, exists := sources[s.ID]; exists {
			return nil, errs.Field(errs.EDecode, prefix+".id", "duplicate source ID")
		}
		if err := validateSource(&s, projectDir, prefix); err != nil {
			return nil, err
		}
		sources[s.ID] = s
	}
	for i, v := range p.Variants {
		if err := validateSeed(v, sources, fmt.Sprintf("variants[%d]", i)); err != nil {
			return nil, err
		}
	}
	variants, err := replay(p, sources)
	if err != nil {
		return nil, err
	}
	if len(variants) == 0 {
		return nil, errs.New(errs.EDecode, "no variants remain after applying operations")
	}
	seen := map[string]bool{}
	for _, v := range variants {
		if seen[v.ID] {
			return nil, errs.Field(errs.EDecode, "operations", "variant IDs are not unique after replay")
		}
		seen[v.ID] = true
		img := sources[v.ImageSourceID]
		if img.Kind != "image" {
			return nil, errs.Field(errs.EDecode, "variant."+v.ID+".imageSource", "source is not an image")
		}
		if v.Recipe.ImageChannels > img.Channels {
			return nil, errs.Field(errs.EPara, v.ID+".imageChannels",
				"output channel count is incompatible with source image")
		}
	}
	if requireTruth {
		if err := validateTruth(p.Truth, variants); err != nil {
			return nil, err
		}
	}
	for i := range variants {
		v := &variants[i]
		v.Recipe = v.Recipe.Clone()
		v.RecipeVersion = RecipeVersion(v.Recipe)
		v.InputHash = VariantInputHash(*v, sources)
	}
	return &domain.ResolvedProject{Project: p, Variants: variants, Sources: sources}, nil
}

func ValidateRecipe(r domain.Recipe, prefix string) error {
	if r.TargetWidth == 0 {
		return errs.Field(errs.EDecode, prefix+".targetWidth", "field is required")
	}
	if r.TargetWidth < 16 || r.TargetWidth > 32768 {
		return errs.Field(errs.EPara, prefix+".targetWidth", "target width must be 16..32768")
	}
	if r.TargetHeight == 0 {
		return errs.Field(errs.EDecode, prefix+".targetHeight", "field is required")
	}
	if r.TargetHeight < 16 || r.TargetHeight > 32768 {
		return errs.Field(errs.EPara, prefix+".targetHeight", "target height must be 16..32768")
	}
	if r.ImageChannels == 0 {
		return errs.Field(errs.EDecode, prefix+".imageChannels", "field is required")
	}
	if r.ImageChannels != 3 && r.ImageChannels != 4 {
		return errs.Field(errs.EPara, prefix+".imageChannels", "must be 3 or 4")
	}
	if r.ImageBitDepth == 0 {
		return errs.Field(errs.EDecode, prefix+".imageBitDepth", "field is required")
	}
	if r.ImageBitDepth != 8 && r.ImageBitDepth != 10 && r.ImageBitDepth != 16 {
		return errs.Field(errs.EPara, prefix+".imageBitDepth", "must be 8, 10 or 16")
	}
	if r.ChromaSampling == "" {
		return errs.Field(errs.EDecode, prefix+".chromaSampling", "field is required")
	}
	if r.ChromaSampling != "4:4:4" && r.ChromaSampling != "4:2:2" && r.ChromaSampling != "4:2:0" {
		return errs.Field(errs.EPara, prefix+".chromaSampling", "must be 4:4:4, 4:2:2 or 4:2:0")
	}
	if r.AudioSampleRate == 0 {
		return errs.Field(errs.EDecode, prefix+".audioSampleRate", "field is required")
	}
	if r.AudioSampleRate < 8000 || r.AudioSampleRate > 192000 {
		return errs.Field(errs.EPara, prefix+".audioSampleRate", "must be 8000..192000")
	}
	if r.AudioBitDepth == 0 {
		return errs.Field(errs.EDecode, prefix+".audioBitDepth", "field is required")
	}
	if r.AudioBitDepth != 16 && r.AudioBitDepth != 24 {
		return errs.Field(errs.EPara, prefix+".audioBitDepth", "must be 16 or 24")
	}
	if r.MetadataPolicy == "" {
		return errs.Field(errs.EDecode, prefix+".metadataPolicy", "field is required")
	}
	if r.MetadataPolicy != "preserve" && r.MetadataPolicy != "strip" && r.MetadataPolicy != "normalize" {
		return errs.Field(errs.EPara, prefix+".metadataPolicy", "must be preserve, strip or normalize")
	}
	if r.MetadataPolicy == "normalize" && len(r.MetadataFields) == 0 {
		return errs.Field(errs.EPara, prefix+".metadataFields", "normalize policy requires a field list")
	}
	for i, f := range r.MetadataFields {
		if strings.TrimSpace(f) == "" {
			return errs.Field(errs.EPara, fmt.Sprintf("%s.metadataFields[%d]", prefix, i), "field name is empty")
		}
	}
	return nil
}

func validateBudget(b domain.Budget) error {
	if b.MaxWorkers == 0 {
		return errs.Field(errs.EDecode, "budget.maxWorkers", "field is required")
	}
	if b.MaxWorkers < 1 {
		return errs.Field(errs.EPara, "budget.maxWorkers", "must be at least 1")
	}
	if b.MemoryBytes == 0 {
		return errs.Field(errs.EDecode, "budget.memoryBytes", "field is required")
	}
	if b.MemoryBytes < 1<<20 {
		return errs.Field(errs.EPara, "budget.memoryBytes", "must be at least 1 MiB")
	}
	if b.PerVariantMillis == 0 {
		return errs.Field(errs.EDecode, "budget.perVariantMillis", "field is required")
	}
	if b.PerVariantMillis < 1 {
		return errs.Field(errs.EPara, "budget.perVariantMillis", "must be positive")
	}
	if b.MaxPixelsPerWorker == 0 {
		return errs.Field(errs.EDecode, "budget.maxPixelsPerWorker", "field is required")
	}
	if b.MaxPixelsPerWorker < 1 {
		return errs.Field(errs.EPara, "budget.maxPixelsPerWorker", "must be positive")
	}
	if b.MaxSamplesPerWorker == 0 {
		return errs.Field(errs.EDecode, "budget.maxSamplesPerWorker", "field is required")
	}
	if b.MaxSamplesPerWorker < 1 {
		return errs.Field(errs.EPara, "budget.maxSamplesPerWorker", "must be positive")
	}
	if b.FineMaxPixels == 0 || b.FineMaxSamples == 0 || b.FineMaxVariants == 0 {
		return errs.Field(errs.EDecode, "budget.fineMax*", "field is required")
	}
	if b.FineMaxPixels < 1 || b.FineMaxSamples < 1 || b.FineMaxVariants < 1 {
		return errs.Field(errs.EPara, "budget.fineMax*", "fine-path thresholds must be positive")
	}
	if b.AdaptiveBlockPixels == 0 {
		return errs.Field(errs.EDecode, "budget.adaptiveBlockPixels", "field is required")
	}
	if b.AdaptiveBlockPixels < 1 {
		return errs.Field(errs.EPara, "budget.adaptiveBlockPixels", "must be positive")
	}
	if b.MaxDownsample == 0 {
		return errs.Field(errs.EDecode, "budget.maxDownsample", "field is required")
	}
	if b.MaxDownsample < 1 {
		return errs.Field(errs.EPara, "budget.maxDownsample", "must be positive")
	}
	return nil
}

func validateSource(s *domain.Source, projectDir, prefix string) error {
	if s.Kind != "image" && s.Kind != "audio" {
		return errs.Field(errs.EPara, prefix+".kind", "must be image or audio")
	}
	if strings.TrimSpace(s.Path) == "" {
		return errs.Field(errs.EDecode, prefix+".path", "source path is required")
	}
	if len(s.SHA256) != 64 {
		return errs.Field(errs.EDecode, prefix+".sha256", "a SHA-256 digest is required")
	}
	if _, err := hex.DecodeString(s.SHA256); err != nil {
		return errs.Field(errs.EDecode, prefix+".sha256", "invalid SHA-256 digest")
	}
	path := s.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(projectDir, filepath.FromSlash(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return errs.Wrap(errs.EDecode, "source is not readable: "+s.ID, err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return errs.Wrap(errs.EDecode, "read source: "+s.ID, err)
	}
	_ = f.Close()
	actualHash := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actualHash, s.SHA256) {
		return &errs.Error{
			Code: errs.EDecode, Message: "source content fingerprint mismatch", Field: prefix + ".sha256",
			Detail: fmt.Sprintf("actual=%s declared=%s source=%s", actualHash, s.SHA256, s.ID),
		}
	}
	switch s.Kind {
	case "image":
		if err := validateImageSource(s, path, prefix); err != nil {
			return err
		}
	case "audio":
		if err := validateAudioSource(s, path, prefix); err != nil {
			return err
		}
	}
	return nil
}

func validateImageSource(s *domain.Source, path, prefix string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return errs.Wrap(errs.EDecode, "read image header", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return errs.Wrap(errs.EDecode, "open image", err)
	}
	cfg, format, err := image.DecodeConfig(f)
	_ = f.Close()
	if err != nil {
		return errs.Wrap(errs.EDecode, "decode image header", err)
	}
	if format != "png" && format != "jpeg" {
		return errs.Field(errs.EDecode, prefix+".path", "only PNG and JPEG images are supported")
	}
	if s.Width <= 0 || s.Height <= 0 {
		return errs.Field(errs.EDecode, prefix+".width", "image dimensions are required")
	}
	if cfg.Width != s.Width || cfg.Height != s.Height {
		return &errs.Error{
			Code: errs.EDecode, Message: "image dimensions differ from manifest", Field: prefix + ".width",
			Detail: fmt.Sprintf("actual=%dx%d declared=%dx%d", cfg.Width, cfg.Height, s.Width, s.Height),
		}
	}
	if s.Channels != 3 && s.Channels != 4 {
		return errs.Field(errs.EPara, prefix+".channels", "must be 3 or 4")
	}
	if format == "png" {
		actualChannels := pngChannels(raw)
		if actualChannels != 0 && actualChannels != s.Channels {
			return &errs.Error{
				Code: errs.EDecode, Message: "image channel count differs from manifest",
				Field:  prefix + ".channels",
				Detail: fmt.Sprintf("actual=%d declared=%d", actualChannels, s.Channels),
			}
		}
	}
	return nil
}

func pngChannels(data []byte) int {
	if len(data) < 26 || !bytes.Equal(data[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return 0
	}
	if binary.BigEndian.Uint32(data[8:12]) != 13 || string(data[12:16]) != "IHDR" {
		return 0
	}
	switch data[25] {
	case 0:
		return 1
	case 2:
		return 3
	case 3:
		return 4
	case 4:
		return 2
	case 6:
		return 4
	default:
		return 0
	}
}

func validateAudioSource(s *domain.Source, path, prefix string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return errs.Wrap(errs.EDecode, "read WAV", err)
	}
	audio, err := codec.DecodeWAV(data)
	if err != nil {
		return errs.Wrap(errs.EDecode, "decode WAV header", err)
	}
	if s.SampleRate <= 0 || s.AudioChannels <= 0 || s.AudioBitDepth <= 0 || s.SamplesPerChannel <= 0 {
		return errs.Field(errs.EDecode, prefix+".audio", "audio parameters are required")
	}
	if audio.SampleRate != s.SampleRate || audio.Channels != s.AudioChannels ||
		audio.BitDepth != s.AudioBitDepth || audio.Samples != s.SamplesPerChannel {
		return &errs.Error{
			Code: errs.EDecode, Message: "WAV parameters differ from manifest", Field: prefix + ".audio",
			Detail: fmt.Sprintf("actual=%dHz/%dch/%dbit/%dsamples declared=%dHz/%dch/%dbit/%dsamples",
				audio.SampleRate, audio.Channels, audio.BitDepth, audio.Samples,
				s.SampleRate, s.AudioChannels, s.AudioBitDepth, s.SamplesPerChannel),
		}
	}
	return nil
}

func validateSeed(v domain.VariantSeed, sources map[string]domain.Source, prefix string) error {
	if strings.TrimSpace(v.ID) == "" {
		return errs.Field(errs.EDecode, prefix+".id", "variant ID is required")
	}
	if v.ImageSource == "" {
		return errs.Field(errs.EDecode, prefix+".imageSource", "image source is required")
	}
	if v.AudioSource == "" {
		return errs.Field(errs.EDecode, prefix+".audioSource", "audio source is required")
	}
	if s, ok := sources[v.ImageSource]; !ok || s.Kind != "image" {
		return errs.Field(errs.EDecode, prefix+".imageSource", "image source does not exist")
	}
	if s, ok := sources[v.AudioSource]; !ok || s.Kind != "audio" {
		return errs.Field(errs.EDecode, prefix+".audioSource", "audio source does not exist")
	}
	return nil
}

type replayVariant struct {
	seed   domain.VariantSeed
	recipe domain.Recipe
}

func replay(p *domain.Project, sources map[string]domain.Source) ([]domain.ResolvedVariant, error) {
	variants := make([]replayVariant, 0, len(p.Variants))
	index := map[string]int{}
	for _, v := range p.Variants {
		if _, exists := index[v.ID]; exists {
			return nil, errs.Field(errs.EDecode, "variants", "duplicate variant ID "+v.ID)
		}
		index[v.ID] = len(variants)
		variants = append(variants, replayVariant{seed: v, recipe: p.BaseRecipe.Clone()})
	}
	projectRecipe := p.BaseRecipe.Clone()
	var lastTime int64 = -1
	lastSeq := int64(-1)
	for i, op := range p.Operations {
		prefix := fmt.Sprintf("operations[%d]", i)
		if op.Seq == 0 {
			return nil, errs.Field(errs.EDecode, prefix+".seq", "operation sequence is required")
		}
		if op.Time < 0 {
			return nil, errs.Field(errs.EDecode, prefix+".time", "logical timestamp must be non-negative")
		}
		if op.Seq <= lastSeq {
			return nil, errs.Field(errs.EDecode, prefix+".seq", "operation sequence must increase")
		}
		if op.Time <= lastTime {
			return nil, errs.Field(errs.EDecode, prefix+".time", "logical timestamps must be strictly increasing")
		}
		if op.Type == "" {
			return nil, errs.Field(errs.EDecode, prefix+".type", "operation type is required")
		}
		lastSeq, lastTime = op.Seq, op.Time
		switch op.Type {
		case "replace_source":
			idx, ok := index[op.Variant]
			if !ok {
				return nil, errs.Field(errs.EDecode, prefix+".variant", "target variant does not exist")
			}
			if op.Media != "image" && op.Media != "audio" {
				return nil, errs.Field(errs.EPara, prefix+".media", "must be image or audio")
			}
			src, ok := sources[op.Source]
			if !ok {
				return nil, errs.Field(errs.EDecode, prefix+".source", "source does not exist")
			}
			if src.Kind != op.Media {
				return nil, errs.Field(errs.EPara, prefix+".source", "source kind does not match media")
			}
			if op.Media == "image" {
				variants[idx].seed.ImageSource = op.Source
			} else {
				variants[idx].seed.AudioSource = op.Source
			}
		case "set_recipe":
			if domain.NormalizeRecipeField(op.Field) == "" {
				return nil, errs.Field(errs.EPara, prefix+".field", "unknown recipe field")
			}
			if len(op.Value) == 0 {
				return nil, errs.Field(errs.EDecode, prefix+".value", "recipe value is required")
			}
			if op.Variant != "" {
				idx, ok := index[op.Variant]
				if !ok {
					return nil, errs.Field(errs.EDecode, prefix+".variant", "target variant does not exist")
				}
				if err := domain.ApplyRecipeField(&variants[idx].recipe, op.Field, op.Value); err != nil {
					return nil, errs.Field(errs.EPara, prefix+".value", "invalid recipe value: "+err.Error())
				}
				if err := ValidateRecipe(variants[idx].recipe, prefix+".value"); err != nil {
					return nil, err
				}
			} else {
				next := projectRecipe.Clone()
				if err := domain.ApplyRecipeField(&next, op.Field, op.Value); err != nil {
					return nil, errs.Field(errs.EPara, prefix+".value", "invalid recipe value: "+err.Error())
				}
				if err := ValidateRecipe(next, prefix+".value"); err != nil {
					return nil, err
				}
				projectRecipe = next
				for j := range variants {
					variants[j].recipe = next.Clone()
				}
			}
		case "add_variant":
			if op.NewVariant == nil {
				return nil, errs.Field(errs.EDecode, prefix+".newVariant", "new variant is required")
			}
			if _, exists := index[op.NewVariant.ID]; exists {
				return nil, errs.Field(errs.EDecode, prefix+".newVariant.id", "variant already exists")
			}
			if err := validateSeed(*op.NewVariant, sources, prefix+".newVariant"); err != nil {
				return nil, err
			}
			index[op.NewVariant.ID] = len(variants)
			variants = append(variants, replayVariant{seed: *op.NewVariant, recipe: projectRecipe.Clone()})
		case "remove_variant":
			idx, ok := index[op.Variant]
			if !ok {
				return nil, errs.Field(errs.EDecode, prefix+".variant", "target variant does not exist")
			}
			variants = append(variants[:idx], variants[idx+1:]...)
			index = map[string]int{}
			for j, v := range variants {
				index[v.seed.ID] = j
			}
		default:
			return nil, errs.Field(errs.EPara, prefix+".type", "unknown operation type")
		}
	}
	out := make([]domain.ResolvedVariant, len(variants))
	for i, v := range variants {
		out[i] = domain.ResolvedVariant{
			ID: v.seed.ID, ImageSourceID: v.seed.ImageSource, AudioSourceID: v.seed.AudioSource,
			Recipe: v.recipe.Clone(),
		}
	}
	return out, nil
}

func validateTruth(t domain.TruthManifest, variants []domain.ResolvedVariant) error {
	if len(t.Variants) != len(variants) {
		return errs.Field(errs.EDecode, "truth.variants", "truth variant count differs from resolved variants")
	}
	byID := t.ByID()
	for i, v := range variants {
		tv, ok := byID[v.ID]
		if !ok {
			return errs.Field(errs.EDecode, "truth.variants", "missing truth for variant "+v.ID)
		}
		if i >= len(t.Variants) || t.Variants[i].ID != v.ID {
			return errs.Field(errs.EDecode, "truth.variants", "truth order must match resolved variant order")
		}
		if tv.Width <= 0 || tv.Height <= 0 || (tv.Channels != 3 && tv.Channels != 4) || tv.AudioSamples < 0 {
			return errs.Field(errs.EDecode, "truth."+v.ID, "invalid truth summary")
		}
		if len(tv.Fingerprint) != 64 {
			return errs.Field(errs.EDecode, "truth."+v.ID+".fingerprint", "fingerprint must be SHA-256")
		}
		if _, err := hex.DecodeString(tv.Fingerprint); err != nil {
			return errs.Field(errs.EDecode, "truth."+v.ID+".fingerprint", "invalid fingerprint")
		}
		fields := append([]string(nil), tv.MetadataFields...)
		sort.Strings(fields)
		if !equalStrings(fields, tv.MetadataFields) {
			return errs.Field(errs.EDecode, "truth."+v.ID+".metadataFields", "metadata fields must be sorted")
		}
	}
	return nil
}

func RecipeVersion(r domain.Recipe) string {
	data, _ := mfxfmt.CanonicalJSON(r.Clone())
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func VariantInputHash(v domain.ResolvedVariant, sources map[string]domain.Source) string {
	img := sources[v.ImageSourceID]
	aud := sources[v.AudioSourceID]
	data := strings.Join([]string{
		img.SHA256, aud.SHA256, v.ImageSourceID, v.AudioSourceID, RecipeVersion(v.Recipe),
	}, "\x00")
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func Fingerprint(p *domain.Project, rp *domain.ResolvedProject) string {
	type canonicalVariant struct {
		ID        string        `json:"id"`
		Image     string        `json:"image"`
		Audio     string        `json:"audio"`
		Recipe    domain.Recipe `json:"recipe"`
		InputHash string        `json:"inputHash"`
	}
	var variants []canonicalVariant
	for _, v := range rp.Variants {
		variants = append(variants, canonicalVariant{
			ID: v.ID, Image: v.ImageSourceID, Audio: v.AudioSourceID, Recipe: v.Recipe, InputHash: v.InputHash,
		})
	}
	sources := make([]domain.Source, 0, len(rp.Sources))
	for _, s := range rp.Sources {
		sources = append(sources, s)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	payload := struct {
		Schema      int                  `json:"schema"`
		ID          string               `json:"id"`
		Sources     []domain.Source      `json:"sources"`
		Variants    []canonicalVariant   `json:"variants"`
		Truth       domain.TruthManifest `json:"truth"`
		Budget      domain.Budget        `json:"budget"`
		Consistency domain.Consistency   `json:"consistency"`
	}{
		Schema: p.SchemaVersion, ID: p.ID, Sources: sources, Variants: variants,
		Truth: p.Truth, Budget: p.Budget, Consistency: p.Consistency,
	}
	data, _ := mfxfmt.CanonicalJSON(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ProjectPath(dir string) string {
	return filepath.Join(dir, "project.json")
}
