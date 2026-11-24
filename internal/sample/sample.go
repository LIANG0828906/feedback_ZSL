package sample

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"texlab/internal/apperr"
	"texlab/internal/gtex"
	"texlab/internal/model"
	"texlab/internal/plan"
	"texlab/internal/truth"
)

type Spec struct {
	Name    string
	Width   int
	Height  int
	Depth   int
	Layout  string
	Seed    uint64
	Patches []PatchSpec
}

type PatchSpec struct {
	Name string
	XF   float64
	YF   float64
	Size int
	RGBA [4]int
}

type Manifest struct {
	Schema           string      `json:"schema"`
	Name             string      `json:"name"`
	Seed             uint64      `json:"seed"`
	Spec             Spec        `json:"spec"`
	Source           gtex.Header `json:"source"`
	PlanFingerprint  string      `json:"plan_fingerprint"`
	TruthFingerprint string      `json:"truth_fingerprint"`
}

type Project struct {
	Dir      string
	Spec     Spec
	Manifest Manifest
	Source   model.Image
	SourceFP string
	Plan     model.Plan
	Truth    truth.Truth
}

var specs = []Spec{
	{
		Name: "aurora_8_rgba", Width: 384, Height: 216, Depth: 8, Layout: "RGBA", Seed: 1101,
		Patches: []PatchSpec{
			{Name: "coral", XF: 0.04, YF: 0.06, Size: 18, RGBA: [4]int{255, 48, 32, 255}},
			{Name: "teal", XF: 0.72, YF: 0.10, Size: 18, RGBA: [4]int{0, 196, 172, 255}},
			{Name: "amber", XF: 0.34, YF: 0.42, Size: 18, RGBA: [4]int{255, 176, 12, 128}},
			{Name: "ghost", XF: 0.58, YF: 0.68, Size: 18, RGBA: [4]int{32, 64, 128, 64}},
		},
	},
	{
		Name: "forge_16_bgra", Width: 1024, Height: 576, Depth: 16, Layout: "BGRA", Seed: 2202,
		Patches: []PatchSpec{
			{Name: "coral", XF: 0.04, YF: 0.06, Size: 26, RGBA: [4]int{65535, 12000, 8000, 65535}},
			{Name: "teal", XF: 0.72, YF: 0.10, Size: 26, RGBA: [4]int{1200, 50000, 45000, 65535}},
			{Name: "amber", XF: 0.34, YF: 0.42, Size: 26, RGBA: [4]int{65535, 42000, 3000, 32768}},
			{Name: "ghost", XF: 0.58, YF: 0.68, Size: 26, RGBA: [4]int{8000, 16000, 32000, 16000}},
		},
	},
	{
		Name: "atlas_8_bgra", Width: 2048, Height: 384, Depth: 8, Layout: "BGRA", Seed: 3303,
		Patches: []PatchSpec{
			{Name: "coral", XF: 0.04, YF: 0.08, Size: 24, RGBA: [4]int{255, 48, 32, 255}},
			{Name: "teal", XF: 0.72, YF: 0.12, Size: 24, RGBA: [4]int{0, 196, 172, 255}},
			{Name: "amber", XF: 0.34, YF: 0.42, Size: 24, RGBA: [4]int{255, 176, 12, 128}},
			{Name: "ghost", XF: 0.58, YF: 0.70, Size: 24, RGBA: [4]int{32, 64, 128, 64}},
		},
	},
}

func Specs() []Spec {
	out := make([]Spec, len(specs))
	copy(out, specs)
	return out
}

func GenerateAll(root string) error {
	for _, spec := range specs {
		if err := Generate(root, spec); err != nil {
			return err
		}
	}
	return nil
}

