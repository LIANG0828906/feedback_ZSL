package generator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"strata/internal/formats"
	"strata/internal/model"
)

const (
	GridWidth    = 49
	GridHeight   = 37
	OutlierCount = 24
	ChunkCols    = 4
	ChunkRows    = 3
	ChunkSize    = 1.2
	ResampleN    = 13
)

type GeneratedFile struct {
	VersionID  string `json:"versionId"`
	File       string `json:"file"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	PointCount int    `json:"pointCount"`
}

type Manifest struct {
	Seed  int             `json:"seed"`
	Truth model.Truth     `json:"truth"`
	Files []GeneratedFile `json:"files"`
}

type rng struct {
	state uint64
}

func newRNG(seed uint64) *rng {
	return &rng{state: seed ^ 0x9e3779b97f4a7c15}
}

func (r *rng) Next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *rng) Float() float64 {
	return float64(r.Next()>>11) / float64(uint64(1)<<53)
}

func Generate(seed int, outDir string) (*Manifest, error) {
	if seed <= 0 {
		return nil, model.NewError(model.EPARA, "seed must be positive", "seed", 0, map[string]any{"seed": seed})
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "output", 0, nil)
	}
	clean, err := buildCleanGrid()
	if err != nil {
		return nil, err
	}
	source := append([]model.Point(nil), clean...)
	rnd := newRNG(uint64(seed))
	for i := 0; i < OutlierCount; i++ {
		x := -2.4 + rnd.Float()*4.8
		y := -1.8 + rnd.Float()*3.6
		sign := 1.0
		if rnd.Next()&1 == 0 {
			sign = -1
		}
		z := SurfaceHeight(x, y) + sign*5
		r, g, b := PointColor(x, y, z)
		intensity := 0.1 + 0.8*rnd.Float()
		nx, ny, nz := SurfaceNormal(x, y).X, SurfaceNormal(x, y).Y, SurfaceNormal(x, y).Z
		source = append(source, model.Point{
			X: x, Y: y, Z: z, NX: nx, NY: ny, NZ: nz,
			R: r, G: g, B: b, Intensity: intensity,
		})
	}
	// Keep the outlier inserts deterministic but visually distributed.
	outliers := source[len(clean):]
	sort.SliceStable(outliers, func(i, j int) bool {
		if outliers[i].X != outliers[j].X {
			return outliers[i].X < outliers[j].X
		}
		return outliers[i].Y < outliers[j].Y
	})
	model.CanonicalizePoints(source)
	cleanTruth := append([]model.Point(nil), clean...)
	model.CanonicalizePoints(cleanTruth)
	geometry := &model.Geometry{
		Points: source,
		Meta: model.SourceMeta{
			VersionID: model.VersionPLYASCII, VersionNumber: 1,
			DisplayName: "PLY ASCII v1", Format: "PLY ascii 1.0", Encoding: "ascii",
			File: "scene-ascii-v1.ply",
		},
	}
	versions := formats.Versions()
	files := make([]GeneratedFile, 0, len(versions))
	for _, v := range versions {
		path := filepath.Join(outDir, v.File)
		g := *geometry
		g.Meta = model.SourceMeta{
			VersionID: v.ID, VersionNumber: v.Number, DisplayName: v.DisplayName,
			HeaderMeta: model.HeaderMeta{Seed: seed, Generator: "strata/deterministic-sample-generator-v1"},
		}
		if _, err := formats.WriteFile(path, v.ID, &g, false); err != nil {
			return nil, err
		}
		loaded, err := formats.LoadFile(path, v.ID)
		if err != nil {
			return nil, err
		}
		info, _ := os.Stat(path)
		sum, err := fileHash(path)
		if err != nil {
			return nil, err
		}
		files = append(files, GeneratedFile{
			VersionID: v.ID, File: v.File, Bytes: info.Size(), SHA256: sum,
			PointCount: len(loaded.Points),
		})
	}
	cleanBounds := boundsOf(cleanTruth)
	sourceBounds := boundsOf(source)
	features := featureSet(cleanTruth)
	chunks := chunkTruth()
	truth := model.Truth{
		Seed: seed, SeedName: "RIDGE TERRAIN", Surface: "deterministic-ridge-terrain-v1",
		GridWidth: GridWidth, GridHeight: GridHeight,
		SourcePoints: len(source), PipelinePoints: len(clean), Outliers: OutlierCount,
		ExpectedFaces: ChunkCols * ChunkRows * (ResampleN - 1) * (ResampleN - 1) * 2,
		SourceBounds:  sourceBounds, CleanBounds: cleanBounds, MeshBounds: cleanBounds,
		Features: features, Chunks: chunks, FeatureTolerance: 0.006, BoundsExpansion: 0.001,
	}
	manifest := Manifest{Seed: seed, Truth: truth, Files: files}
	truthBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	truthBytes = append(truthBytes, '\n')
	if err := os.WriteFile(filepath.Join(outDir, "truth.json"), truthBytes, 0o644); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "truth.json", 0, nil)
	}
	return &manifest, nil
}

func buildCleanGrid() ([]model.Point, error) {
	points := make([]model.Point, 0, GridWidth*GridHeight)
	for iy := 0; iy < GridHeight; iy++ {
		y := -1.8 + float64(iy)*0.1
		for ix := 0; ix < GridWidth; ix++ {
			x := -2.4 + float64(ix)*0.1
			z := SurfaceHeight(x, y)
			n := SurfaceNormal(x, y)
			r, g, b := PointColor(x, y, z)
			intensity := 0.18 + 0.66*((x+2.4)/4.8) + 0.09*math.Cos(1.4*y)
			points = append(points, model.Point{
				X: x, Y: y, Z: z, NX: n.X, NY: n.Y, NZ: n.Z,
				R: r, G: g, B: b, Intensity: math.Max(0, math.Min(1, intensity)),
			})
		}
	}
	return points, nil
}

func boundsOf(points []model.Point) model.Bounds {
	b := model.EmptyBounds()
	for _, p := range points {
		b.Include(p.Position())
	}
	return b
}

func featureSet(points []model.Point) model.FeatureSet {
	f := model.FeatureSet{}
	if len(points) == 0 {
		return f
	}
	peak, trough := points[0], points[0]
	var minCorner, maxCorner model.Vec3
	for i, p := range points {
		if p.Z > peak.Z {
			peak = p
		}
		if p.Z < trough.Z {
			trough = p
		}
		if i == 0 {
			minCorner = p.Position()
			maxCorner = p.Position()
		}
		if p.X < minCorner.X || (p.X == minCorner.X && p.Y < minCorner.Y) {
			minCorner = p.Position()
		}
		if p.X > maxCorner.X || (p.X == maxCorner.X && p.Y > maxCorner.Y) {
			maxCorner = p.Position()
		}
	}
	ridge := peak
	// The ridge feature is the highest point in the central two-thirds of the
	// terrain, keeping it distinct from a corner maximum on a different seed.
	for _, p := range points {
		if p.X > -1.6 && p.X < 1.6 && p.Y > -1.2 && p.Y < 1.2 && p.Z > ridge.Z {
			ridge = p
		}
	}
	return model.FeatureSet{
		Peak: peak.Position(), Trough: trough.Position(), Ridge: ridge.Position(),
		MinCorner: minCorner, MaxCorner: maxCorner,
	}
}

func chunkTruth() []model.ChunkTruth {
	chunks := make([]model.ChunkTruth, 0, ChunkCols*ChunkRows)
	for row := 0; row < ChunkRows; row++ {
		for col := 0; col < ChunkCols; col++ {
			min := model.Vec3{
				X: -2.4 + float64(col)*ChunkSize,
				Y: -1.8 + float64(row)*ChunkSize,
			}
			max := model.Vec3{X: min.X + ChunkSize, Y: min.Y + ChunkSize}
			chunks = append(chunks, model.ChunkTruth{
				ID: ChunkID(row, col), Row: row, Col: col,
				Bounds:           model.Bounds{Min: min, Max: max},
				ExpectedVertices: ResampleN * ResampleN,
				ExpectedFaces:    (ResampleN - 1) * (ResampleN - 1) * 2,
			})
		}
	}
	return chunks
}

func ChunkID(row, col int) string {
	if row == 0 {
		return fmt.Sprintf("C%02d", col)
	}
	return fmt.Sprintf("C%d%02d", row, col)
}

func fileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func LoadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "truth.json", 0, nil)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "truth.json", 0, nil)
	}
	return &manifest, nil
}
