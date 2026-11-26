package sample

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"maskqc/model"
	"maskqc/raster"
	"maskqc/schema"
	"maskqc/topology"
)

type GeneratedProject struct {
	ID        string
	Canonical *model.Project
	ImagePNG  []byte
	Artifacts map[model.SchemaVersion][]byte
	Reports   map[model.SchemaVersion]*model.ConversionReport
	Lossy     map[model.SchemaVersion][]*model.Error
}

type Bundle struct {
	Seed     int64
	Projects []GeneratedProject
}

type Manifest struct {
	Seed     int64             `json:"seed"`
	Projects []ManifestProject `json:"projects"`
}

type ManifestProject struct {
	ID       string            `json:"id"`
	Width    int               `json:"width"`
	Height   int               `json:"height"`
	Image    string            `json:"image"`
	Files    map[string]string `json:"files"`
	Lossy    map[string]int    `json:"lossy"`
	MaskHash string            `json:"maskHash"`
}

func DefaultSizes() []int {
	return []int{96, 128, 160}
}

func Generate(seed int64, sizes []int) (*Bundle, error) {
	if len(sizes) == 0 {
		sizes = DefaultSizes()
	}
	unique := map[int]bool{}
	cleanSizes := make([]int, 0, len(sizes))
	for _, size := range sizes {
		if size < 48 || size > 1024 {
			return nil, &model.Error{
				Code: model.CodeParam, Path: "sizes", Field: "size",
				Message: fmt.Sprintf("resolution %d is outside 48..1024", size), Allowed: "48..1024",
			}
		}
		if unique[size] {
			continue
		}
		unique[size] = true
		cleanSizes = append(cleanSizes, size)
	}
	sort.Ints(cleanSizes)
	bundle := &Bundle{Seed: seed}
	for _, size := range cleanSizes {
		project, err := generateProject(seed, size)
		if err != nil {
			return nil, err
		}
		pngData, err := renderIcon(project)
		if err != nil {
			return nil, err
		}
		generated := GeneratedProject{
			ID: project.ID, Canonical: project, ImagePNG: pngData,
			Artifacts: map[model.SchemaVersion][]byte{},
			Reports:   map[model.SchemaVersion]*model.ConversionReport{},
			Lossy:     map[model.SchemaVersion][]*model.Error{},
		}
		for _, version := range []model.SchemaVersion{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
			exported, err := schema.Export(project, version)
			if err != nil {
				return nil, fmt.Errorf("export %s %s: %w", project.ID, version, err)
			}
			generated.Artifacts[version] = exported.Artifact
			generated.Reports[version] = exported.Report
			generated.Lossy[version] = exported.Issues
		}
		bundle.Projects = append(bundle.Projects, generated)
	}
	return bundle, nil
}

func Write(root string, bundle *Bundle) (*Manifest, error) {
	if bundle == nil {
		return nil, model.NewError(model.CodeSchema, "bundle", "", "bundle is nil")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	manifest := &Manifest{Seed: bundle.Seed}
	for _, project := range bundle.Projects {
		directory := filepath.Join(root, project.ID)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(directory, "icon.png"), project.ImagePNG, 0o644); err != nil {
			return nil, err
		}
		entry := ManifestProject{
			ID: project.ID, Width: project.Canonical.Width, Height: project.Canonical.Height,
			Image: filepath.ToSlash(filepath.Join(project.ID, "icon.png")),
			Files: map[string]string{}, Lossy: map[string]int{},
			MaskHash: model.HashBytes(project.Canonical.CandidateMask),
		}
		for _, version := range []model.SchemaVersion{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
			fileName := "project." + string(version) + ".json"
			relative := filepath.ToSlash(filepath.Join(project.ID, fileName))
			if err := os.WriteFile(filepath.Join(directory, fileName), project.Artifacts[version], 0o644); err != nil {
				return nil, err
			}
			entry.Files[string(version)] = relative
			entry.Lossy[string(version)] = len(project.Lossy[version])
			reportData, err := json.MarshalIndent(project.Reports[version], "", "  ")
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(directory, "conversion."+string(version)+".json"), append(reportData, '\n'), 0o644); err != nil {
				return nil, err
			}
			if project.Lossy[version] != nil {
				lossyData, err := json.MarshalIndent(project.Lossy[version], "", "  ")
				if err != nil {
					return nil, err
				}
				if err := os.WriteFile(filepath.Join(directory, "lossy."+string(version)+".json"), append(lossyData, '\n'), 0o644); err != nil {
					return nil, err
				}
			}
		}
		manifest.Projects = append(manifest.Projects, entry)
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), append(manifestData, '\n'), 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