func Generate(root string, spec Spec) error {
	dir := filepath.Join(root, spec.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "create sample directory", Path: dir, Details: err.Error()}
	}
	source := buildSource(spec)
	sourcePath := filepath.Join(dir, "source.gtex")
	if err := gtex.Write(sourcePath, source); err != nil {
		return err
	}
	sourceFP, err := gtex.FileHash(sourcePath)
	if err != nil {
		return err
	}
	p := buildPlan(spec, source)
	planPath := filepath.Join(dir, "plan.json")
	planRaw, _ := json.MarshalIndent(p, "", "  ")
	if err := os.WriteFile(planPath, append(planRaw, '\n'), 0o644); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "write sample plan", Path: planPath, Details: err.Error()}
	}
	t, err := buildTruth(spec, source, sourceFP, p)
	if err != nil {
		return err
	}
	truthPath := filepath.Join(dir, "truth.json")
	if err := truth.Write(truthPath, t); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "write truth manifest", Path: truthPath, Details: err.Error()}
	}
	header := gtex.HeaderJSON(source, sourceFP)
	m := Manifest{
		Schema: "texlab-sample-v1", Name: spec.Name, Seed: spec.Seed, Spec: spec, Source: header,
		PlanFingerprint: model.PlanFingerprint(p), TruthFingerprint: t.ComputeFingerprint(),
	}
	manifestRaw, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(manifestRaw, '\n'), 0o644); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "write sample manifest", Path: dir, Details: err.Error()}
	}
	return nil
}

func Load(root, name string) (Project, error) {
	dir := filepath.Join(root, name)
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Project{}, &apperr.Error{Code: apperr.EIO, Message: "read sample manifest", Path: dir, Details: err.Error()}
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil || m.Schema != "texlab-sample-v1" {
		return Project{}, apperr.At(apperr.EPlan, 0, "manifest", "invalid sample manifest")
	}
	sourcePath := filepath.Join(dir, "source.gtex")
	source, header, err := gtex.Read(sourcePath)
	if err != nil {
		return Project{}, err
	}
	if header.Width != m.Source.Width || header.Height != m.Source.Height ||
		header.Depth != m.Source.Depth || header.Layout != m.Source.Layout ||
		header.Channels != m.Source.Channels {
		return Project{}, apperr.At(apperr.EPlan, 0, "manifest.source", "source image metadata does not match manifest")
	}
	sourceFP, err := gtex.FileHash(sourcePath)
	if err != nil {
		return Project{}, err
	}
	if sourceFP != m.Source.SHA256 {
		return Project{}, apperr.New(apperr.EDecode, "source file hash does not match manifest")
	}
	p, err := plan.Load(filepath.Join(dir, "plan.json"), source)
	if err != nil {
		return Project{}, err
	}
	t, err := truth.Load(filepath.Join(dir, "truth.json"))
	if err != nil {
		return Project{}, err
	}
	if t.SourceFingerprint != sourceFP || t.PlanFingerprint != model.PlanFingerprint(p) ||
		t.Width != source.Width || t.Height != source.Height || t.SourceDepth != source.Depth || t.SourceLayout != source.Layout {
		return Project{}, apperr.At(apperr.EPlan, 0, "truth", "truth metadata does not match source and plan")
	}
	spec := m.Spec
	if spec.Name == "" {
		spec, _ = findSpec(m.Name)
	}
	return Project{Dir: dir, Spec: spec, Manifest: m, Source: source, SourceFP: sourceFP, Plan: p, Truth: t}, nil
}

