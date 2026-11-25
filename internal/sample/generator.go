package sample

import (
	"fmt"

	"channelpipe"
)

const DefaultSeed int64 = 0x4348414e // "CHAN"

type SourceImage struct {
	ID string `json:"id"`
	channelpipe.Buffer
}

type SamplePoint struct {
	ImageID string       `json:"image_id"`
	X       int          `json:"x"`
	Y       int          `json:"y"`
	Kind    string       `json:"kind"`
	Stages  []StageTruth `json:"stages"`
}

type StageTruth struct {
	StageIndex             int                   `json:"stage_index"`
	Channels               []channelpipe.Channel `json:"channels"`
	Values                 []uint16              `json:"values"`
	Dropped                []channelpipe.Channel `json:"dropped,omitempty"`
	Tolerance              uint16                `json:"tolerance"`
	QuantizationErrorBound uint64                `json:"quantization_error_bound"`
}

type TruthFile struct {
	Seed        int64         `json:"seed"`
	Description string        `json:"description"`
	Samples     []SamplePoint `json:"samples"`
	Fingerprint string        `json:"fingerprint,omitempty"`
}

type SampleSet struct {
	Seed   int64               `json:"seed"`
	Images []*SourceImage      `json:"images"`
	Set    channelpipe.PlanSet `json:"plan_set"`
	Truth  TruthFile           `json:"truth"`
}

type ImageSpec struct {
	ID     string
	Width  int
	Height int
	Depth  int
	Layout []channelpipe.Channel
}

type Options struct {
	Seed  int64
	Tiny  bool
	Specs []ImageSpec
}

func DefaultOptions() Options {
	return Options{Seed: DefaultSeed}
}

func demoSpecs() []ImageSpec {
	return []ImageSpec{
		{ID: "color-320-rgba8", Width: 320, Height: 240, Depth: 8, Layout: []channelpipe.Channel{channelpipe.ChannelR, channelpipe.ChannelG, channelpipe.ChannelB, channelpipe.ChannelA}},
		{ID: "gradient-1024-bgra16", Width: 1024, Height: 512, Depth: 16, Layout: []channelpipe.Channel{channelpipe.ChannelB, channelpipe.ChannelG, channelpipe.ChannelR, channelpipe.ChannelA}},
		{ID: "alpha-1280-rgba16", Width: 1280, Height: 720, Depth: 16, Layout: []channelpipe.Channel{channelpipe.ChannelR, channelpipe.ChannelG, channelpipe.ChannelB, channelpipe.ChannelA}},
	}
}

func tinySpecs() []ImageSpec {
	return []ImageSpec{
		{ID: "tiny-rgba8", Width: 64, Height: 48, Depth: 8, Layout: []channelpipe.Channel{channelpipe.ChannelR, channelpipe.ChannelG, channelpipe.ChannelB, channelpipe.ChannelA}},
		{ID: "tiny-bgra16", Width: 96, Height: 64, Depth: 16, Layout: []channelpipe.Channel{channelpipe.ChannelB, channelpipe.ChannelG, channelpipe.ChannelR, channelpipe.ChannelA}},
	}
}

func Generate(options Options) (*SampleSet, error) {
	if options.Seed == 0 {
		options.Seed = DefaultSeed
	}
	specs := options.Specs
	if specs == nil {
		if options.Tiny {
			specs = tinySpecs()
		} else {
			specs = demoSpecs()
		}
	}

	result := &SampleSet{Seed: options.Seed, Truth: TruthFile{Seed: options.Seed, Description: "deterministic color blocks, gradient bands and alpha steps"}}
	result.Set = channelpipe.PlanSet{ID: "art-channel-plan-set", Seed: options.Seed}

	for _, spec := range specs {
		image := paintImage(spec, options.Seed)
		plan := buildPlan(spec, options.Seed)
		points := sampleCoordinates(spec)
		if err := attachTruth(&result.Truth, image, plan, points); err != nil {
			return nil, err
		}
		result.Images = append(result.Images, image)
		result.Set.Plans = append(result.Set.Plans, plan)
	}
	if err := channelpipe.ValidateSet(&result.Set); err != nil {
		return nil, err
	}
	fingerprint, err := truthFingerprint(result.Truth)
	if err != nil {
		return nil, err
	}
	result.Truth.Fingerprint = fingerprint
	return result, nil
}