func LoadManifest(root string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func generateProject(seed int64, size int) (*model.Project, error) {
	id := fmt.Sprintf("icon-%03d-%d", size, seed)
	rng := rand.New(rand.NewSource(seed ^ int64(size)*7919))
	jitter := func(limit int) int {
		if limit <= 0 {
			return 0
		}
		return rng.Intn(limit*2+1) - limit
	}
	weight := func(base int) int {
		return (base / 10) * 10
	}
	radiusA := max(3, size/24)
	radiusB := max(3, size/28)
	radiusC := max(3, size/30)
	junctionX := size*76/100 + jitter(1)
	junctionY := size*65/100 + jitter(1)
	strokes := []model.Stroke{
		{
			ID: "s01", Seq: 1, X0: size/6 + jitter(2), Y0: size/4 + jitter(2),
			X1: size*7/12 + jitter(2), Y1: size/4 + jitter(1), Radius: radiusA,
			Hardness: model.DefaultHardness, Weight: weight(9200), Priority: 20, Direction: model.DirectionEast,
			GroupID: "g-a", CompositeOrder: 1, SourceVersion: model.SchemaV3,
		},
		{
			ID: "s02", Seq: 2, X0: size/4 + jitter(2), Y0: size/2 + jitter(2),
			X1: size*48/100 + jitter(1), Y1: size*68/100 + jitter(1), Radius: radiusB,
			Hardness: model.DefaultHardness, Weight: weight(8900), Priority: 10, Direction: model.DirectionSouthEast,
			GroupID: "g-b", CompositeOrder: 2, SourceVersion: model.SchemaV3,
		},
		{
			ID: "s03", Seq: 3, X0: junctionX, Y0: size*32/100 + jitter(1),
			X1: junctionX, Y1: junctionY, Radius: radiusC,
			Hardness: model.DefaultHardness, Weight: weight(9400), Priority: 30, Direction: model.DirectionSouth,
			GroupID: "g-c", CompositeOrder: 3, SourceVersion: model.SchemaV3,
		},
		{
			ID: "s04", Seq: 4, X0: junctionX, Y0: junctionY,
			X1: size*56/100 + jitter(1), Y1: junctionY, Radius: radiusC,
			Hardness: model.DefaultHardness, Weight: weight(9400), Priority: 30, Direction: model.DirectionWest,
			GroupID: "g-c", CompositeOrder: 3, SourceVersion: model.SchemaV3,
		},
	}
	blank := make([]byte, size*size)
	composed, err := raster.Compose(blank, size, size, strokes)
	if err != nil {
		return nil, err
	}
	reference := make([]byte, size*size)
	for index, value := range composed.Accumulated {
		if value >= model.ForegroundThreshold {
			reference[index] = 255
		}
	}
	candidate := perturbCandidate(reference, size, seed)
	analysis, err := topology.Analyze(reference, size, size, strokes)
	if err != nil {
		return nil, err
	}
	truth := make([]model.DomainTruth, 0, len(analysis.Domains))
	for _, domain := range analysis.Domains {
		truth = append(truth, model.DomainTruth{
			ID: domain.ID, CentroidX: domain.CentroidX, CentroidY: domain.CentroidY,
			Area: domain.Area, BBox: domain.BBox, Holes: domain.Holes,
			RingWidth: domain.RingWidth, Perimeter: domain.Perimeter,
			ReferenceMask: domain.Mask, Direction: domain.Direction,
			BoundarySegments: domain.Segments,
		})
	}
	groups := []model.StrokeGroup{
		{ID: "g-a", CompositeOrder: 1, StrokeIDs: []string{"s01"}},
		{ID: "g-b", CompositeOrder: 2, StrokeIDs: []string{"s02"}},
		{ID: "g-c", CompositeOrder: 3, StrokeIDs: []string{"s03", "s04"}},
	}
	return &model.Project{
		ID: id, Version: model.SchemaV3, Seed: seed, Width: size, Height: size,
		CandidateMask: candidate, Strokes: strokes, Groups: groups, Truth: truth,
		CompositeOrderHint: 3,
	}, nil
}

func perturbCandidate(reference []byte, size int, seed int64) []byte {
	candidate := append([]byte(nil), reference...)
	hashAt := func(x, y int) uint64 {
		value := uint64(seed) ^ uint64(x*73856093) ^ uint64(y*19349663) ^ uint64(size*83492791)
		value ^= value >> 13
		value *= 0x9e3779b185ebca87
		value ^= value >> 29
		return value
	}
	isForeground := func(x, y int) bool {
		return x >= 0 && x < size && y >= 0 && y < size && reference[y*size+x] >= model.ForegroundThreshold
	}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			index := y*size + x
			if reference[index] >= model.ForegroundThreshold {
				if hashAt(x, y)%7 == 0 {
					for _, delta := range [][2]int{{0, 0}, {1, 0}, {0, 1}} {
						nx, ny := x+delta[0], y+delta[1]
						if isForeground(nx, ny) {
							candidate[ny*size+nx] = 0
						}
					}
				}
				continue
			}
			adjacent := isForeground(x-1, y) || isForeground(x+1, y) || isForeground(x, y-1) || isForeground(x, y+1)
			if adjacent && hashAt(x, y)%17 == 0 {
				candidate[index] = 255
			}
		}
	}
	holes := [][2]int{
		{size / 4, size / 4},
		{size * 9 / 20, size * 11 / 20},
		{size * 76 / 100, size * 48 / 100},
	}
	for holeIndex, point := range holes {
		radius := 1 + holeIndex%2
		for y := point[1] - radius; y <= point[1]+radius; y++ {
			for x := point[0] - radius; x <= point[0]+radius; x++ {
				if x < 0 || x >= size || y < 0 || y >= size {
					continue
				}
				if (x-point[0])*(x-point[0])+(y-point[1])*(y-point[1]) <= radius*radius {
					candidate[y*size+x] = 0
				}
			}
		}
	}
	return candidate
}