func Names(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(root, e.Name(), "manifest.json")); err == nil {
				out = append(out, e.Name())
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func EnsureAll(root string) error {
	names, err := Names(root)
	if err != nil {
		return err
	}
	if len(names) == len(specs) {
		ok := true
		for _, spec := range specs {
			if !contains(names, spec.Name) {
				ok = false
				break
			}
		}
		if ok {
			return nil
		}
	}
	return GenerateAll(root)
}

func contains(v []string, s string) bool {
	for _, x := range v {
		if x == s {
			return true
		}
	}
	return false
}

func buildSource(spec Spec) model.Image {
	im := model.NewImage(spec.Width, spec.Height, spec.Depth, 4, spec.Layout, "straight")
	maxValue := 255
	if spec.Depth == 16 {
		maxValue = 65535
	}
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			r := (x*3 + y + int(spec.Seed)) * 7
			g := (x + y*5 + int(spec.Seed/3)) * 3
			b := (x*2 + y*2 + int(spec.Seed/7)) * 5
			a := maxValue
			if spec.Depth == 8 {
				a = 255
			}
			// Keep the synthetic scene inside a 0..max range while preserving
			// an obvious horizontal gradient and diagonal bands.
			r = (r + x*maxValue/max(1, spec.Width)) & maxValue
			g = (g + y*maxValue/max(1, spec.Height)) & maxValue
			b = (b + (x+y)*maxValue/max(1, spec.Width+spec.Height)) & maxValue
			if ((x/64)+(y/48))%2 == 0 {
				g = (g + maxValue/10) & maxValue
			}
			im.SetSemanticPixel(x, y, [4]uint16{uint16(r), uint16(g), uint16(b), uint16(a)})
		}
	}
	// Deterministic alpha staircase across the bottom segment.
	stepY := spec.Height - spec.Height/12
	for x := 0; x < spec.Width; x++ {
		step := x * maxValue / max(1, spec.Width-1)
		r := maxValue * 35 / 100
		g := maxValue * 55 / 100
		b := maxValue * 80 / 100
		for y := stepY; y < spec.Height; y++ {
			im.SetSemanticPixel(x, y, [4]uint16{uint16(r), uint16(g), uint16(b), uint16(step)})
		}
	}
	for _, p := range spec.Patches {
		x0 := int(p.XF * float64(spec.Width))
		y0 := int(p.YF * float64(spec.Height))
		for y := y0; y < min(spec.Height, y0+p.Size); y++ {
			for x := x0; x < min(spec.Width, x0+p.Size); x++ {
				im.SetSemanticPixel(x, y, [4]uint16{
					uint16(p.RGBA[0]), uint16(p.RGBA[1]), uint16(p.RGBA[2]), uint16(p.RGBA[3]),
				})
			}
		}
	}
	return im
}

func buildPlan(spec Spec, im model.Image) model.Plan {
	w, h := im.Width, im.Height
	regions := []model.Rect{
		{ID: "full", X: 0, Y: 0, W: w, H: h},
		{ID: "left_panel", X: 0, Y: 0, W: w * 2 / 4, H: h},
		{ID: "top_band", X: w / 4, Y: 0, W: w * 2 / 4, H: h / 3},
		{ID: "alpha_steps", X: 0, Y: h * 2 / 3, W: w, H: h - h*2/3},
	}
	entries := []model.Entry{
		{
			Index: 1, ID: "upconvert_swap", Name: "16-bit upconvert + RGBA→BGRA",
			TargetDepth: 16, TargetChannels: 4, Permutation: []int{2, 1, 0, 3},
			Fill: []model.FillSpec{}, Drop: []int{}, Alpha: "straight", Quantize: "round",
			Regions: []string{"full"},
		},
		{
			Index: 2, ID: "premultiply_down", Name: "8-bit downquant + premultiply",
			TargetDepth: 8, TargetChannels: 4, Permutation: []int{0, 1, 2, 3},
			Fill: []model.FillSpec{}, Drop: []int{}, Alpha: "premultiplied", Quantize: "floor",
			Regions: []string{"full"},
		},
		{
			Index: 3, ID: "unpremultiply_up", Name: "16-bit upquant + unpremultiply",
			TargetDepth: 16, TargetChannels: 4, Permutation: []int{2, 1, 0, 3},
			Fill: []model.FillSpec{}, Drop: []int{}, Alpha: "straight", Quantize: "ceil",
			Regions: []string{"full"},
		},
		{
			Index: 4, ID: "regional_swap", Name: "Regional RG → GR swizzle",
			TargetDepth: 16, TargetChannels: 4, Permutation: []int{1, 0, 2, 3},
			Fill: []model.FillSpec{}, Drop: []int{}, Alpha: "straight", Quantize: "round",
			Regions: []string{"top_band", "alpha_steps"},
		},
		{
			Index: 5, ID: "drop_alpha", Name: "Drop alpha (RGBA→BGR)",
			TargetDepth: 16, TargetChannels: 3, Permutation: []int{2, 1, 0},
			Fill: []model.FillSpec{}, Drop: []int{3}, Alpha: "straight", Quantize: "round",
			Regions: []string{"full"},
		},
		{
			Index: 6, ID: "add_alpha", Name: "Add opaque alpha + 8-bit output",
			TargetDepth: 8, TargetChannels: 4, Permutation: []int{2, 1, 0, -1},
			Fill: []model.FillSpec{{TargetIndex: 3, Source: "constant", Channel: "A", Value: 255}},
			Drop: []int{}, Alpha: "straight", Quantize: "round",
			Regions: []string{"full"},
		},
	}
	return model.Plan{
		Schema: "texlab-plan-v1", Name: spec.Name + " conversion plan", Revision: 1,
		Segments: model.SegmentSpec{Cols: 4, Rows: 3}, Regions: regions, Entries: entries,
	}
}

