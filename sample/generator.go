package sample

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"texturekernel"
)

const GeneratorVersion = "sample-generator/1"

type Options struct {
	Seed             int64             `json:"seed"`
	Width            int               `json:"width"`
	Height           int               `json:"height"`
	BitDepth         int               `json:"bit_depth"`
	Layout           []texture.Channel `json:"layout"`
	ChunkWidth       int               `json:"chunk_width"`
	ChunkHeight      int               `json:"chunk_height"`
	Workers          int               `json:"workers"`
	ParamVersion     string            `json:"param_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
}

func DefaultOptions(seed int64) Options {
	bitDepth := 8
	if seed%2 != 0 {
		bitDepth = 16
	}
	layout := []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA}
	if (seed/2)%2 != 0 {
		layout = []texture.Channel{texture.ChB, texture.ChG, texture.ChR, texture.ChA}
	}
	return Options{
		Seed: seed, Width: 512, Height: 384,
		BitDepth: bitDepth, Layout: layout,
		ChunkWidth: 128, ChunkHeight: 96, Workers: 4,
		ParamVersion:     texture.DefaultParamVersion,
		AlgorithmVersion: texture.AlgorithmVersion,
	}
}

func (o Options) normalized() (Options, error) {
	if o.Seed == 0 {
		o.Seed = 42
	}
	if o.Width == 0 {
		o.Width = 512
	}
	if o.Height == 0 {
		o.Height = 384
	}
	if o.BitDepth == 0 {
		o.BitDepth = 8
		if o.Seed%2 != 0 {
			o.BitDepth = 16
		}
	}
	if len(o.Layout) == 0 {
		o.Layout = []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA}
		if (o.Seed/2)%2 != 0 {
			o.Layout = []texture.Channel{texture.ChB, texture.ChG, texture.ChR, texture.ChA}
		}
	}
	if o.ChunkWidth == 0 {
		o.ChunkWidth = 128
	}
	if o.ChunkHeight == 0 {
		o.ChunkHeight = 96
	}
	if o.Workers == 0 {
		o.Workers = 4
	}
	if o.ParamVersion == "" {
		o.ParamVersion = texture.DefaultParamVersion
	}
	if o.AlgorithmVersion == "" {
		o.AlgorithmVersion = texture.AlgorithmVersion
	}
	if o.Width < 32 || o.Height < 24 {
		return o, fmt.Errorf("sample must be at least 32x24")
	}
	if o.BitDepth != 8 && o.BitDepth != 16 {
		return o, fmt.Errorf("sample bit depth must be 8 or 16")
	}
	layoutText := texture.LayoutString(o.Layout)
	if layoutText != "RGBA" && layoutText != "BGRA" {
		return o, fmt.Errorf("sample layout must be RGBA or BGRA")
	}
	return o, nil
}

type ChannelValue struct {
	Channel texture.Channel `json:"channel"`
	Value   uint16          `json:"value"`
}

type TruthSample struct {
	Name   string         `json:"name"`
	Region string         `json:"region"`
	X      int            `json:"x"`
	Y      int            `json:"y"`
	Source []ChannelValue `json:"source"`
	Final  []ChannelValue `json:"final"`
}

type Truth struct {
	GeneratorVersion  string                `json:"generator_version"`
	Seed              int64                 `json:"seed"`
	SourceHash        string                `json:"source_hash"`
	PlanFingerprint   string                `json:"plan_fingerprint"`
	ParamVersion      string                `json:"param_version"`
	AlgorithmVersion  string                `json:"algorithm_version"`
	FinalWidth        int                   `json:"final_width"`
	FinalHeight       int                   `json:"final_height"`
	FinalBitDepth     int                   `json:"final_bit_depth"`
	FinalLayout       []texture.Channel     `json:"final_layout"`
	ExpectedFinalHash string                `json:"expected_final_hash"`
	ExpectedFinal     texture.ImageDocument `json:"expected_final"`
	ChunkWidth        int                   `json:"chunk_width"`
	ChunkHeight       int                   `json:"chunk_height"`
	Samples           []TruthSample         `json:"samples"`
	Fingerprint       string                `json:"fingerprint"`
}

func (t *Truth) RecomputeFingerprint() string {
	if t == nil {
		return ""
	}
	payload := struct {
		GeneratorVersion  string                `json:"generator_version"`
		Seed              int64                 `json:"seed"`
		SourceHash        string                `json:"source_hash"`
		PlanFingerprint   string                `json:"plan_fingerprint"`
		ParamVersion      string                `json:"param_version"`
		AlgorithmVersion  string                `json:"algorithm_version"`
		FinalWidth        int                   `json:"final_width"`
		FinalHeight       int                   `json:"final_height"`
		FinalBitDepth     int                   `json:"final_bit_depth"`
		FinalLayout       []texture.Channel     `json:"final_layout"`
		ExpectedFinalHash string                `json:"expected_final_hash"`
		ExpectedFinal     texture.ImageDocument `json:"expected_final"`
		ChunkWidth        int                   `json:"chunk_width"`
		ChunkHeight       int                   `json:"chunk_height"`
		Samples           []TruthSample         `json:"samples"`
	}{
		GeneratorVersion: t.GeneratorVersion, Seed: t.Seed, SourceHash: t.SourceHash,
		PlanFingerprint: t.PlanFingerprint, ParamVersion: t.ParamVersion,
		AlgorithmVersion: t.AlgorithmVersion, FinalWidth: t.FinalWidth,
		FinalHeight: t.FinalHeight, FinalBitDepth: t.FinalBitDepth,
		FinalLayout: t.FinalLayout, ExpectedFinalHash: t.ExpectedFinalHash,
		ExpectedFinal: t.ExpectedFinal, ChunkWidth: t.ChunkWidth, ChunkHeight: t.ChunkHeight,
		Samples: t.Samples,
	}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	t.Fingerprint = hex.EncodeToString(sum[:])
	return t.Fingerprint
}

type Bundle struct {
	Options   Options
	Source    *texture.Image
	Plan      *texture.Plan
	BasePlan  *texture.Plan
	PlanFiles map[string][]byte
	Truth     *Truth
}

type Manifest struct {
	GeneratorVersion string            `json:"generator_version"`
	Seed             int64             `json:"seed"`
	SourceFile       string            `json:"source_file"`
	SourceHash       string            `json:"source_hash"`
	PlanFingerprint  string            `json:"plan_fingerprint"`
	BaseFingerprint  string            `json:"base_plan_fingerprint"`
	PlanFiles        map[string]string `json:"plan_files"`
	TruthFile        string            `json:"truth_file"`
	TruthFingerprint string            `json:"truth_fingerprint"`
	Options          Options           `json:"options"`
}

func Generate(opts Options) (*Bundle, error) {
	normalized, err := opts.normalized()
	if err != nil {
		return nil, err
	}
	source, err := buildSource(normalized)
	if err != nil {
		return nil, err
	}
	plan := buildPlan(normalized, source)
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	base := buildBasePlan(plan)
	if err := base.Validate(); err != nil {
		return nil, err
	}
	planFiles := make(map[string][]byte)
	for _, version := range []string{"1", "2", "3"} {
		data, losses, exportErr := texture.ExportPlan(plan, version)
		if len(losses) > 0 || (exportErr != nil && !texture.HasCode(exportErr, texture.CodeLossy)) {
			return nil, fmt.Errorf("export sample plan to v%s: %w", version, exportErr)
		}
		loaded, err := texture.LoadPlan(data)
		if err != nil {
			return nil, err
		}
		if err := texture.EquivalentForExport(plan, loaded); err != nil {
			return nil, err
		}
		planFiles["plan-v"+version+".json"] = data
	}
	baseData, losses, exportErr := texture.ExportPlan(base, "3")
	if len(losses) > 0 || (exportErr != nil && !texture.HasCode(exportErr, texture.CodeLossy)) {
		return nil, fmt.Errorf("export base plan: %w", exportErr)
	}
	planFiles["plan-base-v3.json"] = baseData

	expected, err := simulateImage(source, plan)
	if err != nil {
		return nil, err
	}
	truth, err := buildTruth(normalized, source, plan, expected)
	if err != nil {
		return nil, err
	}
	bundle := &Bundle{
		Options: normalized, Source: source, Plan: plan, BasePlan: base,
		PlanFiles: planFiles, Truth: truth,
	}
	return bundle, nil
}

func buildSource(opts Options) (*texture.Image, error) {
	im, err := texture.NewImage(opts.Width, opts.Height, opts.BitDepth, opts.Layout)
	if err != nil {
		return nil, err
	}
	for y := 0; y < opts.Height; y++ {
		for x := 0; x < opts.Width; x++ {
			rgba := sourceSemantic(opts, x, y)
			for c, ch := range im.Layout {
				im.Set(x, y, c, rgba[ch])
			}
		}
	}
	if err := im.Validate(); err != nil {
		return nil, err
	}
	return im, nil
}

func sourceSemantic(opts Options, x, y int) map[texture.Channel]uint16 {
	maxValue := maxForBits(opts.BitDepth)
	seedByte := byte(opts.Seed)
	top := opts.Height / 2
	gradientEnd := opts.Height * 3 / 4
	switch {
	case y < top:
		blockW := max(1, opts.Width/4)
		blockH := max(1, top/2)
		col := min(3, x/blockW)
		row := min(1, y/blockH)
		index := row*4 + col
		return map[texture.Channel]uint16{
			texture.ChR: scale8(byte(int(seedByte)+index*41+19), maxValue),
			texture.ChG: scale8(byte(int(seedByte)+index*73+101), maxValue),
			texture.ChB: scale8(byte(int(seedByte)+index*29+211), maxValue),
			texture.ChA: maxValue,
		}
	case y < gradientEnd:
		bandHeight := max(1, gradientEnd-top)
		red := ramp(x, opts.Width, maxValue)
		green := ramp(y-top, bandHeight, maxValue)
		blueRaw := (uint64(x)*37+uint64(y)*17+uint64(seedByte))%uint64(maxValue) + 1
		return map[texture.Channel]uint16{
			texture.ChR: red,
			texture.ChG: green,
			texture.ChB: uint16(blueRaw),
			texture.ChA: maxValue,
		}
	default:
		stepCount := 5
		step := min(stepCount-1, x*stepCount/opts.Width)
		alpha := uint16(uint64(step) * uint64(maxValue) / uint64(stepCount-1))
		return map[texture.Channel]uint16{
			texture.ChR: scale8(byte(int(seedByte)+61), maxValue),
			texture.ChG: scale8(byte(int(seedByte)+127), maxValue),
			texture.ChB: scale8(byte(int(seedByte)+197), maxValue),
			texture.ChA: alpha,
		}
	}
}

func buildPlan(opts Options, source *texture.Image) *texture.Plan {
	targetDepth := 16
	if opts.BitDepth == 16 {
		targetDepth = 8
	}
	targetLayout := []texture.Channel{texture.ChB, texture.ChG, texture.ChR, texture.ChA}
	if texture.LayoutString(opts.Layout) == "BGRA" {
		targetLayout = []texture.Channel{texture.ChR, texture.ChG, texture.ChB, texture.ChA}
	}
	fillValue := uint16(maxForBits(targetDepth) / 3)
	identity := make([]texture.ChannelMapping, len(targetLayout))
	for i, ch := range targetLayout {
		identity[i] = texture.ChannelMapping{Target: ch, Source: ch}
	}
	filled := cloneMappings(identity)
	for i := range filled {
		if filled[i].Target == texture.ChB {
			fill := texture.FillSource{Kind: texture.FillConstant, Value: fillValue}
			filled[i].Source = ""
			filled[i].Fill = &fill
		}
	}
	swapped := cloneMappings(identity)
	for i := range swapped {
		switch swapped[i].Target {
		case texture.ChR:
			swapped[i].Source = texture.ChB
		case texture.ChB:
			swapped[i].Source = texture.ChR
		}
	}
	full := texture.FullRect(opts.Width, opts.Height)
	partial := texture.Rect{
		X: opts.Width / 5, Y: opts.Height / 4,
		Width: max(1, opts.Width*3/5), Height: max(1, opts.Height/2),
	}
	return &texture.Plan{
		Version: "internal",
		Source: texture.Source{
			Width: opts.Width, Height: opts.Height,
			BitDepth: opts.BitDepth, Layout: append([]texture.Channel(nil), opts.Layout...),
		},
		Entries: []texture.Entry{
			{
				Index: 0, Coverage: full, TargetBitDepth: targetDepth,
				TargetChannels: append([]texture.Channel(nil), targetLayout...),
				Mappings:       cloneMappings(identity), Rounding: texture.RoundNearest,
				Alpha: texture.AlphaCopy,
			},
			{
				Index: 1, Coverage: full, TargetBitDepth: targetDepth,
				TargetChannels: append([]texture.Channel(nil), targetLayout...),
				Mappings:       filled, Rounding: texture.RoundNearest,
				Alpha: texture.AlphaCopy,
			},
			{
				Index: 2, Coverage: partial, TargetBitDepth: targetDepth,
				TargetChannels: append([]texture.Channel(nil), targetLayout...),
				Mappings:       swapped, Rounding: texture.RoundNearest,
				Alpha: texture.AlphaCopy,
			},
		},
	}
}

func buildBasePlan(final *texture.Plan) *texture.Plan {
	base := final.Clone()
	base.Version = "internal-base"
	// Edit only entry 2. Entry 0/1 cache entries therefore remain reusable, and
	// only entry 2's actual covered tiles are invalidated.
	identity := make([]texture.ChannelMapping, len(base.Entries[2].TargetChannels))
	for i, ch := range base.Entries[2].TargetChannels {
		identity[i] = texture.ChannelMapping{Target: ch, Source: ch}
	}
	base.Entries[2].Mappings = identity
	base.RecomputeFingerprint()
	return base
}

func cloneMappings(in []texture.ChannelMapping) []texture.ChannelMapping {
	out := make([]texture.ChannelMapping, len(in))
	for i, mapping := range in {
		out[i] = mapping
		if mapping.Fill != nil {
			fill := *mapping.Fill
			out[i].Fill = &fill
		}
	}
	return out
}

func buildTruth(opts Options, source *texture.Image, plan *texture.Plan, expected *texture.Image) (*Truth, error) {
	truth := &Truth{
		GeneratorVersion: GeneratorVersion,
		Seed:             opts.Seed, SourceHash: source.Hash(), PlanFingerprint: plan.Fingerprint,
		ParamVersion: opts.ParamVersion, AlgorithmVersion: opts.AlgorithmVersion,
		FinalWidth: expected.Width, FinalHeight: expected.Height,
		FinalBitDepth: expected.BitDepth, FinalLayout: append([]texture.Channel(nil), expected.Layout...),
		ExpectedFinalHash: expected.Hash(),
		ExpectedFinal:     expected.Document(), ChunkWidth: opts.ChunkWidth, ChunkHeight: opts.ChunkHeight,
	}
	top := opts.Height / 2
	blockW := max(1, opts.Width/4)
	blockH := max(1, top/2)
	for row := 0; row < 2; row++ {
		for col := 0; col < 4; col++ {
			x := min(opts.Width-1, col*blockW+blockW/2)
			y := min(top-1, row*blockH+blockH/2)
			truth.Samples = append(truth.Samples, makeSample(opts, source, expected, fmt.Sprintf("block-r%dc%d-center", row, col), "solid_block", x, y))
		}
	}
	bandTop := top
	bandEnd := opts.Height * 3 / 4
	bandY := min(opts.Height-1, bandTop+(bandEnd-bandTop)/2)
	for _, numerator := range []int{1, 2, 3, 7, 9} {
		x := min(opts.Width-1, opts.Width*numerator/10)
		truth.Samples = append(truth.Samples, makeSample(opts, source, expected, fmt.Sprintf("gradient-x%02d", numerator*10), "gradient_band", x, bandY))
	}
	alphaTop := bandEnd
	alphaHeight := opts.Height - alphaTop
	if alphaHeight < 1 {
		alphaHeight = 1
	}
	y := min(opts.Height-1, alphaTop+alphaHeight/2)
	for step := 0; step < 5; step++ {
		stepWidth := opts.Width / 5
		x := min(opts.Width-1, step*stepWidth+stepWidth/2)
		truth.Samples = append(truth.Samples, makeSample(opts, source, expected, fmt.Sprintf("alpha-step%d", step), "alpha_steps", x, y))
	}
	sort.SliceStable(truth.Samples, func(i, j int) bool {
		if truth.Samples[i].Y != truth.Samples[j].Y {
			return truth.Samples[i].Y < truth.Samples[j].Y
		}
		return truth.Samples[i].X < truth.Samples[j].X
	})
	truth.RecomputeFingerprint()
	return truth, nil
}

func makeSample(opts Options, source, expected *texture.Image, name, region string, x, y int) TruthSample {
	sourceValues := sourceSemantic(opts, x, y)
	sourceChannels := []ChannelValue{
		{Channel: texture.ChR, Value: sourceValues[texture.ChR]},
		{Channel: texture.ChG, Value: sourceValues[texture.ChG]},
		{Channel: texture.ChB, Value: sourceValues[texture.ChB]},
		{Channel: texture.ChA, Value: sourceValues[texture.ChA]},
	}
	var final []ChannelValue
	for c, ch := range expected.Layout {
		final = append(final, ChannelValue{Channel: ch, Value: expected.At(x, y, c)})
	}
	return TruthSample{Name: name, Region: region, X: x, Y: y, Source: sourceChannels, Final: final}
}

// simulateImage is deliberately independent from texture.ApplyEntry. It is an
// analytical oracle used only to construct sample truth.
func simulateImage(source *texture.Image, plan *texture.Plan) (*texture.Image, error) {
	current := source
	for _, entry := range plan.Entries {
		out, err := texture.NewImage(source.Width, source.Height, entry.TargetBitDepth, entry.TargetChannels)
		if err != nil {
			return nil, err
		}
		formatChanges := current.BitDepth != entry.TargetBitDepth ||
			texture.LayoutString(current.Layout) != texture.LayoutString(entry.TargetChannels)
		for y := 0; y < source.Height; y++ {
			for x := 0; x < source.Width; x++ {
				inside := x >= entry.Coverage.X && x < entry.Coverage.Right() &&
					y >= entry.Coverage.Y && y < entry.Coverage.Bottom()
				if !inside {
					if formatChanges {
						return nil, fmt.Errorf("format-changing entry %d has partial coverage", entry.Index)
					}
					for c, ch := range out.Layout {
						out.Set(x, y, c, simChannel(current, x, y, ch))
					}
					continue
				}
				values := map[texture.Channel]uint16{}
				for _, mapping := range entry.Mappings {
					v := uint16(0)
					if mapping.Fill != nil {
						switch mapping.Fill.Kind {
						case texture.FillConstant:
							v = mapping.Fill.Value
						case texture.FillZero:
							v = 0
						case texture.FillOne:
							v = maxForBits(entry.TargetBitDepth)
						case texture.FillFromChannel:
							v = simQuantize(simChannel(current, x, y, mapping.Fill.Channel), current.BitDepth, entry.TargetBitDepth, entry.Rounding)
						}
					} else {
						v = simQuantize(simChannel(current, x, y, mapping.Source), current.BitDepth, entry.TargetBitDepth, entry.Rounding)
					}
					values[mapping.Target] = v
				}
				alpha := maxForBits(entry.TargetBitDepth)
				if v, ok := values[texture.ChA]; ok {
					alpha = v
				}
				switch entry.Alpha {
				case texture.AlphaDrop:
					if _, ok := values[texture.ChA]; ok {
						values[texture.ChA] = alpha
					}
				case texture.AlphaPremultiply:
					for _, ch := range []texture.Channel{texture.ChR, texture.ChG, texture.ChB} {
						if v, ok := values[ch]; ok {
							values[ch] = simScale(v, alpha, maxForBits(entry.TargetBitDepth))
						}
					}
				case texture.AlphaUnpremultiply:
					for _, ch := range []texture.Channel{texture.ChR, texture.ChG, texture.ChB} {
						if v, ok := values[ch]; ok {
							values[ch] = simUnscale(v, alpha, maxForBits(entry.TargetBitDepth))
						}
					}
				}
				for c, ch := range out.Layout {
					out.Set(x, y, c, values[ch])
				}
			}
		}
		current = out
	}
	return current, nil
}

func simChannel(im *texture.Image, x, y int, ch texture.Channel) uint16 {
	if value, ok := im.ChannelAt(x, y, ch); ok {
		return value
	}
	return 0
}

func simQuantize(value uint16, srcBits, dstBits int, mode texture.Rounding) uint16 {
	srcMax := uint32((1 << srcBits) - 1)
	dstMax := uint32((1 << dstBits) - 1)
	if value == 0 {
		return 0
	}
	if uint32(value) == srcMax {
		return uint16(dstMax)
	}
	product := uint64(value) * uint64(dstMax)
	q := product / uint64(srcMax)
	r := product % uint64(srcMax)
	if mode == texture.RoundNearest && r*2 >= uint64(srcMax) {
		q++
	}
	if mode == texture.RoundCeil && r != 0 {
		q++
	}
	if q > uint64(dstMax) {
		q = uint64(dstMax)
	}
	return uint16(q)
}

func simScale(value, alpha, maxValue uint16) uint16 {
	if value == 0 || alpha == 0 {
		return 0
	}
	result := (uint64(value)*uint64(alpha) + uint64(maxValue)/2) / uint64(maxValue)
	if result > uint64(maxValue) {
		result = uint64(maxValue)
	}
	return uint16(result)
}

func simUnscale(value, alpha, maxValue uint16) uint16 {
	if value == 0 || alpha == 0 {
		return 0
	}
	result := (uint64(value)*uint64(maxValue) + uint64(alpha)/2) / uint64(alpha)
	if result > uint64(maxValue) {
		result = uint64(maxValue)
	}
	return uint16(result)
}

func maxForBits(bits int) uint16 {
	if bits >= 16 {
		return 65535
	}
	return uint16((1 << bits) - 1)
}

func scale8(value byte, maxValue uint16) uint16 {
	return uint16((uint32(value)*uint32(maxValue) + 127) / 255)
}

func ramp(value, size int, maxValue uint16) uint16 {
	if size <= 1 {
		return maxValue / 2
	}
	if value >= size {
		value = size - 1
	}
	return uint16(uint64(value) * uint64(maxValue) / uint64(size-1))
}

func WriteBundle(dir string, bundle *Bundle) error {
	if bundle == nil {
		return fmt.Errorf("nil bundle")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := texture.WriteImageFile(filepath.Join(dir, "source.json"), bundle.Source); err != nil {
		return err
	}
	for name, data := range bundle.PlanFiles {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	truthJSON, err := json.MarshalIndent(bundle.Truth, "", "  ")
	if err != nil {
		return err
	}
	truthJSON = append(truthJSON, '\n')
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), truthJSON, 0o644); err != nil {
		return err
	}
	manifest := Manifest{
		GeneratorVersion: GeneratorVersion,
		Seed:             bundle.Options.Seed, SourceFile: "source.json",
		SourceHash: bundle.Source.Hash(), PlanFingerprint: bundle.Plan.Fingerprint,
		BaseFingerprint: bundle.BasePlan.Fingerprint,
		PlanFiles: map[string]string{
			"1": "plan-v1.json", "2": "plan-v2.json", "3": "plan-v3.json",
			"base": "plan-base-v3.json",
		},
		TruthFile: "truth.json", TruthFingerprint: bundle.Truth.Fingerprint,
		Options: bundle.Options,
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestJSON = append(manifestJSON, '\n')
	return os.WriteFile(filepath.Join(dir, "manifest.json"), manifestJSON, 0o644)
}

func LoadTruth(path string) (*Truth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var truth Truth
	if err := json.Unmarshal(data, &truth); err != nil {
		return nil, err
	}
	if truth.RecomputeFingerprint() != truth.Fingerprint {
		return nil, fmt.Errorf("truth fingerprint mismatch")
	}
	return &truth, nil
}
