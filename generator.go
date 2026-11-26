package vpf

import "fmt"

type deterministicRNG struct {
	state uint64
}

func newDeterministicRNG(seed int64) *deterministicRNG {
	return &deterministicRNG{state: uint64(seed) + 0x9e3779b97f4a7c15}
}

func (r *deterministicRNG) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *deterministicRNG) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

type SampleBundle struct {
	Seed         int64               `json:"seed"`
	Canonical    *Project            `json:"-"`
	Documents    map[int][]byte      `json:"-"`
	Loaded       map[int]*Project    `json:"-"`
	Truth        *TruthSet           `json:"truth"`
	ExportLosses map[int]*LossReport `json:"-"`
}

// GenerateSample constructs the single deterministic fixture and emits all
// three schema encodings. Truth is calculated from this construction before
// any renderer pixels are produced.
func GenerateSample(seed int64) (*SampleBundle, error) {
	rng := newDeterministicRNG(seed)
	gridVariant := rng.intn(4)

	ground := Layer{
		ID: "ground_base", Name: "Ground / stage floor", Kind: KindGround,
		Rect: Rect{X: 0, Y: 0, W: 96, H: 64}, Visible: true,
		OpacityBP: OpacityScale, Blend: BlendNormal,
		Color:          RGBA{R: 38, G: 72, B: 56, A: 255},
		ContentSummary: fmt.Sprintf("integer-grid-ground-variant-%d", gridVariant),
	}
	ripple := Layer{
		ID: "ripple_mask", Name: "Ripple influence mask", Kind: KindRipple,
		Rect: Rect{X: 0, Y: 0, W: 96, H: 64}, Visible: true,
		OpacityBP: 8000, Blend: BlendAdd,
		Color:          RGBA{R: 80, G: 200, B: 255, A: 204},
		ContentSummary: "two-source-linear-decay-ripple-mask",
		Ripples: []RippleSource{
			{ID: "r_left", Center: Point{X: 28, Y: 28}, Radius: 18, DecayBP: 10000, AmplitudeBP: 9000},
			{ID: "r_right", Center: Point{X: 74, Y: 36}, Radius: 16, DecayBP: 11000, AmplitudeBP: 8200},
		},
	}
	badge := Layer{
		ID: "slate_badge", Name: "Slate badge", Kind: KindSolid,
		Rect: Rect{X: 10, Y: 10, W: 18, H: 10}, Visible: true,
		OpacityBP: 9000, Blend: BlendNormal,
		Color:          RGBA{R: 240, G: 195, B: 65, A: 204},
		ContentSummary: "solid-slate-badge-with-border",
	}
	guide := Layer{
		ID: "guide_overlay", Name: "Hidden composition guides", Kind: KindGuide,
		Rect: Rect{X: 0, Y: 0, W: 96, H: 64}, Visible: true,
		OpacityBP: 5000, Blend: BlendMultiply,
		Color:          RGBA{R: 220, G: 70, B: 220, A: 255},
		ContentSummary: "rule-of-thirds-guide-lines",
	}

	root := Node{
		ID: "production_root", Kind: NodeGroup, Label: "Virtual production foreground stack", Visible: true,
		Children: []Node{
			{
				ID: "scene_group", Kind: NodeGroup, Label: "Scene", Visible: true,
				Children: []Node{
					{ID: ground.ID, Kind: NodeLayer, Label: ground.Name, Visible: true, LayerID: ground.ID},
					{
						ID: "fx_group", Kind: NodeGroup, Label: "Effects", Visible: true,
						Children: []Node{
							{ID: ripple.ID, Kind: NodeLayer, Label: ripple.Name, Visible: true, LayerID: ripple.ID},
						},
					},
				},
			},
			{
				ID: "overlay_group", Kind: NodeGroup, Label: "Overlay", Visible: true,
				Children: []Node{
					{ID: badge.ID, Kind: NodeLayer, Label: badge.Name, Visible: true, LayerID: badge.ID},
				},
			},
			{
				ID: "guides_group", Kind: NodeGroup, Label: "Guides", Visible: false,
				Children: []Node{
					{ID: guide.ID, Kind: NodeLayer, Label: guide.Name, Visible: true, LayerID: guide.ID},
				},
			},
		},
	}

	canonical := &Project{
		ProjectID:            fmt.Sprintf("vpf-deterministic-%d", seed),
		Seed:                 seed,
		SourceVersion:        SchemaV3,
		Width:                96,
		Height:               64,
		Channels:             4,
		TileWidth:            32,
		TileHeight:           32,
		Layers:               []Layer{ground, ripple, badge, guide},
		Root:                 root,
		StackSnapshotVersion: 1,
		RippleSourceVersion:  1,
		Frames: []Frame{
			{Index: 0, AtNS: 0, Label: "cue-a"},
			{Index: 1, AtNS: 20 * MillisecondNanoseconds, Label: "cue-b"},
			{Index: 2, AtNS: 40 * MillisecondNanoseconds, Label: "cue-c"},
		},
		Instructions: []Instruction{
			{Order: 1, AtNS: 0, Op: "set_ripple_radius", LayerID: ripple.ID, SourceID: "r_left", Radius: 18},
			{Order: 2, AtNS: 20 * MillisecondNanoseconds, Op: "set_ripple_radius", LayerID: ripple.ID, SourceID: "r_left", Radius: 22},
			{Order: 3, AtNS: 40 * MillisecondNanoseconds, Op: "set_ripple_radius", LayerID: ripple.ID, SourceID: "r_left", Radius: 26},
		},
	}
	if err := finalizeLoadedProject(canonical); err != nil {
		return nil, err
	}

	truth := buildTruth(canonical)
	bundle := &SampleBundle{
		Seed:         seed,
		Canonical:    canonical,
		Documents:    map[int][]byte{},
		Loaded:       map[int]*Project{},
		Truth:        truth,
		ExportLosses: map[int]*LossReport{},
	}
	for _, version := range []int{SchemaV1, SchemaV2, SchemaV3} {
		data, losses, err := ExportProject(canonical, version)
		if err != nil {
			return nil, fmt.Errorf("export v%d: %w", version, err)
		}
		bundle.Documents[version] = data
		bundle.ExportLosses[version] = losses
		loaded, err := LoadProject(data)
		if err != nil {
			return nil, fmt.Errorf("generated v%d document failed loader: %w", version, err)
		}
		bundle.Loaded[version] = loaded
	}
	return bundle, nil
}