func buildTruth(spec Spec, source model.Image, sourceFP string, p model.Plan) (truth.Truth, error) {
	t := truth.Truth{
		Schema: model.TruthSchema, SourceFingerprint: sourceFP, PlanFingerprint: model.PlanFingerprint(p),
		AlgorithmVersion: model.AlgorithmVersion, Width: source.Width, Height: source.Height,
		SourceDepth: source.Depth, SourceLayout: source.Layout,
	}
	current := source.Clone()
	for _, e := range p.Entries {
		next, err := referenceTransform(current, e, p.EntryRegions(e))
		if err != nil {
			return truth.Truth{}, err
		}
		current = next
		pix, err := truth.EncodePixels(current)
		if err != nil {
			return truth.Truth{}, err
		}
		et := truth.EntryTruth{
			EntryID: e.ID, EntryIndex: e.Index, ContentHash: current.ContentHash(),
			Depth: current.Depth, Channels: current.Channels, Layout: current.Layout, Alpha: current.Alpha,
			Width: current.Width, Height: current.Height, PixelsZ: pix,
		}
		for _, ps := range spec.Patches {
			x := int(ps.XF * float64(source.Width))
			y := int(ps.YF * float64(source.Height))
			rgba := current.SemanticPixel(x, y)
			et.Patches = append(et.Patches, truth.PatchTruth{
				Name: ps.Name, X: x, Y: y, Width: ps.Size, Height: ps.Size, RGBA: rgba,
			})
		}
		stepY := source.Height - source.Height/12 + 2
		for i := 0; i < 16; i++ {
			x := i * (source.Width - 1) / 15
			et.AlphaSteps = append(et.AlphaSteps, truth.AlphaStep{
				X: x, Y: stepY, RGBA: current.SemanticPixel(x, stepY),
			})
		}
		t.Entries = append(t.Entries, et)
	}
	t.FinalHash = current.ContentHash()
	t.Fingerprint = t.ComputeFingerprint()
	return t, nil
}

// BuildTruthForPlan regenerates the independent per-entry reference images for
// an edited plan. It intentionally does not call the main conversion engine.
func BuildTruthForPlan(name string, source model.Image, sourceFP string, p model.Plan) (truth.Truth, error) {
	return BuildTruthWithSpecName(name, source, sourceFP, p)
}

func BuildTruthWithSpecName(name string, source model.Image, sourceFP string, p model.Plan) (truth.Truth, error) {
	spec, ok := findSpec(name)
	if !ok {
		return truth.Truth{}, apperr.New(apperr.EPlan, "unknown sample specification %q", name)
	}
	return buildTruth(spec, source, sourceFP, p)
}

func BuildTruthWithSpec(spec Spec, source model.Image, sourceFP string, p model.Plan) (truth.Truth, error) {
	return buildTruth(spec, source, sourceFP, p)
}

func findSpec(name string) (Spec, bool) {
	for _, s := range specs {
		if s.Name == name {
			return s, true
		}
	}
	return Spec{}, false
}