func renderIcon(project *model.Project) ([]byte, error) {
	width, height := project.Width, project.Height
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	foregroundMask := make([]byte, width*height)
	for _, truth := range project.Truth {
		for index, value := range truth.ReferenceMask {
			if value >= model.ForegroundThreshold {
				foregroundMask[index] = 255
			}
		}
	}
	isForeground := func(x, y int) bool {
		return x >= 0 && x < width && y >= 0 && y < height && foregroundMask[y*width+x] >= model.ForegroundThreshold
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			index := y*width + x
			t := (x + y) * 255 / max(1, width+height-2)
			bgR := mix(28, 92, t)
			bgG := mix(38, 66, t)
			bgB := mix(62, 54, t)
			noise := int((uint64(project.Seed)*0x9e3779b97f4a7c15 + uint64(index)*0xbf58476d1ce4e5b9) >> 59)
			noise -= 8
			r, g, b, a := bgR, bgG, bgB, 255
			if isForeground(x, y) {
				shade := (x*3 + y*2) % 18
				r, g, b = 232-shade/2, 172-shade/3, 72+shade/2
				a = 255
			} else {
				boundary := false
				for _, delta := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
					if isForeground(x+delta[0], y+delta[1]) {
						boundary = true
						break
					}
				}
				if boundary {
					r, g, b, a = mixColor(232, 172, 72, 255, bgR, bgG, bgB, 45)
					a = 115
				}
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: clampByte(r + noise), G: clampByte(g + noise), B: clampByte(b + noise), A: clampByte(a),
			})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func mix(a, b, t int) int {
	return a + (b-a)*t/255
}

func mixColor(fr, fg, fb, fa, br, bg, bb, alpha int) (int, int, int, int) {
	return (fr*alpha + br*(255-alpha)) / 255,
		(fg*alpha + bg*(255-alpha)) / 255,
		(fb*alpha + bb*(255-alpha)) / 255,
		255
}

func clampByte(value int) uint8 {
	if value < 0 {
		return 0
	}
	if value > 255 {
		return 255
	}
	return uint8(value)
}