func buildTruth(p *Project) *TruthSet {
	truth := &TruthSet{
		Seed:             p.Seed,
		AlgorithmVersion: AlgorithmVersion,
		Width:            p.Width,
		Height:           p.Height,
		Channels:         p.Channels,
		TileWidth:        p.TileWidth,
		TileHeight:       p.TileHeight,
	}
	rippleLayer, _ := p.LayerByID("ripple_mask")
	for _, frame := range p.Frames {
		frameTruth := FrameTruth{
			FrameIndex:     frame.Index,
			AtNS:           frame.AtNS,
			ActiveLayerIDs: ActiveLayerIDs(p),
			ChunkCount:     p.ChunkCount(),
		}
		sources := EffectiveRipples(p, rippleLayer, frame)
		for _, source := range sources {
			points := []Point{
				source.Center,
				{X: source.Center.X + 8, Y: source.Center.Y},
				{X: source.Center.X, Y: source.Center.Y + source.Radius + 6},
			}
			for _, point := range points {
				distance, weight := RippleWeightBP(source, point.X, point.Y)
				frameTruth.SampleWeights = append(frameTruth.SampleWeights, WeightSampleTruth{
					FrameIndex: frame.Index, AtNS: frame.AtNS, LayerID: rippleLayer.ID,
					SourceID: source.ID, X: point.X, Y: point.Y,
					DistancePX: distance, ExpectedWeightBP: weight,
				})
			}
		}
		truth.Frames = append(truth.Frames, frameTruth)
	}
	return truth
}