func referenceTransform(in model.Image, e model.Entry, regions []model.Rect) (model.Image, error) {
	if e.TargetDepth != 8 && e.TargetDepth != 16 {
		return model.Image{}, apperr.At(apperr.EPara, e.Index, "target_depth", "invalid target depth")
	}
	out := in.Clone()
	out.Depth = e.TargetDepth
	out.Channels = e.TargetChannels
	out.Alpha = e.Alpha
	targetMax := 255
	if e.TargetDepth == 16 {
		targetMax = 65535
	}
	fill := map[int]model.FillSpec{}
	for _, f := range e.Fill {
		fill[f.TargetIndex] = f
	}
	labels := make([]string, e.TargetChannels)
	for slot, src := range e.Permutation {
		if src >= 0 {
			labels[slot] = string("RGBA"[src])
		} else {
			labels[slot] = fill[slot].Channel
		}
	}
	out.Layout = strings.Join(labels, "")
	alphaSlot, rSlot, gSlot, bSlot := -1, -1, -1, -1
	for slot, label := range labels {
		switch label {
		case "A":
			alphaSlot = slot
		case "R":
			rSlot = slot
		case "G":
			gSlot = slot
		case "B":
			bSlot = slot
		}
	}
	mask := make([]bool, in.Width*in.Height)
	for _, r := range regions {
		for y := max(0, r.Y); y < min(in.Height, r.Y+r.H); y++ {
			for x := max(0, r.X); x < min(in.Width, r.X+r.W); x++ {
				mask[y*in.Width+x] = true
			}
		}
	}
	for y := 0; y < in.Height; y++ {
		for x := 0; x < in.Width; x++ {
			if !mask[y*in.Width+x] {
				continue
			}
			src := in.SemanticPixel(x, y)
			var slots [4]uint16
			for slot, srcIndex := range e.Permutation {
				if srcIndex >= 0 {
					slots[slot] = refQuant(src[srcIndex], in.Depth, e.TargetDepth, e.Quantize)
				} else {
					f := fill[slot]
					switch f.Source {
					case "constant":
						slots[slot] = uint16(f.Value)
					case "zero":
						slots[slot] = 0
					case "one":
						slots[slot] = uint16(targetMax)
					case "alpha":
						slots[slot] = refQuant(src[3], in.Depth, e.TargetDepth, e.Quantize)
					case "channel":
						slots[slot] = refQuant(src[f.SourceChannel], in.Depth, e.TargetDepth, e.Quantize)
					}
				}
			}
			if e.Alpha != in.Alpha {
				a := uint16(targetMax)
				if alphaSlot >= 0 {
					a = slots[alphaSlot]
				}
				for _, slot := range []int{rSlot, gSlot, bSlot} {
					if slot < 0 {
						continue
					}
					if e.Alpha == "premultiplied" {
						slots[slot] = refPremul(slots[slot], a, targetMax, e.Quantize)
					} else {
						slots[slot] = refUnpremul(slots[slot], a, targetMax, e.Quantize)
					}
				}
			}
			i := (y*in.Width + x) * 4
			for slot := 0; slot < 4; slot++ {
				if slot < e.TargetChannels {
					out.Pixels[i+slot] = slots[slot]
				} else {
					out.Pixels[i+slot] = 0
				}
			}
		}
	}
	return out, nil
}

func refQuant(v uint16, from, to int, mode string) uint16 {
	if from == to {
		return v
	}
	fromMax := 255
	if from == 16 {
		fromMax = 65535
	}
	toMax := 255
	if to == 16 {
		toMax = 65535
	}
	num := int(v) * toMax
	q := num / fromMax
	switch mode {
	case "ceil":
		q = (num + fromMax - 1) / fromMax
	case "round":
		q = (num + fromMax/2) / fromMax
	}
	return uint16(q)
}

func refPremul(c, a uint16, maxValue int, mode string) uint16 {
	if a == 0 {
		return c
	}
	num := int(c) * int(a)
	switch mode {
	case "ceil":
		return uint16((num + maxValue - 1) / maxValue)
	case "round":
		return uint16((num + maxValue/2) / maxValue)
	default:
		return uint16(num / maxValue)
	}
}

func refUnpremul(c, a uint16, maxValue int, mode string) uint16 {
	if a == 0 {
		return c
	}
	num := int(c) * maxValue
	q := num / int(a)
	switch mode {
	case "ceil":
		q = (num + int(a) - 1) / int(a)
	case "round":
		q = (num + int(a)/2) / int(a)
	}
	if q > maxValue {
		q = maxValue
	}
	return uint16(q)
}

func FingerprintBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