func buildPlan(spec ImageSpec, seed int64) channelpipe.Plan {
	normalize := channelpipe.Stage{
		ID:             "normalize-rgba16",
		TargetDepth:    16,
		Rounding:       channelpipe.RoundNearest,
		TargetChannels: []channelpipe.Channel{channelpipe.ChannelR, channelpipe.ChannelG, channelpipe.ChannelB, channelpipe.ChannelA},
		Swizzle:        semanticSwizzle(spec.Layout, []channelpipe.Channel{channelpipe.ChannelR, channelpipe.ChannelG, channelpipe.ChannelB, channelpipe.ChannelA}),
		Fills:          make([]channelpipe.FillSpec, 4),
		DropPolicy:     channelpipe.DropRecord,
		AlphaMode:      channelpipe.AlphaPassthrough,
		Metadata:       map[string]string{"generator.zone": "known-block-gradient-alpha"},
	}
	dropAlpha := channelpipe.Stage{
		ID:             "drop-alpha-bgr8",
		TargetDepth:    8,
		Rounding:       channelpipe.RoundNearest,
		TargetChannels: []channelpipe.Channel{channelpipe.ChannelB, channelpipe.ChannelG, channelpipe.ChannelR},
		Swizzle:        []int{2, 1, 0},
		Fills:          make([]channelpipe.FillSpec, 3),
		DropPolicy:     channelpipe.DropRecord,
		AlphaMode:      channelpipe.AlphaDiscard,
	}
	restoreAlpha := channelpipe.Stage{
		ID:             "restore-alpha-rgba16",
		TargetDepth:    16,
		Rounding:       channelpipe.RoundNearest,
		TargetChannels: []channelpipe.Channel{channelpipe.ChannelR, channelpipe.ChannelG, channelpipe.ChannelB, channelpipe.ChannelA},
		Swizzle:        []int{2, 1, 0, -1},
		Fills: []channelpipe.FillSpec{
			{}, {}, {}, {Kind: channelpipe.FillOpaque},
		},
		DropPolicy: channelpipe.DropRecord,
		AlphaMode:  channelpipe.AlphaOpaque,
	}
	return channelpipe.Plan{
		ID:           spec.ID,
		Seed:         seed,
		Width:        spec.Width,
		Height:       spec.Height,
		SourceDepth:  spec.Depth,
		SourceLayout: append([]channelpipe.Channel(nil), spec.Layout...),
		Stages:       []channelpipe.Stage{normalize, dropAlpha, restoreAlpha},
	}
}

func semanticSwizzle(input, target []channelpipe.Channel) []int {
	result := make([]int, len(target))
	for i, wanted := range target {
		result[i] = -1
		for j, actual := range input {
			if actual == wanted {
				result[i] = j
				break
			}
		}
	}
	return result
}

func sampleCoordinates(spec ImageSpec) []struct {
	X, Y int
	Kind string
} {
	var points []struct {
		X, Y int
		Kind string
	}
	blockY := spec.Height / 8
	for block := 0; block < 4; block++ {
		x := (block*2 + 1) * spec.Width / 8
		points = append(points, struct {
			X, Y int
			Kind string
		}{x, blockY, fmt.Sprintf("color-block-%d", block)})
	}
	gradientY := spec.Height / 2
	for _, numerator := range []int{1, 5, 9} {
		x := numerator * spec.Width / 10
		points = append(points, struct {
			X, Y int
			Kind string
		}{x, gradientY, "gradient"})
	}
	alphaY := spec.Height * 7 / 8
	for step := 0; step < 5; step++ {
		x := (step*2 + 1) * spec.Width / 10
		points = append(points, struct {
			X, Y int
			Kind string
		}{x, alphaY, fmt.Sprintf("alpha-step-%d", step)})
	}
	return points
}
